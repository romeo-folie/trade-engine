package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// stockEntryBrokerProtection is a package variable so reconciliation tests can
// exercise a transient durable-policy read failure without taking down the
// shared database pool.
var stockEntryBrokerProtection = store.StockEntryBrokerProtection

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

// brokerManagedExitTimeInForce mirrors the stock exit submission policy.
// Alpaca reports a resting GTC leg as done_for_day after the session; that
// does not make the leg terminal, because it resumes on the next session.
// Bracket children are GTC, as are whole-share protective stops. Fractional
// protective stops are DAY-only and must retain the ordinary terminal meaning.
func brokerManagedExitTimeInForce(parentOrderID, orderType string, isStopLoss, isTakeProfit bool, quantity float64) string {
	if parentOrderID != "" && (isStopLoss || isTakeProfit) {
		return "gtc"
	}
	if orderType == "stop" && isStopLoss && math.Abs(quantity-math.Trunc(quantity)) <= 1e-9 {
		return "gtc"
	}
	return "day"
}

func normalizeBrokerExitStatus(leg store.Order, status string) string {
	return execution.NormalizeOrderStatusForTimeInForce(status,
		brokerManagedExitTimeInForce(leg.ParentOrderID, leg.OrderType, leg.IsStopLoss, leg.IsTakeProfit, leg.Quantity))
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
	// Include terminal protective rows with durable fills. A crash can occur
	// after their fill was applied to the trade but before the open trade was
	// claimed and finalized; those rows are the only restart evidence capable
	// of completing that transition.
	all, err := store.ExitLegsForMarketExit(ctx, t.ID)
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
		if required, parentID, policyErr := stockEntryBrokerProtection(ctx, t.ID); policyErr != nil && !errors.Is(policyErr, pgx.ErrNoRows) {
			// A strict entry with an unreadable policy may still have live,
			// unprojected broker children. Do not let software exits race them.
			logger.Warn("bracket exits: strict policy lookup failed — retaining broker ownership",
				"trade_id", t.ID, "ticker", t.Ticker, "err", policyErr)
			return true, policyErr
		} else if policyErr == nil && required && parentID != "" {
			// A strict protected entry must never be handed to software exits
			// merely because child projection failed or the broker legs vanished.
			// A fresh parent snapshot can still prove the broker stop is healthy
			// when a transient local child write is the only missing projection.
			e.verifyOrContainBrokerEntryAsync(t.ID, parentID, t.Ticker, t.Quantity)
			return true, nil
		}
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
					_, paperExecutor := e.executor.(*execution.PaperExecutor)
					ephemeralPaper := paperExecutor && strings.HasPrefix(leg.BrokerOrderID, "PAPER-")
					// A zero remainder means another durable protective fill already
					// accounted for the position; a purged zero-fill OCO sibling no
					// longer needs holdings proof before finalization.
					if !ephemeralPaper && t.Quantity > 1e-9 {
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
		switch normalizeBrokerExitStatus(leg, res.Status) {
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
	required, parentID, policyErr := stockEntryBrokerProtection(ctx, t.ID)
	if policyErr != nil && !errors.Is(policyErr, pgx.ErrNoRows) {
		// This path is reached when projected legs have all become terminal.
		// A transient policy read is still not proof that a strict entry can
		// safely hand ownership back to application-managed exits.
		logger.Warn("bracket exits: strict policy lookup failed — retaining broker ownership",
			"trade_id", t.ID, "ticker", t.Ticker, "err", policyErr)
		return true, errors.Join(pollErr, policyErr)
	}
	if policyErr == nil && required && parentID != "" {
		if brokerStopSnapshotUnknown(states) {
			// A failed stop lookup is not evidence that protection disappeared.
			// Keep broker ownership and retry before taking a destructive action.
			return true, pollErr
		}
		if !workingBrokerStopCoversTrade(states, brokerSnapshotRemainingTradeQty(t.Quantity, states)) {
			// A live TP is not downside protection. Retain broker ownership and
			// verify the parent snapshot before containing the position. The
			// projected legs can be stale or incomplete even while its broker
			// stop covers the full current holding.
			e.verifyOrContainBrokerEntryAsync(t.ID, parentID, t.Ticker, t.Quantity)
			return true, pollErr
		}
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
				normalizeBrokerExitStatus(s.leg, s.res.Status), s.res.FilledAt,
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
				status := normalizeBrokerExitStatus(states[i].leg, states[i].res.Status)
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
				confirmedStatus := normalizeBrokerExitStatus(states[i].leg, confirmed.Status)
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
					normalizeBrokerExitStatus(s.leg, s.res.Status), s.res.FilledAt,
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
						normalizeBrokerExitStatus(s.leg, s.res.Status),
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
					normalizeBrokerExitStatus(s.leg, s.res.Status),
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

func brokerStopSnapshotUnknown(states []legState) bool {
	for _, state := range states {
		if state.leg.IsStopLoss && !state.ok {
			return true
		}
	}
	return false
}

// brokerSnapshotRemainingTradeQty anticipates cumulative broker fills that
// will be atomically applied later in this sweep. This avoids treating a stop
// that Alpaca correctly reduced after a TP partial as too small merely because
// the local trade row has not yet caught up.
func brokerSnapshotRemainingTradeQty(currentQty float64, states []legState) float64 {
	remaining := currentQty
	for _, state := range states {
		if !state.ok || state.res.FilledQty <= state.leg.FilledQty {
			continue
		}
		remaining -= state.res.FilledQty - state.leg.FilledQty
	}
	if remaining < 0 {
		return 0
	}
	return remaining
}

func workingBrokerStopCoversTrade(states []legState, heldQty float64) bool {
	for _, state := range states {
		if !state.leg.IsStopLoss || !state.ok {
			continue
		}
		status := normalizeBrokerExitStatus(state.leg, state.res.Status)
		if status == "cancelled" || status == "rejected" || status == "filled" || state.res.Status == "held" {
			continue
		}
		qty := state.res.RemainingQty
		if qty <= 0 && state.res.Quantity == 0 {
			qty = state.leg.Quantity
		}
		if qty+1e-9 >= heldQty {
			return true
		}
	}
	return false
}

func (e *Engine) brokerStillHoldsOwnedTickerQuantity(ctx context.Context, ticker string) (bool, error) {
	brokerQty, dbQty, err := e.brokerAndOwnedTickerQuantities(ctx, ticker)
	if err != nil {
		return false, err
	}
	const tolerance = 0.01
	return dbQty != 0 && brokerQty*dbQty > 0 && math.Abs(brokerQty-dbQty) <= tolerance, nil
}

func (e *Engine) brokerMatchesOwnedTickerQuantity(ctx context.Context, ticker string) (bool, error) {
	brokerQty, dbQty, err := e.brokerAndOwnedTickerQuantities(ctx, ticker)
	if err != nil {
		return false, err
	}
	return math.Abs(brokerQty-dbQty) <= 0.01, nil
}

func (e *Engine) brokerAndOwnedTickerQuantities(ctx context.Context, ticker string) (brokerQty, dbQty float64, err error) {
	positions, err := e.executor.GetPositions(ctx)
	if err != nil {
		return 0, 0, err
	}
	owned, err := store.GetOwnedPositionTrades(ctx)
	if err != nil {
		return 0, 0, err
	}
	ticker = normalizedTicker(ticker)
	for _, p := range positions {
		if normalizedTicker(p.Ticker) != ticker {
			continue
		}
		qty := p.Quantity
		if p.Side == "short" && qty > 0 {
			qty = -qty
		}
		brokerQty += qty
	}
	for _, trade := range owned {
		if normalizedTicker(trade.Ticker) != ticker {
			continue
		}
		qty := trade.Quantity
		if trade.Direction == "short" {
			qty = -qty
		}
		dbQty += qty
	}
	return brokerQty, dbQty, nil
}

func normalizedTicker(ticker string) string {
	return strings.ToUpper(strings.TrimSpace(ticker))
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
	var handled bool
	err := store.WithTradeOrderLock(t.ID, func() error {
		var closeErr error
		handled, closeErr = e.closeBracketTradeLocked(ctx, t, states, filledIdx, fallbackHandled)
		return closeErr
	})
	return handled, err
}

// closeBracketTradeLocked is the terminal side of the same per-trade
// lifecycle lock used by initial-stop, recovery, and trailing mutations. A
// filled stop must either finish reconciliation before a trailing worker can
// inspect the trade, or wait for that worker to complete its fresh-state
// check; otherwise a stale tick can submit a successor SELL after the fill.
func (e *Engine) closeBracketTradeLocked(
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
		status := normalizeBrokerExitStatus(s.leg, s.res.Status)
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
	// The fill that flattened this trade can race a previously unprojected
	// broker stop. Local exit-leg rows are not a complete cancellation
	// inventory after a projection/replacement failure, so inspect the broker's
	// working orders before finalizing. Otherwise a leftover engine-owned SELL
	// can fill into a flat account and open an unintended short (VOYG/TSM).
	if err := e.cancelResidualEngineExitOrders(ctx, t, states); err != nil {
		if claimed && !fillPersisted {
			_ = store.ReleaseTradeExitClaim(ctx, t.ID)
		}
		logger.Warn("bracket exits: residual broker exit cleanup incomplete",
			"trade_id", t.ID, "ticker", t.Ticker, "err", err)
		return true, err
	}
	// A residual can fill between the winning-leg lookup and the working-order
	// snapshot, at which point Alpaca no longer lists it as open. Finalization
	// is safe only when the broker position still matches every durable owned
	// lot for this ticker (including any other pyramided lots).
	matched, err := e.brokerMatchesOwnedTickerQuantity(ctx, t.Ticker)
	if err != nil || !matched {
		if claimed && !fillPersisted {
			_ = store.ReleaseTradeExitClaim(ctx, t.ID)
		}
		if err == nil {
			err = errors.New("broker position differs from durable owned quantity")
		}
		logger.Warn("bracket exits: residual position safety check incomplete",
			"trade_id", t.ID, "ticker", t.Ticker, "err", err)
		return true, fmt.Errorf("verify broker position after residual exit cleanup: %w", err)
	}
	realized, err := store.FinalizeTradeExit(ctx, t.ID)
	if err != nil {
		logger.Warn("bracket exits: finalize broker-filled trade", "trade_id", t.ID, "err", err)
		return true, err
	}
	e.recordBracketClose(t, realized, exitReason)
	return true, nil
}

// cancelResidualEngineExitOrders retires working engine-owned exit orders that
// escaped the local leg projection. It runs only after the winning exit has
// durably reduced the tracked trade to zero, while the trade-order lock still
// excludes trailing/protective-stop mutations.
//
// We intentionally do not cancel an arbitrary user order sharing a ticker:
// unprojected orders must resolve to this exact trade through durable intent
// ownership. Projected legs are also safe to cancel by broker ID.
func (e *Engine) cancelResidualEngineExitOrders(
	ctx context.Context,
	t store.Trade,
	states []legState,
) error {
	lister, ok := e.executor.(execution.WorkingOrdersLister)
	if !ok {
		// Brokers without a working-order view cannot provide the final
		// inventory check. Their existing per-leg reconciliation remains the
		// best available protection (the production Alpaca executor implements
		// this interface).
		return nil
	}
	working, err := lister.GetWorkingOrders(ctx)
	if err != nil {
		return fmt.Errorf("list working broker exits: %w", err)
	}
	known := make(map[string]struct{}, len(states))
	for _, state := range states {
		known[state.leg.BrokerOrderID] = struct{}{}
	}
	wantSide := string(execution.OrderSideSell)
	if t.Direction == "short" {
		wantSide = string(execution.OrderSideBuy)
	}
	var cleanupErrs []error
	for _, order := range working {
		if normalizedTicker(order.Ticker) != normalizedTicker(t.Ticker) ||
			!strings.EqualFold(order.Side, wantSide) || order.OrderID == "" {
			continue
		}
		_, projected := known[order.OrderID]
		var ownership *residualExitOwnership
		if !projected {
			var ownershipErr error
			ownership, ownershipErr = residualExitIntentForTrade(ctx, e.executor, t.ID, order)
			if ownershipErr != nil {
				cleanupErrs = append(cleanupErrs, ownershipErr)
				continue
			}
			if ownership == nil {
				// This may belong to another pyramided lot or to the user. Never
				// infer ownership from ticker, side, or an engine-looking ID.
				continue
			}
			if err := persistResidualExitOrder(ctx, t, order, ownership); err != nil {
				cleanupErrs = append(cleanupErrs, err)
				continue
			}
		}
		persisted, err := store.GetOrderByBrokerOrderID(ctx, order.OrderID)
		if err != nil || persisted == nil {
			if err == nil {
				err = errors.New("missing durable order projection")
			}
			cleanupErrs = append(cleanupErrs, fmt.Errorf("load residual exit %s after projection: %w", order.OrderID, err))
			continue
		}
		cancelErr := e.executor.CancelOrder(ctx, order.OrderID)
		status, statusErr := e.executor.GetOrderStatus(ctx, order.OrderID)
		if errors.Is(statusErr, execution.ErrOrderNotFound) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("confirm residual exit %s cancellation: broker no longer has order", order.OrderID))
			continue
		}
		if statusErr != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("confirm residual exit %s cancellation: cancel=%v status=%w",
				order.OrderID, cancelErr, statusErr))
			continue
		}
		normalized := normalizeBrokerExitStatus(*persisted, status.Status)
		if normalized == "filled" || status.FilledQty > 1e-9 ||
			(normalized != "cancelled" && normalized != "rejected") {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("residual exit %s remains %s after cancel (cancel error: %v)",
				order.OrderID, status.Status, cancelErr))
			continue
		}
		if err := store.UpdateOrderFillAtForBrokerOrder(ctx, persisted.ID, order.OrderID,
			status.FilledPrice, 0, normalized, status.FilledAt); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("persist residual exit %s terminal cancellation: %w", order.OrderID, err))
			continue
		}
		logger.Warn("bracket exits: cancelled residual engine exit after flatten",
			"trade_id", t.ID, "ticker", t.Ticker,
			"broker_order_id", order.OrderID)
	}
	return errors.Join(cleanupErrs...)
}

type residualExitOwnership struct {
	intent        *store.StockOrderIntent
	parentOrderID string
	childType     string
}

// residualExitIntentForTrade proves a working order belongs to the trade being
// closed. Bracket children receive their own broker-generated client IDs, so a
// child that escaped projection is resolved from the nested durable parent
// snapshot, never by guessing that its client ID equals the parent's.
func residualExitIntentForTrade(ctx context.Context, executor execution.Executor, tradeID string, order execution.WorkingOrder) (*residualExitOwnership, error) {
	intent, err := store.GetStockOrderIntentByBrokerOrderID(ctx, order.OrderID)
	if err != nil {
		return nil, fmt.Errorf("find residual exit %s by broker ID: %w", order.OrderID, err)
	}
	if intent != nil && intent.TradeID == tradeID {
		return residualOwnershipForIntent(ctx, order.OrderID, intent)
	}
	if order.TradeID != "" {
		intent, err = store.GetStockOrderIntentByClientOrderID(ctx, order.TradeID)
		if err != nil {
			return nil, fmt.Errorf("find residual exit %s by client ID: %w", order.OrderID, err)
		}
		if intent != nil && intent.TradeID == tradeID {
			return residualOwnershipForIntent(ctx, order.OrderID, intent)
		}
	}
	parent, err := store.GetStockEntryOrderIntent(ctx, tradeID)
	if err != nil {
		return nil, fmt.Errorf("find bracket parent for residual exit %s: %w", order.OrderID, err)
	}
	lookup, ok := executor.(execution.ClientOrderLookup)
	if parent == nil || parent.ClientOrderID == "" || !ok {
		return nil, nil
	}
	parentSnapshot, err := lookup.GetOrderByClientID(ctx, parent.ClientOrderID)
	if err != nil {
		return nil, fmt.Errorf("resolve bracket parent for residual exit %s: %w", order.OrderID, err)
	}
	for _, child := range parentSnapshot.ChildOrders {
		if child.BrokerOrderID == order.OrderID {
			parentOrderID := parentSnapshot.BrokerOrderID
			if parentOrderID == "" {
				parentOrderID = parent.BrokerOrderID
			}
			if parentOrderID == "" {
				return nil, fmt.Errorf("resolve bracket parent for residual exit %s: missing broker parent ID", order.OrderID)
			}
			return &residualExitOwnership{intent: parent, parentOrderID: parentOrderID, childType: child.Type}, nil
		}
	}
	return nil, nil
}

func residualOwnershipForIntent(ctx context.Context, brokerOrderID string, intent *store.StockOrderIntent) (*residualExitOwnership, error) {
	ownership := &residualExitOwnership{intent: intent}
	if intent.ParentIntentID == "" {
		return ownership, nil
	}
	parent, err := store.GetStockOrderIntentByID(ctx, intent.ParentIntentID)
	if err != nil {
		return nil, fmt.Errorf("find durable parent for residual exit %s: %w", brokerOrderID, err)
	}
	if parent == nil || parent.BrokerOrderID == "" {
		return nil, fmt.Errorf("find durable parent for residual exit %s: missing broker parent ID", brokerOrderID)
	}
	ownership.parentOrderID = parent.BrokerOrderID
	ownership.childType = intent.OrderType
	if ownership.childType == "" {
		return nil, fmt.Errorf("find durable parent for residual exit %s: child has no type", brokerOrderID)
	}
	return ownership, nil
}

// persistResidualExitOrder gives the ordinary claimed-exit reconciler durable
// ownership of an unprojected order before cancellation. If cancellation is
// asynchronous or the order filled meanwhile, the trade cannot be finalized
// and forgotten; the pending row is polled until broker truth is terminal.
func persistResidualExitOrder(ctx context.Context, t store.Trade, order execution.WorkingOrder, ownership *residualExitOwnership) error {
	intent := ownership.intent
	if existing, err := store.GetOrderByBrokerOrderID(ctx, order.OrderID); err != nil {
		return fmt.Errorf("look up residual exit %s projection: %w", order.OrderID, err)
	} else if existing != nil {
		if existing.TradeID != t.ID {
			return fmt.Errorf("residual exit %s is already projected to trade %s", order.OrderID, existing.TradeID)
		}
		return nil
	}
	quantity := order.RemainingQty
	if quantity <= 0 {
		quantity = intent.RequestedQty
	}
	if quantity <= 0 {
		quantity = t.Quantity
	}
	projection := store.Order{
		TradeID: t.ID, BrokerOrderID: order.OrderID, Ticker: t.Ticker,
		Side: order.Side, OrderType: intent.OrderType, Quantity: quantity,
		Status: "pending", IsStopLoss: intent.OrderType == string(execution.OrderTypeStop),
	}
	if ownership.parentOrderID != "" {
		// This durable child already has its own intent, but its order row was
		// lost. Keep both the child intent and its real bracket metadata.
		projection.StockIntentID = intent.ID
		projection.ParentOrderID = ownership.parentOrderID
		projection.OrderType = ownership.childType
		projection.IsStopLoss = strings.EqualFold(ownership.childType, string(execution.OrderTypeStop))
		projection.IsTakeProfit = strings.EqualFold(ownership.childType, string(execution.OrderTypeLimit))
	} else if intent.BrokerOrderID == order.OrderID {
		// The child/standalone intent is this exact broker order.
		projection.StockIntentID = intent.ID
	} else {
		// This is a broker-created child identified through its bracket parent.
		// The parent intent already projects the entry, so it cannot be reused as
		// this child row's stock intent.
		projection.ParentOrderID = ownership.parentOrderID
		projection.OrderType = ownership.childType
		projection.IsStopLoss = strings.EqualFold(ownership.childType, string(execution.OrderTypeStop))
		projection.IsTakeProfit = strings.EqualFold(ownership.childType, string(execution.OrderTypeLimit))
		if projection.OrderType == "" {
			return fmt.Errorf("persist residual exit %s: bracket child has no type", order.OrderID)
		}
	}
	if _, err := store.InsertOrderWithRetry(ctx, projection); err != nil {
		return fmt.Errorf("persist residual exit %s: %w", order.OrderID, err)
	}
	return nil
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
			switch normalizeBrokerExitStatus(leg, res.Status) {
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
						normalizeBrokerExitStatus(leg, res.Status), res.FilledAt,
					)
					if err != nil {
						return nil, 0, false, false
					}
				} else if err := store.UpdateOrderFill(
					ctx, leg.ID, res.FilledPrice, 0,
					normalizeBrokerExitStatus(leg, res.Status),
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
