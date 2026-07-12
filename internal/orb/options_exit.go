package orb

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// optionExitCheckInterval throttles the app-managed exit sweep.
const optionExitCheckInterval = 15 * time.Second

// optionsFlattenCutoffMin is the intraday flatten cutoff (15:50 ET, 10 min before
// the 16:00 ET US options close) — after it, any open option is flattened so
// nothing is held overnight (no assignment/expiration risk).
const optionsFlattenCutoffMin = 15*60 + 50

// optionsSessionEnding reports whether now is at/after the intraday flatten cutoff
// in US/Eastern, regardless of the clock's own zone.
func optionsSessionEnding(now time.Time) bool {
	et := now.In(nyLoc)
	return et.Hour()*60+et.Minute() >= optionsFlattenCutoffMin
}

// SetOptionExitManager wires the app-managed exit manager into the scheduler
// (issue #418). nil leaves options-ORB exits disabled.
func (s *ORBScheduler) SetOptionExitManager(m *OptionExitManager) { s.optionExit = m }

// enforceOptionExits is the per-tick sweep that evaluates and executes app-managed
// option exits. Inert (no-op) when the exit manager is unset, so non-options runs
// are unaffected.
func (s *ORBScheduler) enforceOptionExits(ctx context.Context) {
	if s.optionExit == nil {
		return
	}
	if time.Since(s.lastOptionExitCheckAt) < optionExitCheckInterval {
		return
	}
	s.lastOptionExitCheckAt = s.now()

	trades, err := store.GetOpenFilledOptionTrades(ctx, s.db)
	if err != nil {
		slog.Error("orb options: load open filled option trades failed", "err", err)
		return
	}
	if len(trades) == 0 {
		return
	}
	now := s.now()
	sessionEnding := optionsSessionEnding(now)
	// Snapshot broker-held option positions once (keyed by OCC symbol). If a prior
	// sell succeeded but its DB record write failed, the row is still 'filled' and
	// gets re-swept — the held check makes that a record-only heal instead of a
	// second, naked sell. held is nil only when the positions read fails; in that
	// (rare, transient) window we still attempt the protective exit.
	heldQty := s.heldOptionQuantities(ctx)
	// Working sell-to-close orders the broker still has open, keyed by OCC symbol.
	// Adopting these covers the restart case: pendingOptionExits is in-memory, so
	// after a restart an in-flight exit sell would be untracked and re-sent — the
	// broker's own open-orders are the durable source of truth.
	workingSells := s.workingOptionSells(ctx)
	for _, t := range trades {
		// Adopt an in-flight exit sell the broker holds for this contract that we are
		// not tracking in memory (e.g. after a restart), so it's polled below rather
		// than duplicated by a fresh sell. Re-derive the exit reason from the current
		// conditions so the recorded exit_type stays one of the valid values (the
		// original trigger is lost across the restart); default to the EOD flatten
		// when no live condition fires.
		if _, tracked := s.pendingOptionExits[t.ID]; !tracked {
			if oid, working := workingSells[t.OCCSymbol]; working {
				reason, exit := s.optionExit.plan(ctx, t, now, sessionEnding)
				if !exit || reason == "" {
					reason = optExitEOD
				}
				// The adopted sell closes what the broker still holds (capped at the row).
				adoptQty := t.Qty
				if heldQty != nil {
					if h := int(heldQty[t.OCCSymbol]); h < adoptQty {
						adoptQty = h
					}
				}
				s.pendingOptionExits[t.ID] = pendingExit{orderID: oid, reason: reason, qty: adoptQty}
			}
		}
		// If an exit sell is already in flight for this trade, poll it instead of
		// submitting another — otherwise a not-yet-filled sell would be re-sent every
		// sweep, stacking multiple sell-to-close orders on one position.
		if pe, ok := s.pendingOptionExits[t.ID]; ok {
			if s.pollPendingOptionExit(ctx, t, pe) {
				continue // recorded, still working, or transient error — handled
			}
			// else: the tracked order is terminal-but-unfilled — fall through to
			// attempt a fresh exit below.
		}

		reason, exit := s.optionExit.plan(ctx, t, now, sessionEnding)
		if !exit {
			continue
		}

		// Cap the sell at the broker-held contract count so a prior partial fill (or
		// an already-flat position) can never over-sell into a short. When the
		// positions read failed (heldQty nil) we fall back to the full DB quantity.
		sellQty := t.Qty
		if heldQty != nil {
			if h := int(heldQty[t.OCCSymbol]); h < sellQty {
				sellQty = h
			}
		}
		if heldQty != nil && sellQty <= 0 {
			// Already flat / fully closed by a prior partial — do NOT re-sell. Record
			// the close with a best-effort price; the reconciler (#416) heals it.
			if s.recordOptionClose(ctx, t, reason, "reconcile", s.optionExit.estimateExitPrice(ctx, t), t.Qty) {
				delete(s.pendingOptionExits, t.ID)
			}
			continue
		}

		px, confirmed, orderID, err := s.optionExit.closeOption(ctx, t, sellQty)
		if err != nil {
			slog.Error("orb options: exit close failed",
				"ticker", t.Ticker, "occ", t.OCCSymbol, "reason", reason, "err", err)
			continue
		}
		if !confirmed {
			// Sell submitted but not confirmed filled. Track the in-flight order so
			// the next sweep POLLS it (above) rather than sending a second sell, and
			// leave the row open until the fill confirms.
			s.pendingOptionExits[t.ID] = pendingExit{orderID: orderID, reason: reason, qty: sellQty}
			slog.Warn("orb options: exit sell not confirmed; tracking order to poll",
				"ticker", t.Ticker, "occ", t.OCCSymbol, "reason", reason, "order_id", orderID)
			continue
		}
		if s.recordOptionClose(ctx, t, reason, "broker", px, sellQty) {
			delete(s.pendingOptionExits, t.ID)
		} else {
			// The sell FILLED but the DB close failed — retain the filled order so the
			// next sweep polls it and re-records with the real price (not an estimate),
			// and never sends a second sell.
			s.pendingOptionExits[t.ID] = pendingExit{orderID: orderID, reason: reason, qty: sellQty}
		}
	}
}

// pendingExit records an exit sell that was submitted but not yet confirmed filled,
// so it can be polled instead of re-sent. qty is the contract count the sell was
// for, so the recorded P&L reflects what was actually sold (not the full row).
type pendingExit struct {
	orderID string
	reason  string
	qty     int
}

// pollPendingOptionExit checks an in-flight exit sell. It returns true when the
// trade is handled for this sweep (filled-and-recorded, still working, or a
// transient status-check error — in all of which no new sell should be sent), and
// false only when the order is terminal-but-unfilled (rejected/cancelled/gone), so
// the caller attempts a fresh exit.
func (s *ORBScheduler) pollPendingOptionExit(ctx context.Context, t store.ORBTrade, pe pendingExit) bool {
	res, err := s.executor.GetOrderStatus(ctx, pe.orderID)
	if err != nil {
		if errors.Is(err, execution.ErrOrderNotFound) {
			delete(s.pendingOptionExits, t.ID) // gone at broker — re-attempt
			return false
		}
		slog.Warn("orb options: exit status check failed; will retry",
			"occ", t.OCCSymbol, "order_id", pe.orderID, "err", err)
		return true // transient — do NOT re-sell this sweep
	}
	if execution.NormalizeOrderStatus(res.Status) == "filled" && res.FilledPrice > 0 {
		// Retain the pending marker if the DB write fails, so the next sweep re-records
		// this filled order with its real price rather than losing it. Either way the
		// order is done — return true so no new sell is sent.
		if s.recordOptionClose(ctx, t, pe.reason, "broker", res.FilledPrice, pe.qty) {
			delete(s.pendingOptionExits, t.ID)
		}
		return true
	}
	if !execution.IsTerminalOrderStatus(res.Status) {
		return true // still working — wait for the fill
	}
	delete(s.pendingOptionExits, t.ID) // terminal but not filled — re-attempt
	return false
}

// recordOptionClose writes the exit to the trade row (multiplier-correct P&L on
// the quantity actually sold, not the full row — matching time_exit's flattenQty
// accounting so a capped/partial sell doesn't over-state P&L), untracks it, and
// publishes the update. Returns false when the DB write failed, so the caller can
// RETAIN the (already-filled) exit order and re-record it with the real fill price
// next sweep instead of losing it to an estimated-price heal.
func (s *ORBScheduler) recordOptionClose(ctx context.Context, t store.ORBTrade, reason, exitSource string, exitPrice float64, qty int) bool {
	pnl := ExitPnLM("buy", t.FillPrice, exitPrice, qty, t.Multiplier)
	if dbErr := store.CloseORBTrade(ctx, s.db, t.ID, reason, exitSource, exitPrice, pnl); dbErr != nil {
		slog.Error("orb options: close DB record failed",
			"ticker", t.Ticker, "occ", t.OCCSymbol, "err", dbErr)
		return false
	}
	delete(s.trackedTrades, t.OrderID)
	s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
		OrderID: t.OrderID, Ticker: t.Ticker, Side: t.Side, Status: "closed",
	})
	slog.Info("orb options: exit",
		"ticker", t.Ticker, "occ", t.OCCSymbol, "reason", reason,
		"exit_price", exitPrice, "exit_source", exitSource, "pnl", pnl)
	return true
}

// workingOptionSells returns the broker's open sell-to-close option orders keyed by
// OCC symbol (orderID as value), or nil when the broker doesn't expose working
// orders or the read fails. Used to re-adopt an in-flight exit after a restart.
func (s *ORBScheduler) workingOptionSells(ctx context.Context) map[string]string {
	lister, ok := s.executor.(execution.WorkingOrdersLister)
	if !ok {
		return nil
	}
	orders, err := lister.GetWorkingOrders(ctx)
	if err != nil {
		slog.Warn("orb options: working orders read failed", "err", err)
		return nil
	}
	out := make(map[string]string)
	for _, o := range orders {
		if strings.EqualFold(o.Side, "sell") && execution.IsOCCSymbol(o.Ticker) {
			out[o.Ticker] = o.OrderID
		}
	}
	return out
}

// heldOptionQuantities returns the count of LONG contracts the broker currently
// holds per OCC symbol, or nil when the positions read fails. The exit sweep caps
// each sell at this so a prior partial fill (or an already-flat position) can never
// over-sell into a short; a 0/absent entry means "no long contracts to close".
//
// Options-ORB is long-only, so a SHORT position (negative qty) is treated as flat —
// selling it would deepen the short, not close a long. Only positive long holdings
// are counted.
func (s *ORBScheduler) heldOptionQuantities(ctx context.Context) map[string]float64 {
	positions, err := s.executor.GetPositions(ctx)
	if err != nil {
		slog.Warn("orb options: broker positions read failed; proceeding without flat-guard", "err", err)
		return nil
	}
	held := make(map[string]float64)
	for _, p := range positions {
		if p.Quantity <= 0 || strings.EqualFold(p.Side, "short") {
			continue // not a long option position — nothing to sell-to-close
		}
		switch {
		case p.Option != nil && p.Option.OCCSymbol != "":
			held[p.Option.OCCSymbol] = p.Quantity
		case execution.IsOCCSymbol(p.Ticker):
			// Non-broker adapters may not populate Option; for an Alpaca option the
			// raw symbol is the OCC symbol.
			held[p.Ticker] = p.Quantity
		}
	}
	return held
}

// Options-ORB exits are app-managed (issue #415): Alpaca offers no bracket/OCO on
// options, so the engine watches the UNDERLYING against the stored ORB stop/target
// and market-closes the option itself, adds an IV-crush time exit, and enforces a
// hard end-of-session flatten (intraday-only — no overnight, no assignment risk).

// Exit-reason values recorded in orb_trades.exit_type. 'tp'/'stop'/'time_exit'
// match the existing ORB conventions; 'eod' is the session-flatten catch-all.
const (
	optExitStop   = "stop"
	optExitTarget = "tp"
	optExitTime   = "time_exit"
	optExitEOD    = "eod"
)

// OptionExitInputs are the observable facts for one filled option position.
type OptionExitInputs struct {
	Side               string  // ORB direction: "buy" (call) | "sell" (put)
	UnderlyingStop     float64 // stored StopLoss level (underlying price)
	UnderlyingTarget   float64 // stored TakeProfit level (underlying price)
	CurrentUnderlying  float64
	EntryOptionPrice   float64
	CurrentOptionPrice float64
	OptionPriceKnown   bool // false when the option quote is unavailable
	HeldMinutes        int
	TimeExitMinutes    int  // 0 disables the time exit
	SessionEnding      bool // true when the position must be flattened before close
}

// decideOptionExit chooses the exit reason for a position, or ("", false) to hold.
// Order: protective/profit underlying crossings first (most informative), then the
// IV-crush time exit, then the end-of-session flatten as a hard catch-all — so a
// session-ending position is ALWAYS closed, whatever its price.
func decideOptionExit(in OptionExitInputs) (reason string, exit bool) {
	switch strings.ToLower(in.Side) {
	case "buy", "long": // long breakout → call: stop below entry, target above
		if in.UnderlyingStop > 0 && in.CurrentUnderlying <= in.UnderlyingStop {
			return optExitStop, true
		}
		if in.UnderlyingTarget > 0 && in.CurrentUnderlying >= in.UnderlyingTarget {
			return optExitTarget, true
		}
	case "sell", "short": // short breakout → put: stop above entry, target below
		if in.UnderlyingStop > 0 && in.CurrentUnderlying >= in.UnderlyingStop {
			return optExitStop, true
		}
		if in.UnderlyingTarget > 0 && in.CurrentUnderlying <= in.UnderlyingTarget {
			return optExitTarget, true
		}
	}
	// IV-crush time exit: past the window AND not in profit (needs a live option price).
	if in.TimeExitMinutes > 0 && in.HeldMinutes >= in.TimeExitMinutes &&
		in.OptionPriceKnown && in.CurrentOptionPrice <= in.EntryOptionPrice {
		return optExitTime, true
	}
	// End-of-session flatten — hard invariant, no overnight hold.
	if in.SessionEnding {
		return optExitEOD, true
	}
	return "", false
}

// UnderlyingPricer supplies the current price of an underlying. Implemented over
// the market-data provider (issue #418 wiring).
type UnderlyingPricer interface {
	UnderlyingPrice(ctx context.Context, underlying string) (float64, bool)
}

// OptionQuoter supplies the current mid of a specific option contract, used by the
// time exit's profitability check. Optional: a nil quoter disables the time exit.
type OptionQuoter interface {
	OptionMid(ctx context.Context, underlying, occSymbol string) (float64, bool)
}

// OptionExitManager evaluates and executes app-managed option exits.
type OptionExitManager struct {
	pricer          UnderlyingPricer
	quoter          OptionQuoter
	placer          optionsPlacer
	timeExitMinutes int
}

// NewOptionExitManager builds the manager. quoter may be nil (time exit disabled).
func NewOptionExitManager(pricer UnderlyingPricer, quoter OptionQuoter, placer optionsPlacer, timeExitMinutes int) *OptionExitManager {
	return &OptionExitManager{pricer: pricer, quoter: quoter, placer: placer, timeExitMinutes: timeExitMinutes}
}

// plan decides the exit reason for one filled option trade. It returns ("", false)
// to hold — including when the underlying price is unavailable and the session is
// not ending, since acting on missing data could close a healthy position. A
// session-ending position is always flattened, even without a fresh price.
func (m *OptionExitManager) plan(ctx context.Context, t store.ORBTrade, now time.Time, sessionEnding bool) (string, bool) {
	px, ok := m.pricer.UnderlyingPrice(ctx, t.Underlying)
	if !ok {
		if sessionEnding {
			return optExitEOD, true
		}
		return "", false
	}
	var curOpt float64
	var optKnown bool
	if m.quoter != nil {
		curOpt, optKnown = m.quoter.OptionMid(ctx, t.Underlying, t.OCCSymbol)
	}
	anchor := t.CreatedAt
	if t.FilledAt != nil {
		anchor = *t.FilledAt
	}
	return decideOptionExit(OptionExitInputs{
		Side:               t.Side,
		UnderlyingStop:     t.StopLoss,
		UnderlyingTarget:   t.TakeProfit,
		CurrentUnderlying:  px,
		EntryOptionPrice:   t.FillPrice,
		CurrentOptionPrice: curOpt,
		OptionPriceKnown:   optKnown,
		HeldMinutes:        int(now.Sub(anchor).Minutes()),
		TimeExitMinutes:    m.timeExitMinutes,
		SessionEnding:      sessionEnding,
	})
}

// closeOption market-sells the long option to flatten the position. Selling closes
// a long, so this is a SELL order regardless of the ORB direction (which only chose
// call vs put at entry).
//
// It returns (fillPrice, confirmed, orderID, err). confirmed is true ONLY when the
// broker reports the sell as filled with a real price. An 'accepted'/pending market
// order returns confirmed=false with the broker orderID, so the caller can TRACK
// and poll that order next sweep instead of submitting a second sell-to-close (and
// does not record the close, which would orphan a still-held option).
func (m *OptionExitManager) closeOption(ctx context.Context, t store.ORBTrade, qty int) (fillPrice float64, confirmed bool, orderID string, err error) {
	var expiry time.Time
	if t.Expiry != nil {
		expiry = *t.Expiry
	}
	contract := execution.OptionContract{
		Underlying: t.Underlying,
		Expiry:     expiry,
		Strike:     t.Strike,
		Right:      t.Right,
		OCCSymbol:  t.OCCSymbol,
		Multiplier: t.Multiplier,
	}
	res, err := m.placer.PlaceOrder(ctx, execution.OrderRequest{
		Ticker:   t.Underlying,
		SecType:  "OPT",
		Side:     execution.OrderSideSell, // sell-to-close the long
		Type:     execution.OrderTypeMarket,
		Quantity: float64(qty),
		Option:   &contract,
	})
	if err != nil {
		return 0, false, "", err
	}
	if execution.NormalizeOrderStatus(res.Status) == "filled" && res.FilledPrice > 0 {
		return res.FilledPrice, true, res.BrokerOrderID, nil
	}
	return 0, false, res.BrokerOrderID, nil
}

// estimateExitPrice is the best available exit price without a broker fill: the
// live option mid, falling back to the entry price (P&L 0). Used both as the
// fallback in closeOption and when recording a position that is already flat at
// the broker (so a DB-record retry never triggers a second sell). The broker-truth
// reconciler (#416) heals the exact price later.
func (m *OptionExitManager) estimateExitPrice(ctx context.Context, t store.ORBTrade) float64 {
	if m.quoter != nil {
		if mid, ok := m.quoter.OptionMid(ctx, t.Underlying, t.OCCSymbol); ok && mid > 0 {
			return mid
		}
	}
	return t.FillPrice
}
