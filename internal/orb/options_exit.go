package orb

import (
	"context"
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
	held := s.heldOptionSymbols(ctx)
	for _, t := range trades {
		reason, exit := s.optionExit.plan(ctx, t, now, sessionEnding)
		if !exit {
			continue
		}
		var exitPrice float64
		exitSource := "broker"
		if held != nil && !held[t.OCCSymbol] {
			// Already flat at the broker — do NOT re-sell. Record the close with a
			// best-effort price; the reconciler (#416) heals the exact fill.
			exitPrice = s.optionExit.estimateExitPrice(ctx, t)
			exitSource = "reconcile"
		} else {
			px, confirmed, err := s.optionExit.closeOption(ctx, t)
			if err != nil {
				slog.Error("orb options: exit close failed",
					"ticker", t.Ticker, "occ", t.OCCSymbol, "reason", reason, "err", err)
				continue
			}
			if !confirmed {
				// Sell placed but not confirmed filled. Do NOT close the row — that
				// would orphan a still-held option. Retry next sweep; once the
				// position clears, the flat-guard records it (no duplicate sell).
				slog.Warn("orb options: exit sell not confirmed filled; leaving open to retry",
					"ticker", t.Ticker, "occ", t.OCCSymbol, "reason", reason)
				continue
			}
			exitPrice = px
		}
		// Long option: realized P&L uses the long formula regardless of ORB direction.
		pnl := ExitPnLM("buy", t.FillPrice, exitPrice, t.Qty, t.Multiplier)
		if dbErr := store.CloseORBTrade(ctx, s.db, t.ID, reason, exitSource, exitPrice, pnl); dbErr != nil {
			slog.Error("orb options: close DB record failed",
				"ticker", t.Ticker, "occ", t.OCCSymbol, "err", dbErr)
			continue
		}
		delete(s.trackedTrades, t.OrderID)
		s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
			OrderID: t.OrderID, Ticker: t.Ticker, Side: t.Side, Status: "closed",
		})
		slog.Info("orb options: exit",
			"ticker", t.Ticker, "occ", t.OCCSymbol, "reason", reason,
			"exit_price", exitPrice, "exit_source", exitSource, "pnl", pnl)
	}
}

// heldOptionSymbols returns the set of OCC symbols the broker currently holds as
// option positions, or nil when the positions read fails. Used to avoid re-selling
// a position that is already flat.
func (s *ORBScheduler) heldOptionSymbols(ctx context.Context) map[string]bool {
	positions, err := s.executor.GetPositions(ctx)
	if err != nil {
		slog.Warn("orb options: broker positions read failed; proceeding without flat-guard", "err", err)
		return nil
	}
	held := make(map[string]bool)
	for _, p := range positions {
		switch {
		case p.Option != nil && p.Option.OCCSymbol != "":
			held[p.Option.OCCSymbol] = true
		default:
			// Non-broker adapters may not populate Option; fall back to the raw symbol,
			// which for an Alpaca option position is the OCC symbol.
			held[p.Ticker] = true
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
// It returns (fillPrice, confirmed, err). confirmed is true ONLY when the broker
// reports the sell as filled with a real price. An 'accepted'/pending market order
// returns confirmed=false so the caller does NOT record the close — otherwise a
// sell that later rejects or never fills would leave a live option marked 'closed'
// and unmanaged, violating the intraday-flatten invariant. The caller retries on
// the next sweep; the flat-guard there records the close (and prevents a duplicate
// sell) once the position actually clears.
func (m *OptionExitManager) closeOption(ctx context.Context, t store.ORBTrade) (fillPrice float64, confirmed bool, err error) {
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
		Quantity: float64(t.Qty),
		Option:   &contract,
	})
	if err != nil {
		return 0, false, err
	}
	if execution.NormalizeOrderStatus(res.Status) == "filled" && res.FilledPrice > 0 {
		return res.FilledPrice, true, nil
	}
	return 0, false, nil
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
