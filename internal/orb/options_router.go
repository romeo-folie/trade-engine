package orb

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/options"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// optionsPlacer is the broker capability the router needs to submit an option
// order. *execution.AlpacaBroker satisfies it (SecType "OPT" path).
type optionsPlacer interface {
	PlaceOrder(ctx context.Context, req execution.OrderRequest) (execution.OrderResult, error)
}

// OptionsRouter turns an ORB breakout signal into a long single-leg option order.
// It is the post-signal execution vehicle for options-ORB (issue #414): run the
// fail-closed gate pipeline (#412), size by premium-at-risk, and place a limit
// order at mid. It is long-only — the direction chooses call vs put, but the
// order is always a BUY, so max loss is the premium paid.
type OptionsRouter struct {
	gates           *options.Engine
	placer          optionsPlacer
	underlyings     map[string]bool
	premiumBudget   float64 // dollars of premium to deploy per trade
	maxLossPerTrade float64 // hard cap on premium at risk (0 = no extra cap)
}

// NewOptionsRouter builds the router. underlyings is the set of options-mode
// symbols; a ticker outside the set is not handled (the ORB bracket path runs
// unchanged for it).
func NewOptionsRouter(gates *options.Engine, placer optionsPlacer, underlyings []string, premiumBudget, maxLossPerTrade float64) *OptionsRouter {
	set := make(map[string]bool, len(underlyings))
	for _, u := range underlyings {
		set[strings.ToUpper(u)] = true
	}
	return &OptionsRouter{
		gates:           gates,
		placer:          placer,
		underlyings:     set,
		premiumBudget:   premiumBudget,
		maxLossPerTrade: maxLossPerTrade,
	}
}

// Handles reports whether ticker is an options-mode underlying. Safe on a nil
// router (returns false), so the scheduler hook is inert when options-ORB is off.
func (r *OptionsRouter) Handles(ticker string) bool {
	return r != nil && r.underlyings[strings.ToUpper(ticker)]
}

// SetOptionsRouter wires an options router into the scheduler. Called by the
// options-mode setup (issue #418); nil leaves options-ORB disabled.
func (s *ORBScheduler) SetOptionsRouter(r *OptionsRouter) { s.optionsRouter = r }

// routeOptionsEntry executes one breakout side as a long option: run the router,
// and on a placed order persist the orb_trades row with full contract identity
// plus the UNDERLYING stop/target levels (the #415 app-managed exit watcher reads
// those to close the option).
//
// It returns (orderID, line): orderID is the broker entry order ID when an order
// was placed (""" otherwise), and line is a TUI display string. The caller MUST
// record orderID into the session's orderIDs map so the re-entry guard fires —
// otherwise a fresh entry is placed every tick (options have no broker bracket to
// mark the ticker as entered). Aborts and broker failures are logged, never fatal.
func (s *ORBScheduler) routeOptionsEntry(ctx context.Context, sessionID, ticker, side string, params BracketParams) (orderID, line string) {
	res := s.optionsRouter.Route(ctx, params, s.now())
	if !res.Placed {
		slog.Info("orb options: entry not placed",
			"ticker", ticker, "side", side, "gate", res.Decision.Gate, "reason", res.Reason,
			"iv_rank", res.Decision.IVRank, "iv_rank_ok", res.Decision.IVRankOK,
			"delta", res.Decision.Delta, "dte", res.Decision.DTE, "spread_pct", res.Decision.SpreadPct)
		return "", ""
	}
	orderID = res.Order.BrokerOrderID
	oc := res.Contract
	expiry := oc.Expiry
	dbID, err := store.CreateORBTrade(ctx, s.db, store.ORBTrade{
		SessionID:  sessionID,
		Ticker:     ticker,
		Side:       side,
		EntryStop:  params.EntryStop, // UNDERLYING levels — the #415 exit watcher reads these
		StopLoss:   params.StopLoss,
		TakeProfit: params.TakeProfit,
		Qty:        res.Contracts,
		OrderID:    orderID,
		Status:     "pending",
		AssetClass: "option",
		Underlying: oc.Underlying,
		Expiry:     &expiry,
		Strike:     oc.Strike,
		Right:      oc.Right,
		OCCSymbol:  oc.OCCSymbol,
		Multiplier: oc.Multiplier,
	})
	if err != nil {
		// The order is LIVE but unpersisted. Do NOT track it with an empty dbID
		// (that hides it from the exit watcher, which keys on the DB row). Surface
		// it loudly so the broker-truth reconciler (#416) or an operator manages it.
		// Still return orderID so the re-entry guard fires and we never re-buy.
		slog.Error("orb options: LIVE order not persisted — unmanaged until reconciled",
			"ticker", ticker, "side", side, "occ", oc.OCCSymbol, "order_id", orderID, "err", err)
		return orderID, fmt.Sprintf("  %s %s: %s x%d @ $%.2f (UNPERSISTED — see logs)",
			strings.ToUpper(side), ticker, oc.OCCSymbol, res.Contracts, res.Mid)
	}
	s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
		OrderID: orderID, Ticker: ticker, Side: side, Status: "pending",
	})
	s.trackedTrades[orderID] = &trackedTrade{
		dbID: dbID, side: side, ticker: ticker, qty: res.Contracts, entryStop: params.EntryStop,
		isOption: true, // the entry fill is an option premium — skip the underlying-level sanitizer
	}
	return orderID, fmt.Sprintf("  %s %s: %s x%d @ $%.2f (Δ%.2f IVR%.1f DTE%d)",
		strings.ToUpper(side), ticker, oc.OCCSymbol, res.Contracts, res.Mid,
		res.Decision.Delta, res.Decision.IVRank, res.Decision.DTE)
}

// OptionRouteResult is the outcome of routing one breakout side.
type OptionRouteResult struct {
	Placed    bool
	Decision  options.Decision         // the gate decision (carries observed values / aborting gate)
	Contracts int                      // contracts ordered (when Placed)
	Contract  execution.OptionContract // the chosen contract (when Placed)
	Mid       float64                  // limit price used
	Order     execution.OrderResult    // broker result (when Placed)
	Reason    string                   // why not placed (gate abort, sizing, or broker error)
}

// Route runs the gates, sizes by premium, and places the option order. It never
// returns an error — a not-placed outcome (gate abort, budget too small, broker
// failure) is reported via OptionRouteResult.Placed=false and Reason, so a single
// field decides whether anything was sent.
func (r *OptionsRouter) Route(ctx context.Context, params BracketParams, asOf time.Time) OptionRouteResult {
	dec := r.gates.Select(ctx, options.Signal{
		Underlying: params.Ticker,
		Direction:  params.Side,
		AsOf:       asOf,
	})
	if !dec.Accepted || dec.Contract == nil {
		return OptionRouteResult{Placed: false, Decision: dec, Reason: dec.Gate + ": " + dec.Reason}
	}

	mult := dec.Contract.Multiplier
	if mult <= 0 {
		mult = 100
	}
	costPerContract := dec.Mid * float64(mult)
	if costPerContract <= 0 {
		return OptionRouteResult{Placed: false, Decision: dec, Reason: "non-positive contract cost"}
	}
	contracts := int(math.Floor(r.premiumBudget / costPerContract))
	// For a long option, max loss == premium paid == contracts × cost, so the
	// max-loss cap is a second bound on the same quantity.
	if r.maxLossPerTrade > 0 {
		if capC := int(math.Floor(r.maxLossPerTrade / costPerContract)); capC < contracts {
			contracts = capC
		}
	}
	if contracts < 1 {
		return OptionRouteResult{Placed: false, Decision: dec,
			Reason: fmt.Sprintf("premium budget $%.0f too small for one %s contract at mid $%.2f", r.premiumBudget, dec.Contract.OCCSymbol, dec.Mid)}
	}

	contract := *dec.Contract
	res, err := r.placer.PlaceOrder(ctx, execution.OrderRequest{
		Ticker:     params.Ticker, // underlying, for reference/logging
		SecType:    "OPT",
		Side:       execution.OrderSideBuy, // long-only: buy the call or the put
		Type:       execution.OrderTypeLimit,
		Quantity:   float64(contracts),
		LimitPrice: dec.Mid,
		Option:     &contract,
	})
	if err != nil {
		return OptionRouteResult{Placed: false, Decision: dec, Reason: "broker placement failed: " + err.Error()}
	}
	return OptionRouteResult{
		Placed:    true,
		Decision:  dec,
		Contracts: contracts,
		Contract:  contract,
		Mid:       dec.Mid,
		Order:     res,
	}
}
