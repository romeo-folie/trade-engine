package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// Broker-resident bracket exits (#435). The execution study measured the
// stock path's hourly candle-close stop checks costing ~1.5%/trade: 49 of 53
// stop breaches happened intraday where a broker-resident stop would have
// filled at the level, and 11 trades touched their take-profit intrabar yet
// were recorded as losses. With risk.bracket_exits_enabled, BUY entries are
// submitted as Alpaca bracket orders (GTC, whole shares) whose TP/SL legs
// live at the broker; monitorPositions then only reconciles leg fills into
// the trades table instead of running its own price-compare exits.
//
// Scope note: OCO-linked bracket CHILDREN (parent_order_id set, migration 044)
// are always broker-managed. A standalone trailing stop is broker-managed only
// when the trade has no software take-profit; otherwise the software monitor
// must retain ownership of that TP (Codex P2 on #435). The no-TP case covers
// uncapped ATR/EMA trades and prevents the monitor from double-selling after
// their broker-resident hard stop fills.
//
// Degradation is graceful in both directions: a bracket entry whose broker
// returns no child legs (e.g. the no-API-key PaperExecutor) simply stays
// app-managed, and a bracket trade whose legs disappear (operator cancel at
// the broker) reverts to app-managed monitoring on the next sweep.

// legState pairs a leg row with its live broker status for close bookkeeping.
type legState struct {
	leg store.Order
	res execution.OrderResult
	ok  bool // res is populated (status call succeeded)
}

// bracketChildren returns only the OCO-linked bracket legs from a working
// exit-leg set.
func bracketChildren(legs []store.Order) []store.Order {
	out := legs[:0:0]
	for _, l := range legs {
		if l.ParentOrderID != "" {
			out = append(out, l)
		}
	}
	return out
}

// brokerManagedExitLegs returns the broker orders that own all exits for this
// trade. Bracket children always qualify. A standalone stop qualifies only
// when no software take-profit remains; in that case there is no app-managed
// winning exit for monitorPositions to preserve.
func brokerManagedExitLegs(t store.Trade, legs []store.Order) []store.Order {
	if children := bracketChildren(legs); len(children) > 0 {
		return children
	}
	if t.TakeProfit > 0 {
		return nil
	}
	return standaloneStopLegs(legs)
}

func standaloneStopLegs(legs []store.Order) []store.Order {
	out := legs[:0:0]
	for _, leg := range legs {
		if leg.ParentOrderID == "" && leg.IsStopLoss {
			out = append(out, leg)
		}
	}
	return out
}

// reconcileBracketTrade books broker-side exit fills into the trades table and
// reports whether the trade's exits are broker-resident this tick. A true
// result means monitorPositions must not run its own price-compare exit: the
// position is protected by working legs, was closed here, or has unknown broker
// state. A false result means app-managed monitoring applies. Standalone stops
// on trades with a software TP deliberately do not count.
func (e *Engine) reconcileBracketTrade(ctx context.Context, t store.Trade) bool {
	return e.reconcileBrokerExitTrade(ctx, t, false)
}

// reconcileBrokerExitTrade optionally polls standalone protective stops on
// capped trades. Those stops must be checked for offline fills during startup,
// but a still-working stop does not own the software take-profit and therefore
// must not suppress app-managed monitoring.
func (e *Engine) reconcileBrokerExitTrade(ctx context.Context, t store.Trade, pollCappedStandalone bool) bool {
	handled, _ := e.reconcileBrokerExitTradeStatus(ctx, t, pollCappedStandalone)
	return handled
}

func (e *Engine) reconcileBrokerExitTradeStatus(
	ctx context.Context,
	t store.Trade,
	pollCappedStandalone bool,
) (bool, error) {
	all, err := store.OpenExitLegsForTrade(ctx, t.ID)
	if err != nil {
		// Unknown state: legs may be live at the broker. A software exit on
		// top of a live leg double-sells, so skip this tick and retry.
		logger.Warn("bracket exits: leg lookup failed — skipping software exit this tick",
			"trade_id", t.ID, "ticker", t.Ticker, "err", err)
		return true, err
	}
	legs := brokerManagedExitLegs(t, all)
	ownsAllExits := len(legs) > 0
	if len(legs) == 0 && pollCappedStandalone && t.TakeProfit > 0 {
		legs = standaloneStopLegs(all)
	}
	if len(legs) == 0 {
		return false, nil
	}

	states := make([]legState, 0, len(legs))
	working := 0
	var filledIdx = -1
	var pollErr error
	for _, leg := range legs {
		res, err := e.executor.GetOrderStatus(ctx, leg.BrokerOrderID)
		if err != nil {
			if errors.Is(err, execution.ErrOrderNotFound) {
				// Purged from the broker's lookup — terminal and gone.
				// A durable partial must remain sweepable until its quantity and
				// terminal order state can be committed atomically below.
				if leg.FilledQty <= 0 {
					matches, matchErr := store.OrderBrokerIDMatches(ctx, leg.ID, leg.BrokerOrderID)
					if matchErr != nil {
						return true, matchErr
					}
					if !matches {
						// A replacement may still be live at the broker. Poll its fresh
						// ID on the next sweep without suppressing a capped software TP.
						states = append(states, legState{leg: leg})
						working++
						continue
					}
					_, ephemeralPaper := e.executor.(*execution.PaperExecutor)
					if !ephemeralPaper {
						held, proofErr := e.brokerStillHoldsOwnedTickerQuantity(ctx, t.Ticker)
						if proofErr != nil || !held {
							if proofErr != nil {
								return true, fmt.Errorf("purged protective leg %s has no durable fill and broker holdings proof failed: %w",
									leg.BrokerOrderID, proofErr)
							}
							return true, fmt.Errorf("purged protective leg %s has no durable fill and broker holdings do not prove cancellation",
								leg.BrokerOrderID)
						}
					}
					uErr := store.UpdateOrderStatusForBrokerOrder(ctx, leg.ID, leg.BrokerOrderID, "cancelled")
					if errors.Is(uErr, store.ErrOrderBrokerIDChanged) {
						// The row now points at a replacement which may still be live.
						// Retain broker ownership until a fresh snapshot polls that ID.
						states = append(states, legState{leg: leg})
						working++
						continue
					}
					if uErr != nil {
						logger.Warn("bracket exits: mark purged leg", "order_id", leg.ID, "err", uErr)
						pollErr = errors.Join(pollErr, uErr)
					}
				}
				res := execution.OrderResult{
					Status: "cancelled", FilledPrice: leg.FilledPrice, FilledQty: leg.FilledQty,
				}
				if leg.FilledAt != nil {
					res.FilledAt = *leg.FilledAt
				}
				states = append(states, legState{leg: leg, res: res, ok: true})
				continue
			}
			// Transient broker error: assume the leg is still working.
			logger.Warn("bracket exits: leg status check failed",
				"trade_id", t.ID, "broker_order_id", leg.BrokerOrderID, "err", err)
			pollErr = errors.Join(pollErr, err)
			states = append(states, legState{leg: leg})
			working++
			continue
		}
		states = append(states, legState{leg: leg, res: res, ok: true})
		switch execution.NormalizeOrderStatus(res.Status) {
		case "filled":
			filledIdx = len(states) - 1
		case "cancelled", "rejected":
			// Persist only after the corresponding trade-quantity transition
			// below. ApplyPartialExit commits both writes atomically.
		default: // pending | partial — still working at the broker
			working++
		}
	}

	if filledIdx >= 0 {
		handled, closeErr := e.closeBracketTrade(ctx, t, states, filledIdx, ownsAllExits)
		return handled, errors.Join(pollErr, closeErr)
	}
	if working > 0 {
		// A working order may already have sold part of the position. Apply
		// its cumulative fill now so startup broker/DB comparison sees the
		// true remaining quantity rather than waiting for terminal status.
		remaining := t.Quantity
		latestIdx := -1
		var latestFillAt time.Time
		for i, s := range states {
			if !s.ok || s.res.FilledQty <= 0 {
				continue
			}
			var uErr error
			remaining, _, uErr = store.ApplyPartialExitAtForBrokerOrder(
				ctx, t.ID, s.leg.ID, s.leg.BrokerOrderID,
				fillPriceOf(s), s.res.FilledQty,
				execution.NormalizeOrderStatus(s.res.Status), s.res.FilledAt,
			)
			if uErr != nil {
				if errors.Is(uErr, store.ErrOrderBrokerIDChanged) {
					logger.Info("bracket exits: ignored stale fill from replaced broker stop",
						"trade_id", t.ID, "order_id", s.leg.ID,
						"broker_order_id", s.leg.BrokerOrderID)
					return true, nil
				}
				logger.Warn("bracket exits: apply live partial leg exit failed — retrying next sweep",
					"trade_id", t.ID, "order_id", s.leg.ID, "err", uErr)
				return true, uErr
			}
			if latestIdx < 0 || s.res.FilledAt.After(latestFillAt) {
				latestIdx, latestFillAt = i, s.res.FilledAt
			}
		}
		if remaining <= 1e-9 && latestIdx >= 0 {
			// Some broker snapshots expose the full cumulative quantity before
			// advancing status from partial to filled. Confirm every still-live
			// leg is terminal before finalizing, or an OCO sibling could later
			// over-sell the now-flat position.
			for i := range states {
				status := execution.NormalizeOrderStatus(states[i].res.Status)
				if states[i].ok && (status == "filled" || status == "cancelled" || status == "rejected") {
					continue
				}
				cancelErr := e.executor.CancelOrder(ctx, states[i].leg.BrokerOrderID)
				confirmed, statusErr := e.executor.GetOrderStatus(ctx, states[i].leg.BrokerOrderID)
				if errors.Is(statusErr, execution.ErrOrderNotFound) {
					confirmed = execution.OrderResult{Status: "cancelled"}
					statusErr = nil
				}
				if statusErr != nil {
					return true, errors.Join(cancelErr, statusErr)
				}
				confirmedStatus := execution.NormalizeOrderStatus(confirmed.Status)
				if confirmedStatus != "filled" && confirmedStatus != "cancelled" && confirmedStatus != "rejected" {
					return true, fmt.Errorf("full cumulative bracket fill left leg %s %s after cancel: %w",
						states[i].leg.BrokerOrderID, confirmedStatus, cancelErr)
				}
				if confirmed.FilledQty < states[i].res.FilledQty {
					confirmed.FilledQty = states[i].res.FilledQty
					confirmed.FilledPrice = states[i].res.FilledPrice
					confirmed.FilledAt = states[i].res.FilledAt
				}
				states[i].res, states[i].ok = confirmed, true
			}
			handled, closeErr := e.closeBracketTrade(ctx, t, states, latestIdx, ownsAllExits)
			return handled, errors.Join(pollErr, closeErr)
		}
		return ownsAllExits, pollErr
	}
	if working == 0 {
		// All legs are terminal without a FULL fill while the trade is
		// still open (operator cancelled at the broker, or rejection). Any
		// partial fills already SOLD shares — account for them before
		// handing back to app-managed monitoring, or the software exit
		// would later market-sell the stale full quantity (over-sell or
		// broker-rejected order) and compute pnl on shares no longer held.
		var partialQty float64
		for _, s := range states {
			if s.ok && s.res.FilledQty > 0 {
				partialQty += s.res.FilledQty
			}
		}
		if partialQty > 0 {
			remaining := t.Quantity
			var filledAt time.Time
			latestIdx := -1
			for _, s := range states {
				if !s.ok || s.res.FilledQty <= 0 {
					continue
				}
				var uErr error
				remaining, _, uErr = store.ApplyPartialExitAtForBrokerOrder(
					ctx, t.ID, s.leg.ID, s.leg.BrokerOrderID,
					fillPriceOf(s), s.res.FilledQty,
					execution.NormalizeOrderStatus(s.res.Status), s.res.FilledAt,
				)
				if uErr != nil {
					if errors.Is(uErr, store.ErrOrderBrokerIDChanged) {
						// A replacement may still be live at the broker. Keep exit
						// ownership and poll the fresh ID on the next snapshot.
						return true, nil
					}
					logger.Warn("bracket exits: atomically apply partial leg exit failed — retrying next sweep",
						"trade_id", t.ID, "order_id", s.leg.ID, "err", uErr)
					return true, uErr // order remains sweepable; quantity is unchanged
				}
			}
			for i, s := range states {
				if s.ok && s.res.FilledQty > 0 && (latestIdx < 0 || s.res.FilledAt.After(filledAt)) {
					filledAt, latestIdx = s.res.FilledAt, i
				}
			}
			if remaining <= 1e-9 {
				// Decide completion from the durable post-application remainder,
				// not by comparing cumulative broker fills with t.Quantity. The
				// latter is already reduced when an earlier sweep persisted a
				// working partial (for example, 6 filled leaves t.Quantity=4).
				handled, closeErr := e.closeBracketTrade(
					ctx, t, states, latestIdx, ownsAllExits,
				)
				return handled, errors.Join(pollErr, closeErr)
			}
			for _, s := range states {
				if s.ok && s.res.FilledQty == 0 {
					if uErr := store.UpdateOrderFillForBrokerOrder(
						ctx, s.leg.ID, s.leg.BrokerOrderID, s.res.FilledPrice, 0,
						execution.NormalizeOrderStatus(s.res.Status),
					); uErr != nil {
						if errors.Is(uErr, store.ErrOrderBrokerIDChanged) {
							return true, nil
						}
						logger.Warn("bracket exits: terminalize zero-fill leg",
							"order_id", s.leg.ID, "err", uErr)
						return true, uErr
					}
				}
			}
			// ApplyPartialExit also advances the trade's durable realized
			// totals, so the eventual close includes this slice in weighted
			// exit price, P&L, and return.
			logger.Warn("bracket exits: legs died after a partial exit — tracked quantity reduced",
				"trade_id", t.ID, "ticker", t.Ticker,
				"sold_qty", partialQty, "remaining_qty", remaining)
		} else {
			for _, s := range states {
				if !s.ok {
					continue
				}
				if uErr := store.UpdateOrderFillForBrokerOrder(
					ctx, s.leg.ID, s.leg.BrokerOrderID, s.res.FilledPrice, 0,
					execution.NormalizeOrderStatus(s.res.Status),
				); uErr != nil {
					if errors.Is(uErr, store.ErrOrderBrokerIDChanged) {
						return true, nil
					}
					logger.Warn("bracket exits: terminalize zero-fill leg",
						"order_id", s.leg.ID, "err", uErr)
					return true, uErr
				}
			}
		}
		// Position (or its remainder) is now UNPROTECTED — hand it back to
		// app-managed monitoring, which resumes on this very sweep.
		logger.Warn("bracket exits: all legs gone with trade still open — reverting to app-managed exits",
			"trade_id", t.ID, "ticker", t.Ticker)
		return false, pollErr
	}
	return ownsAllExits, pollErr
}

func (e *Engine) brokerStillHoldsOwnedTickerQuantity(ctx context.Context, ticker string) (bool, error) {
	positions, err := e.executor.GetPositions(ctx)
	if err != nil {
		return false, err
	}
	owned, err := store.GetOwnedPositionTrades(ctx)
	if err != nil {
		return false, err
	}
	var brokerQty, dbQty float64
	for _, p := range positions {
		if p.Ticker != ticker {
			continue
		}
		qty := p.Quantity
		if p.Side == "short" && qty > 0 {
			qty = -qty
		}
		brokerQty += qty
	}
	for _, trade := range owned {
		if trade.Ticker != ticker {
			continue
		}
		qty := trade.Quantity
		if trade.Direction == "short" {
			qty = -qty
		}
		dbQty += qty
	}
	const tolerance = 0.01
	return dbQty != 0 && brokerQty*dbQty > 0 && math.Abs(brokerQty-dbQty) <= tolerance, nil
}

// fillPriceOf resolves a leg's fill price, falling back to the leg's own
// level when the broker reported a fill without a price yet (the 10-min
// exit-fill reconciler heals rows to broker truth afterwards).
func fillPriceOf(s legState) float64 {
	if s.ok && s.res.FilledPrice > 0 {
		return s.res.FilledPrice
	}
	if s.leg.IsTakeProfit {
		return s.leg.LimitPrice
	}
	return s.leg.StopPrice
}

// closeBracketTrade books the exit when a bracket leg fills. The exit price
// and quantity are the fill-weighted aggregate across ALL legs that sold
// shares — a take-profit that partially filled before the ratcheted stop
// took out the remainder contributes its real proceeds instead of being
// discarded as a zero-fill cancel (Codex P2 on #435).
func (e *Engine) closeBracketTrade(
	ctx context.Context,
	t store.Trade,
	states []legState,
	filledIdx int,
	fallbackHandled bool,
) (bool, error) {
	filled := states[filledIdx]
	exitReason := "stop-loss hit"
	if filled.leg.IsTakeProfit {
		exitReason = "take-profit hit"
	}
	intent := bracketExitIntent(filled.leg)
	claimed, err := store.ClaimTradeExitWithIntent(ctx, t.ID, intent)
	if err != nil {
		logger.Warn("bracket exits: claim trade for broker fill", "trade_id", t.ID, "err", err)
		return true, err
	}
	if !claimed {
		// A software exit may have claimed while this broker poll was in
		// flight. The protective fill still won the broker cancellation race;
		// apply it under that durable claim instead of discarding it.
		logger.Info("bracket exits: protective fill joined an existing exit claim",
			"trade_id", t.ID, "broker_order_id", filled.leg.BrokerOrderID)
	}

	remaining := t.Quantity
	fillPersisted := false
	applyOrder := make([]int, 0, len(states))
	applyOrder = append(applyOrder, filledIdx)
	for i := range states {
		if i != filledIdx {
			applyOrder = append(applyOrder, i)
		}
	}
	// Validate and apply the winning leg first. Besides failing fast on a stale
	// broker ID, this commits the protective intent in the same transaction as
	// the fill that can make the trade finalizable.
	for _, i := range applyOrder {
		s := states[i]
		if !s.ok {
			continue
		}
		status := execution.NormalizeOrderStatus(s.res.Status)
		cumulativeQty := s.res.FilledQty
		if i == filledIdx && cumulativeQty <= 0 {
			cumulativeQty = s.leg.Quantity
			if cumulativeQty <= 0 {
				cumulativeQty = t.Quantity
			}
		}
		if cumulativeQty > 0 {
			var applyErr error
			if claimed {
				remaining, _, applyErr = store.ApplyPartialExitAtForBrokerOrder(
					ctx, t.ID, s.leg.ID, s.leg.BrokerOrderID,
					fillPriceOf(s), cumulativeQty, status, s.res.FilledAt,
				)
			} else {
				remaining, _, applyErr = store.ApplyProtectiveExitAtForBrokerOrder(
					ctx, t.ID, s.leg.ID, s.leg.BrokerOrderID,
					fillPriceOf(s), cumulativeQty, status, s.res.FilledAt, intent,
				)
			}
			if applyErr != nil {
				if errors.Is(applyErr, store.ErrOrderBrokerIDChanged) {
					if claimed && !fillPersisted {
						_ = store.ReleaseTradeExitClaim(ctx, t.ID)
					}
					logger.Info("bracket exits: ignored stale terminal fill from replaced broker stop",
						"trade_id", t.ID, "order_id", s.leg.ID,
						"broker_order_id", s.leg.BrokerOrderID)
					return fallbackHandled, nil
				}
				if claimed && !fillPersisted {
					_ = store.ReleaseTradeExitClaim(ctx, t.ID)
				}
				logger.Warn("bracket exits: apply terminal broker fill", "trade_id", t.ID, "order_id", s.leg.ID, "err", applyErr)
				return true, applyErr
			}
			fillPersisted = true
			continue
		}
		if status == "cancelled" || status == "rejected" {
			if updateErr := store.UpdateOrderFillAtForBrokerOrder(
				ctx, s.leg.ID, s.leg.BrokerOrderID,
				s.res.FilledPrice, 0, status, s.res.FilledAt,
			); updateErr != nil {
				if errors.Is(updateErr, store.ErrOrderBrokerIDChanged) {
					if claimed && !fillPersisted {
						_ = store.ReleaseTradeExitClaim(ctx, t.ID)
					}
					return fallbackHandled, nil
				}
				if claimed && !fillPersisted {
					_ = store.ReleaseTradeExitClaim(ctx, t.ID)
				}
				logger.Warn("bracket exits: terminalize zero-fill sibling", "order_id", s.leg.ID, "err", updateErr)
				return true, updateErr
			}
		}
	}
	if remaining > 1e-9 {
		if claimed && !fillPersisted {
			_ = store.ReleaseTradeExitClaim(ctx, t.ID)
		}
		logger.Warn("bracket exits: terminal fills did not cover the tracked position",
			"trade_id", t.ID, "ticker", t.Ticker, "remaining_qty", remaining)
		return true, fmt.Errorf("terminal broker fills left %.8f shares unreconciled", remaining)
	}
	realized, err := store.FinalizeTradeExit(ctx, t.ID)
	if err != nil {
		logger.Warn("bracket exits: finalize broker-filled trade", "trade_id", t.ID, "err", err)
		return true, err
	}
	e.recordBracketClose(t, realized, exitReason)
	return true, nil
}

func bracketExitIntent(leg store.Order) store.ExitIntent {
	reasonCode := leg.ExitReason
	evidence := make(map[string]any)
	if len(leg.ExitEvidence) > 0 {
		_ = json.Unmarshal(leg.ExitEvidence, &evidence)
	}
	if evidence == nil {
		evidence = make(map[string]any)
	}
	trigger := leg.StopPrice
	if reasonCode == "" {
		reasonCode = store.ExitReasonStopLoss
		if leg.IsTakeProfit {
			reasonCode = store.ExitReasonTakeProfit
		}
	}
	if leg.IsTakeProfit {
		trigger = leg.LimitPrice
	}
	evidence["broker_order_id"] = leg.BrokerOrderID
	evidence["trigger_price"] = trigger
	evidence["order_type"] = leg.OrderType
	return newExitIntent(reasonCode, evidence)
}

func (e *Engine) recordBracketClose(t store.Trade, realized store.TradeRealization, exitReason string) {
	if realized.PnL < 0 {
		// Same cooldown bookkeeping as the software exit path — without it,
		// risk.cooldown_after_loss never fires on bracket stop-outs.
		e.portfolio.RecordLoss()
	}

	logger.Info("trade closed by broker bracket leg",
		"trade_id", t.ID,
		"ticker", t.Ticker,
		"reason", exitReason,
		"exit_price", realized.ExitPrice,
		"sold_qty", realized.ExitQty,
		"pnl", fmt.Sprintf("%.2f", realized.PnL),
		"pnl_pct", fmt.Sprintf("%.2f%%", realized.PnLPct),
	)
	e.publishTradeClosed(realized)
	e.checkExitResidue(t.Ticker)
}

// cancelBracketLegsForTicker cancels every working exit leg across the
// ticker's open trades and reports whether ALL of them are confirmed gone.
// Called before a SELL order: the legs hold the long's shares at the broker,
// so any other sell is rejected for insufficient free quantity until they
// are cancelled. Callers must NOT submit the sell when this returns false —
// a still-live leg would race the sell (Codex P2 on #435); the signal is
// blocked and the next analysis cycle retries with reconciled state.
//
// Leg rows are marked cancelled only after the broker confirms the cancel —
// an optimistic mark on a failed cancel would make the monitor treat the
// trade as app-managed while a live leg still works the position.
//
// Called by executeSellExit (#436) before its market sell: the SELL closes
// every open long lot, so no exit leg may outlive the position or hold its
// shares when the sell hits the broker.
func (e *Engine) cancelBracketLegsForTicker(ctx context.Context, ticker string) bool {
	openTrades, err := store.GetOpenTrades(ctx)
	if err != nil {
		logger.Warn("bracket exits: open-trades lookup for leg cancel failed", "ticker", ticker, "err", err)
		return false
	}
	return e.cancelExitLegsForTrades(ctx, openTrades, ticker)
}

func (e *Engine) cancelExitLegsForTrades(ctx context.Context, trades []store.Trade, ticker string) bool {
	allClear := true
	for _, t := range trades {
		if t.Ticker != ticker {
			continue
		}
		legs, err := store.OpenExitLegsForTrade(ctx, t.ID)
		if err != nil {
			logger.Warn("bracket exits: leg lookup for cancel failed", "trade_id", t.ID, "err", err)
			allClear = false
			continue
		}
		for _, leg := range legs {
			if err := e.executor.CancelOrder(ctx, leg.BrokerOrderID); err != nil {
				logger.Warn("bracket exits: cancel leg before sell failed",
					"trade_id", t.ID, "broker_order_id", leg.BrokerOrderID, "err", err)
				allClear = false
				continue
			}
			if err := store.UpdateOrderFill(ctx, leg.ID, 0, 0, "cancelled"); err != nil {
				logger.Warn("bracket exits: mark cancelled leg", "order_id", leg.ID, "err", err)
			}
		}
	}
	return allClear
}

// cancelClaimedExitLegs confirms every cancellation and returns the actual
// remaining aggregate quantity after any cancellation-race partial fills.
func (e *Engine) cancelClaimedExitLegs(
	ctx context.Context,
	trades []store.Trade,
	ticker string,
) (map[string]float64, float64, bool, bool) {
	remainingByTrade := make(map[string]float64, len(trades))
	var totalRemaining float64
	for _, t := range trades {
		if t.Ticker != ticker {
			continue
		}
		remaining := t.Quantity
		legs, err := store.OpenExitLegsForTrade(ctx, t.ID)
		if err != nil {
			return nil, 0, false, true
		}
		for _, leg := range legs {
			cancelErr := e.executor.CancelOrder(ctx, leg.BrokerOrderID)
			res, statusErr := e.executor.GetOrderStatus(ctx, leg.BrokerOrderID)
			if statusErr != nil {
				if errors.Is(statusErr, execution.ErrOrderNotFound) {
					continue
				}
				logger.Warn("analyst exit: cannot confirm leg cancellation",
					"broker_order_id", leg.BrokerOrderID,
					"cancel_err", cancelErr, "status_err", statusErr)
				return nil, 0, false, true
			}
			switch execution.NormalizeOrderStatus(res.Status) {
			case "cancelled", "rejected":
				if res.FilledQty > 0 {
					fillPrice := res.FilledPrice
					if fillPrice <= 0 {
						if leg.IsTakeProfit {
							fillPrice = leg.LimitPrice
						} else {
							fillPrice = leg.StopPrice
						}
					}
					var err error
					remaining, _, err = store.ApplyPartialExitAt(
						ctx, t.ID, leg.ID, fillPrice, res.FilledQty,
						execution.NormalizeOrderStatus(res.Status), res.FilledAt,
					)
					if err != nil {
						return nil, 0, false, false
					}
				} else if err := store.UpdateOrderFill(
					ctx, leg.ID, res.FilledPrice, 0,
					execution.NormalizeOrderStatus(res.Status),
				); err != nil {
					return nil, 0, false, false
				}
			case "filled":
				// The exit reconciler owns this claimed trade now. Releasing
				// it would permit a duplicate analyst/software sell.
				return nil, 0, false, false
			default:
				return nil, 0, false, true
			}
		}
		remainingByTrade[t.ID] = remaining
		totalRemaining += remaining
	}
	return remainingByTrade, totalRemaining, totalRemaining > 0, totalRemaining > 0
}
