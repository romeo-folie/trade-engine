package orb

import (
	"context"
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
	active     bool               // forex/metals scheduler with a positions read attempted
	readFailed bool               // positions read failed → fail CLOSED against fabrication
	net        map[string]float64 // ticker → signed broker position
	managed    map[string]int     // "ticker|side" → total open (filled) qty at pass start
	claimed    map[string]int     // "ticker|side" → qty already approved-to-close THIS pass
}

func managedKey(ticker, side string) string { return ticker + "|" + strings.ToLower(side) }

// newExitGuard reads live positions once and precomputes the managed open qty per side.
// Disabled (allows all) for non-forex schedulers so futures/options exit paths are untouched;
// fails closed (blocks all) when the positions OR the managed-set read fails.
//
// The managed budget is built from ALL open filled ORB trades ACCOUNT-WIDE, not just the
// caller's per-session slice: the broker position read is account-wide, so a same-ticker/side
// position still open in ANOTHER session must be counted too — otherwise this session's
// genuine close is blocked, its qty wrongly attributed to the cross-session held leg (Codex P1).
func (s *ORBScheduler) newExitGuard(ctx context.Context) *exitGuard {
	if s.forexCfg == nil || !s.forexCfg.Enabled || s.executor == nil || s.db == nil {
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
	positions = s.forexOnlyPositions(positions)
	net := make(map[string]float64)
	for _, p := range positions {
		net[p.Ticker] += p.Quantity
	}
	managed := make(map[string]int)
	for i := range filled {
		if filled[i].Status == "filled" {
			managed[managedKey(filled[i].Ticker, filled[i].Side)] += filled[i].Qty
		}
	}
	return &exitGuard{active: true, net: net, managed: managed, claimed: make(map[string]int)}
}

// allows reports whether trade t's close may be recorded, and — when it returns true —
// CLAIMS t's quantity against the per-side budget so a later same-side candidate in the same
// pass is checked against the REMAINING open qty. Without this, two equal-qty same-side
// pyramided legs would both pass the identical static check even though the broker reduced by
// only one (re-approving the very phantom this guards against). A disabled guard allows; a
// failed read blocks (fail closed).
func (g *exitGuard) allows(t *store.ORBTrade) bool {
	if !g.active {
		return true
	}
	if g.readFailed {
		return false
	}
	key := managedKey(t.Ticker, t.Side)
	remaining := g.managed[key] - g.claimed[key]
	if !exitConsistentWithBroker(t.Side, g.net[t.Ticker], remaining, t.Qty) {
		return false
	}
	g.claimed[key] += t.Qty
	return true
}
