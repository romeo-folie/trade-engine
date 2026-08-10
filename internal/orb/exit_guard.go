package orb

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// exitConsistentWithBroker reports whether recording a close for a forex/metals trade of
// `closingQty` on `side` is consistent with broker truth — i.e. the broker has ACTUALLY
// reduced the position. `brokerNet` is the broker's signed position for the ticker (negative
// = short); `managedQtyOnSide` is the DB's total open (filled) qty on that side INCLUDING
// this trade.
//
// The rule: the position is genuinely gone only if, after removing this trade's qty, the
// broker still holds no more than the DB will show open — heldOnSide <= managed - closing.
// Today's XAG phantom: an inference closer booked a "stop" while the broker still held the
// full -850 short (heldOnSide 850, managed 850, closing 850 → 850 <= 0 is false → BLOCK).
// Gold's real TP: broker flat (heldOnSide 0, managed 22, closing 22 → 0 <= 0 → ALLOW).
func exitConsistentWithBroker(side string, brokerNet float64, managedQtyOnSide, closingQty int) bool {
	var heldOnSide float64
	switch {
	case strings.EqualFold(side, "sell") && brokerNet < 0:
		heldOnSide = -brokerNet
	case strings.EqualFold(side, "buy") && brokerNet > 0:
		heldOnSide = brokerNet
	}
	// Tolerance mirrors the orphan detector's position-compare slack (fractional lot noise).
	return heldOnSide <= float64(managedQtyOnSide-closingQty)+orphanPositionTolerance
}

// signedPositionFor sums a ticker's signed broker quantity across position rows.
func signedPositionFor(positions []execution.Position, ticker string) float64 {
	var q float64
	for _, p := range positions {
		if p.Ticker == ticker {
			q += p.Quantity
		}
	}
	return q
}

// exitGuard is a per-pass broker-truth check that prevents an inference-based exit reconciler
// (trades-feed match / bar replay) from booking a close the broker never actually made —
// the phantom that stranded today's unprotected XAG short. Built once per reconcile pass so
// the position read is shared across all candidate closes.
type exitGuard struct {
	active     bool                // forex/metals scheduler with a positions read attempted
	readFailed bool                // positions read failed → fail CLOSED against fabrication
	net        map[string]float64  // ticker → signed broker position
	managed    map[string]int      // "ticker|side" → total open (filled) qty at pass start
	claimed    map[string]int      // "ticker|side" → qty already approved-to-close THIS pass
	ambiguous  map[string]struct{} // tickers with mixed legacy/durable identity; fail closed locally
}

type exitGuardCode uint8

const (
	guardAllow exitGuardCode = iota
	guardBrokerStillHeld
	guardReadFailed
	guardInvalidManagedBudget
	guardAmbiguousIdentity
)

type exitGuardDecision struct {
	Code      exitGuardCode
	BrokerNet float64
	Managed   int
	Closing   int
}

func positionIdentity(ticker, conID string) string {
	if conID != "" {
		return "conid:" + conID
	}
	return "ticker:" + ticker
}

func managedKey(ticker, conID, side string) string {
	return positionIdentity(ticker, conID) + "|" + strings.ToLower(side)
}

// newExitGuard reads live positions once and precomputes the managed open qty per side.
// Active for forex AND futures schedulers (both place dual-sided/split brackets that the
// inference reconcilers can phantom-close); disabled (allows all) for the equities scheduler,
// which has no per-instrument position config here. Fails closed (blocks all) when the
// positions OR the managed-set read fails. Futures positions are keyed by symbol via
// reverseConid (which resolves the static/dynamic futures conids back to "MNQ" etc.) — without
// that resolution this guard would silently rubber-stamp every futures close (net=0 → ALLOW).
//
// The managed budget is built from ALL open filled ORB trades ACCOUNT-WIDE, not just the
// caller's per-session slice: the broker position read is account-wide, so a same-ticker/side
// position still open in ANOTHER session must be counted too — otherwise this session's
// genuine close is blocked, its qty wrongly attributed to the cross-session held leg (Codex P1).
func (s *ORBScheduler) newExitGuard(ctx context.Context) *exitGuard {
	if (!s.isForexMode() && !s.isFuturesMode()) || s.executor == nil || s.db == nil {
		return &exitGuard{active: false}
	}
	positions, err := s.executor.GetPositions(ctx)
	if err != nil {
		slog.Warn("orb: exit guard: positions read failed — deferring inference-based closes to avoid a phantom", "err", err)
		return &exitGuard{active: true, readFailed: true}
	}
	filled, err := store.GetOpenFilledORBTrades(ctx, s.db)
	if err != nil {
		slog.Warn("orb: exit guard: account-wide filled-trades read failed — deferring inference-based closes", "err", err)
		return &exitGuard{active: true, readFailed: true}
	}
	owned := s.ownedTradeRows(filled)
	ambiguous := mixedIdentityTickers(owned)
	positions = positionsForOwnedTrades(positions, owned)
	net := make(map[string]float64)
	for _, p := range positions {
		net[positionIdentity(p.Ticker, p.ConID)] += p.Quantity
	}
	managed := make(map[string]int)
	for i := range owned {
		if owned[i].Status == "filled" {
			managed[managedKey(owned[i].Ticker, owned[i].ConID, owned[i].Side)] += owned[i].Qty
		}
	}
	return &exitGuard{active: true, net: net, managed: managed, claimed: make(map[string]int), ambiguous: ambiguous}
}

func hasMixedLegacyAndDurableIdentity(trades []store.ORBTrade) bool {
	return len(mixedIdentityTickers(trades)) > 0
}

func mixedIdentityTickers(trades []store.ORBTrade) map[string]struct{} {
	type identityState uint8
	const (
		legacy identityState = 1 << iota
		durable
	)
	byTicker := make(map[string]identityState)
	for _, t := range trades {
		state := legacy
		if t.ConID != "" {
			state = durable
		}
		byTicker[t.Ticker] |= state
	}
	out := make(map[string]struct{})
	for ticker, state := range byTicker {
		if state == legacy|durable {
			out[ticker] = struct{}{}
		}
	}
	return out
}

// allows reports whether trade t's close may be recorded, and — when it returns true —
// CLAIMS t's quantity against the per-side budget so a later same-side candidate in the same
// pass is checked against the REMAINING open qty. Without this, two equal-qty same-side
// pyramided legs would both pass the identical static check even though the broker reduced by
// only one (re-approving the very phantom this guards against). A disabled guard allows; a
// failed read blocks (fail closed).
func (g *exitGuard) check(t *store.ORBTrade) exitGuardDecision {
	if !g.active {
		return exitGuardDecision{Code: guardAllow, Closing: t.Qty}
	}
	if g.readFailed {
		return exitGuardDecision{Code: guardReadFailed, Closing: t.Qty}
	}
	if _, ambiguous := g.ambiguous[t.Ticker]; ambiguous {
		return exitGuardDecision{Code: guardAmbiguousIdentity, Closing: t.Qty}
	}
	key := managedKey(t.Ticker, t.ConID, t.Side)
	remaining := g.managed[key] - g.claimed[key]
	d := exitGuardDecision{BrokerNet: g.net[positionIdentity(t.Ticker, t.ConID)], Managed: remaining, Closing: t.Qty}
	if remaining < t.Qty {
		d.Code = guardInvalidManagedBudget
		return d
	}
	if !exitConsistentWithBroker(t.Side, d.BrokerNet, remaining, t.Qty) {
		d.Code = guardBrokerStillHeld
		return d
	}
	g.claimed[key] += t.Qty
	d.Code = guardAllow
	return d
}

func (s *ORBScheduler) checkExitGuard(g *exitGuard, t *store.ORBTrade) exitGuardDecision {
	d := g.check(t)
	if d.Code == guardAllow {
		delete(s.alertedGuardBlocks, t.ID)
	}
	return d
}

func (s *ORBScheduler) reportGuardBlock(ctx context.Context, stage string, t *store.ORBTrade, d exitGuardDecision) {
	switch d.Code {
	case guardBrokerStillHeld:
		slog.Warn("orb: exit guard: broker still holds position — deferring close",
			"stage", stage, "ticker", t.Ticker, "side", t.Side, "qty", t.Qty,
			"broker_net", d.BrokerNet, "managed", d.Managed)
	case guardInvalidManagedBudget:
		slog.Error("orb: exit guard: row is outside managed ownership budget — deferring and alerting as orphan",
			"stage", stage, "trade_id", t.ID, "ticker", t.Ticker, "side", t.Side,
			"closing_qty", d.Closing, "managed_qty", d.Managed, "owner_scope", t.OwnerScope)
		if s.alertedGuardBlocks == nil {
			s.alertedGuardBlocks = make(map[string]struct{})
		}
		if _, alerted := s.alertedGuardBlocks[t.ID]; !alerted {
			s.alertedGuardBlocks[t.ID] = struct{}{}
			s.notify(ctx, fmt.Sprintf(
				"⚠️ *ORB orphan row*: `%s` %s %d has managed budget %d in `%s`; close deferred for investigation.",
				t.Ticker, t.Side, t.Qty, d.Managed, stage))
		}
	case guardReadFailed:
		// The read failure was logged where the guard was built. Callers only need
		// to preserve retry semantics.
	case guardAmbiguousIdentity:
		slog.Error("orb: exit guard: mixed legacy and durable contract identity — deferring affected ticker",
			"stage", stage, "trade_id", t.ID, "ticker", t.Ticker)
	}
}
