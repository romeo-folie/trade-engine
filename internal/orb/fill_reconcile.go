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
	"github.com/romeo-folie/trade-engine/internal/store"
)

// orphanFillReconcileInterval throttles the broker-truth entry reconciler.
// Positions move slowly, so the detector's 2-minute cadence is ample and keeps
// the extra GetPositions call cheap.
const orphanFillReconcileInterval = 2 * time.Minute

// reconcileCancelledLookback bounds how far back a wrongly-'cancelled' entry may
// be reopened to 'filled', so a stale cancel can't match an unrelated later
// position. A day covers same-session and overnight metals holds.
const reconcileCancelledLookback = 24 * time.Hour

// reconcileOrphanFills closes the metals-fill blind spot behind the July
// incident: a bracket entry fills at IBKR but GetOrderStatus can't confirm it
// (after the size/stop confirmation-prompt chain the parent order id ages out
// of both /iserver/account/orders and /iserver/account/trades), so the
// orb_trades row stays 'pending'. Every stop/TP/time-exit/adaptive manager
// guards on status='filled', so the position sits unmanaged and unprotected —
// exactly what the orphan detector alerts on but cannot fix.
//
// This reconciler resolves it from BROKER TRUTH: it compares live positions
// against pending entries and, when a held position matches a pending entry by
// ticker + side + quantity, records the fill (pending → filled, price from the
// broker's average cost) so the existing management machinery engages, then
// cancels the opposing OCO leg (a live resting stop-entry could otherwise fill
// into a reverse position). Positions with no matching pending entry are left
// untouched for the detection-only detector to alert on — a true orphan is
// human territory.
//
// This PROACTIVE mid-session pass is forex/metals-only and opt-in via
// forex_orb.reconcile_orphan_fills, because it mutates real-money order/trade state from
// position inference. Futures have no equivalent opt-in flag and are covered instead by the
// always-on reconcileHeldPositionsToFilled call at session cleanup (which now handles both
// modes) — the safety-critical path that prevents a filled futures entry being cancelled.
func (s *ORBScheduler) reconcileOrphanFills(ctx context.Context) {
	if s.forexCfg == nil || !s.forexCfg.Enabled || !s.forexCfg.ReconcileOrphanFills {
		return // proactive mid-session reconcile is opt-in (forex/metals only)
	}
	if time.Since(s.lastOrphanFillReconcile) < orphanFillReconcileInterval {
		return
	}
	s.lastOrphanFillReconcile = s.now()
	s.reconcileHeldPositionsToFilled(ctx)
}

// reconcileHeldPositionsToFilled is the broker-truth core, WITHOUT the opt-in
// flag or throttle. The session-cleanup path calls it directly and
// unconditionally, immediately before cancelling unfilled orders — so a filled-
// but-unconfirmed metals entry (GetOrderStatus returns "order not found") is
// recorded as filled from the held position instead of being wrongly cancelled
// (the July / today's XAG+XAU trap: cancelled at session end, then invisible to
// every status='filled' reconciler because they don't read cancelled rows).
func (s *ORBScheduler) reconcileHeldPositionsToFilled(ctx context.Context) {
	if (!s.isForexMode() && !s.isFuturesMode()) || s.db == nil || s.executor == nil {
		return
	}

	positions, err := s.executor.GetPositions(ctx)
	if err != nil {
		slog.Warn("orb: fill reconcile: get positions failed", "err", err)
		return
	}
	// Scope to this scheduler's own instruments (forex/metals OR futures). This runs
	// unconditionally at session cleanup right before the cancel loop: a filled-but-
	// unconfirmed entry (GetOrderStatus "order not found" after the confirmation-prompt
	// chain ages the parent order out) is recorded as filled from broker truth instead of
	// being wrongly cancelled. Must cover futures too — they place the same brackets and hit
	// the same not-found trap, and reverseConid now resolves their positions to symbols.
	filled, err := store.GetOpenFilledORBTrades(ctx, s.db)
	if err != nil {
		slog.Warn("orb: fill reconcile: load filled trades failed", "err", err)
		return
	}
	filled = s.ownedTradeRows(filled)
	pending, err := store.GetOpenPendingORBTrades(ctx, s.db)
	if err != nil {
		slog.Warn("orb: fill reconcile: load pending trades failed", "err", err)
		return
	}
	pending = s.ownedTradeRows(pending)
	// Also consider recently-cancelled entries: the July/today's trap is a fill
	// wrongly marked 'cancelled' at session end. When the broker still holds the
	// position, such a row is reopened to 'filled'. Bounded to a day so a stale
	// cancel can't match an unrelated new position.
	cancelled, err := store.GetRecentlyCancelledORBTrades(ctx, s.db, s.now().Add(-reconcileCancelledLookback))
	if err != nil {
		slog.Warn("orb: fill reconcile: load cancelled trades failed", "err", err)
		return
	}
	cancelled = s.ownedTradeRows(cancelled)
	identityRows := make([]store.ORBTrade, 0, len(filled)+len(pending)+len(cancelled))
	identityRows = append(identityRows, filled...)
	identityRows = append(identityRows, pending...)
	identityRows = append(identityRows, cancelled...)
	positions = positionsForOwnedTrades(positions, identityRows)
	// Reuse the detector's residue math to find positions no filled row explains,
	// then attribute each to the entry rows (pending OR wrongly-cancelled) that
	// account for it. Match pending and cancelled candidates as SEPARATE
	// single-status sets: a position that a live pending order and a stale
	// cancelled order could each explain (cross-session, same ticker/side/qty) is
	// genuinely ambiguous, so it is left for the alerting detector rather than
	// risk recording the wrong order as filled and orphaning a live stop entry.
	for _, f := range findOrphanedPositions(positions, filled, orphanPositionTolerance) {
		pendingMatch := matchOrphanEntries(f, pending, orphanPositionTolerance)
		cancelledMatch := matchOrphanEntries(f, cancelled, orphanPositionTolerance)
		if len(pendingMatch) > 0 && len(cancelledMatch) > 0 {
			slog.Warn("orb: fill reconcile: ambiguous — both a pending and a cancelled entry match; leaving for the detector",
				"ticker", f.Ticker, "broker_qty", f.BrokerQty)
			continue
		}
		entries := pendingMatch
		if len(entries) == 0 {
			entries = cancelledMatch
		}
		if len(entries) == 0 {
			continue // no entry explains it — leave it for the alerting detector
		}
		fillPrice := math.Abs(avgCostFor(positions, f.Ticker, f.ConID))

		// Record the fill on every matched row (a split bracket stores the runner
		// and partial as separate same-side rows summing to the residue). Once
		// filled, the DB-backed managers (adaptive stops, time exits, trades-feed
		// exit reconcile) engage.
		var totalQty int
		for _, e := range entries {
			slog.Warn("orb: fill reconcile: recording orphaned entry fill from broker truth",
				"ticker", f.Ticker, "side", e.Side, "qty", e.Qty, "prior_status", e.Status,
				"broker_qty", f.BrokerQty, "fill_price", fillPrice, "order_id", e.OrderID)
			s.recordReconciledFill(ctx, e, fillPrice)
			s.clearEntryID(f.Ticker, e.OrderID)
			totalQty += e.Qty
		}
		s.notify(ctx, fmt.Sprintf(
			"✅ *ORB fill recovered*: `%s` %s %d recorded from broker truth (fill ~%.4f). Engine now manages the position.",
			f.Ticker, strings.ToUpper(entries[0].Side), totalQty, fillPrice,
		))
	}

	// Cancel any stale OCO leg: a pending entry whose opposite side is now filled.
	// Driven by DB state (not positions), so it also retries on a later tick if a
	// cancel fails — a live opposing stop-entry must never linger and reverse the
	// position.
	s.cancelStaleOCOLegs(ctx)
}

// recordReconciledFill marks one matched entry row filled from broker truth,
// anchoring filled_at to the row's placement time (created_at) for BOTH the
// pending and cancelled paths — these are inferred fills discovered after the
// fact, so a NOW() stamp would grant a stale position a fresh max-hold window.
// The price is sanitized against the row's own entry stop (broker avg cost can
// be implausible). Both writes are pure DB updates keyed on order_id, so the
// broker's inability to find the order can't block them.
func (s *ORBScheduler) recordReconciledFill(ctx context.Context, e *store.ORBTrade, price float64) {
	if corrected, was := sanitizeEntryFill(e.Side, e.EntryStop, price); was {
		slog.Warn("orb: fill reconcile: implausible entry fill from broker — recording stop level instead",
			"side", e.Side, "ticker", e.Ticker, "reported", price, "entry_stop", e.EntryStop, "recorded", corrected)
		price = corrected
	}

	var updated bool
	var err error
	if strings.EqualFold(e.Status, "cancelled") {
		updated, err = store.ReopenORBTradeToFilled(ctx, s.db, e.OrderID, price)
	} else {
		updated, err = store.FillPendingORBTradeAtCreated(ctx, s.db, e.OrderID, price)
	}
	if err != nil {
		slog.Error("orb: fill reconcile: record fill failed",
			"ticker", e.Ticker, "side", e.Side, "order_id", e.OrderID, "prior_status", e.Status, "err", err)
		return
	}
	if updated {
		s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
			OrderID: e.OrderID, Ticker: e.Ticker, Side: e.Side, Status: "filled", FillPrice: price,
		})
		if tc := s.trackedTrades[e.OrderID]; tc != nil {
			tc.entryFill = price
		}
	}
}

// cancelStaleOCOLegs cancels pending ORB entries whose opposite-side sibling on
// the same ticker has already filled (the losing OCO leg). It marks the DB row
// cancelled and clears tracking ONLY after CancelOrder succeeds; a failed cancel
// leaves the row 'pending' so the next reconcile tick retries — otherwise a live
// resting stop-entry could reverse the position with nothing watching it.
func (s *ORBScheduler) cancelStaleOCOLegs(ctx context.Context) {
	filled, err := store.GetOpenFilledORBTrades(ctx, s.db)
	if err != nil {
		return
	}
	pending, err := store.GetOpenPendingORBTrades(ctx, s.db)
	if err != nil {
		return
	}
	filled = s.ownedTradeRows(filled)
	pending = s.ownedTradeRows(pending)

	filledSides := make(map[string]map[string]bool)
	for _, t := range filled {
		if filledSides[t.Ticker] == nil {
			filledSides[t.Ticker] = make(map[string]bool)
		}
		filledSides[t.Ticker][strings.ToLower(t.Side)] = true
	}

	for i := range pending {
		p := &pending[i]
		if !filledSides[p.Ticker][oppositeSide(p.Side)] {
			continue // no filled winner on the opposite side — leave it working
		}
		if err := CancelOrder(ctx, s.executor, p.OrderID); err != nil {
			slog.Warn("orb: fill reconcile: cancel stale OCO leg failed — will retry next tick",
				"ticker", p.Ticker, "side", p.Side, "order_id", p.OrderID, "err", err)
			continue // leave 'pending' so a later tick retries
		}
		if err := store.CancelORBTradeByOrderID(ctx, s.db, p.OrderID); err != nil {
			slog.Error("orb: fill reconcile: mark stale OCO leg cancelled failed",
				"ticker", p.Ticker, "side", p.Side, "err", err)
			continue
		}
		s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
			OrderID: p.OrderID, Ticker: p.Ticker, Side: p.Side, Status: "cancelled",
		})
		s.clearEntryID(p.Ticker, p.OrderID)
		slog.Info("orb: fill reconcile: cancelled stale OCO leg", "ticker", p.Ticker, "side", p.Side)
	}
}

// matchOrphanEntries returns the pending ORB entry rows that account for a
// broker-truth orphan finding: same ticker and side (short position ⇒ 'sell'),
// whose quantities together match the unaccounted residue. The whole same-side
// set is returned when its total matches (a single entry, or a split bracket's
// runner + partial summing to the residue); otherwise a lone row equal to the
// residue is returned. Empty when nothing matches (a true orphan).
func matchOrphanEntries(f orphanFinding, pending []store.ORBTrade, tolerance float64) []*store.ORBTrade {
	residue := math.Abs(f.BrokerQty) - math.Abs(f.ManagedQty)
	wantSell := f.BrokerQty < 0

	var group []*store.ORBTrade
	var total float64
	for i := range pending {
		t := &pending[i]
		if positionIdentity(t.Ticker, t.ConID) != positionIdentity(f.Ticker, f.ConID) || strings.EqualFold(t.Side, "sell") != wantSell {
			continue
		}
		group = append(group, t)
		total += float64(t.Qty)
	}
	if len(group) == 0 {
		return nil
	}
	// Whole same-side set explains the residue (single entry OR a split bracket's
	// runner + partial).
	if math.Abs(total-residue) <= tolerance {
		return group
	}
	// Fall back: exactly one row equals the residue (its sibling already managed).
	// If two equal-qty legs both match (e.g. a 0.5 split), we can't tell which one
	// the broker filled — they share ticker/side/qty but differ in TP/SL/order id —
	// so treat it as ambiguous and leave it for the alerting detector rather than
	// record the wrong bracket.
	var singles []*store.ORBTrade
	for _, t := range group {
		if math.Abs(float64(t.Qty)-residue) <= tolerance {
			singles = append(singles, t)
		}
	}
	if len(singles) == 1 {
		return singles
	}
	return nil
}

// oppositeSide returns the OCO counter-side ("buy"↔"sell").
func oppositeSide(side string) string {
	if strings.EqualFold(side, "buy") {
		return "sell"
	}
	return "buy"
}

// avgCostFor returns the broker's average cost for an exact persisted position
// identity. ConID distinguishes futures expiries that share a root ticker.
func avgCostFor(positions []execution.Position, ticker, conID string) float64 {
	for _, p := range positions {
		if positionIdentity(p.Ticker, p.ConID) == positionIdentity(ticker, conID) {
			return p.AvgCostBasis
		}
	}
	return 0
}
