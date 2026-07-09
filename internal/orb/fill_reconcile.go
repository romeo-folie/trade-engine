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
// Forex/metals only and opt-in via forex_orb.reconcile_orphan_fills, because it
// mutates real-money order/trade state from position inference.
func (s *ORBScheduler) reconcileOrphanFills(ctx context.Context) {
	if s.forexCfg == nil || !s.forexCfg.Enabled || !s.forexCfg.ReconcileOrphanFills || s.db == nil || s.executor == nil {
		return
	}
	if time.Since(s.lastOrphanFillReconcile) < orphanFillReconcileInterval {
		return
	}
	s.lastOrphanFillReconcile = s.now()

	positions, err := s.executor.GetPositions(ctx)
	if err != nil {
		slog.Warn("orb: fill reconcile: get positions failed", "err", err)
		return
	}
	filled, err := store.GetOpenFilledORBTrades(ctx, s.db)
	if err != nil {
		slog.Warn("orb: fill reconcile: load filled trades failed", "err", err)
		return
	}
	pending, err := store.GetOpenPendingORBTrades(ctx, s.db)
	if err != nil {
		slog.Warn("orb: fill reconcile: load pending trades failed", "err", err)
		return
	}

	// Reuse the detector's residue math to find positions no filled row explains,
	// then try to attribute each to a pending entry.
	for _, f := range findOrphanedPositions(positions, filled, orphanPositionTolerance) {
		entry := matchOrphanEntry(f, pending, orphanPositionTolerance)
		if entry == nil {
			continue // no pending entry explains it — leave it for the alerting detector
		}
		fillPrice := math.Abs(avgCostFor(positions, f.Ticker))

		slog.Warn("orb: fill reconcile: recording orphaned entry fill from broker truth",
			"ticker", f.Ticker, "side", entry.Side, "qty", entry.Qty,
			"broker_qty", f.BrokerQty, "fill_price", fillPrice, "order_id", entry.OrderID)

		// Record the fill (pending → filled). persistEntryFill is a pure DB update
		// keyed on order_id, so the broker's inability to find the order can't
		// block it; once filled, the DB-backed managers (adaptive stops, time
		// exits, trades-feed exit reconcile) take over.
		s.persistEntryFill(ctx, f.Ticker, entry.Side, entry.OrderID, fillPrice)
		s.clearEntryID(f.Ticker, entry.OrderID)

		// Cancel the opposing OCO leg so a live resting stop-entry can't fill into
		// a reverse position now that this side is committed.
		if opp := opposingPendingLeg(f.Ticker, entry.Side, pending); opp != nil {
			s.cancelReconciledOpposingLeg(ctx, opp)
		}

		s.notify(ctx, fmt.Sprintf(
			"✅ *ORB fill recovered*: `%s` %s %d recorded from broker truth (fill ~%.4f). Engine now manages the position; opposing leg cancelled.",
			f.Ticker, strings.ToUpper(entry.Side), entry.Qty, fillPrice,
		))
	}
}

// cancelReconciledOpposingLeg cancels the opposing OCO entry at the broker
// (best-effort) and marks the DB row cancelled. A broker cancel failure is
// logged but does not block the DB update — the opposing row must not linger
// 'pending' once the winner is committed.
func (s *ORBScheduler) cancelReconciledOpposingLeg(ctx context.Context, opp *store.ORBTrade) {
	if err := CancelOrder(ctx, s.executor, opp.OrderID); err != nil {
		slog.Warn("orb: fill reconcile: cancel opposing leg failed (may already be gone)",
			"ticker", opp.Ticker, "side", opp.Side, "order_id", opp.OrderID, "err", err)
	}
	if err := store.CancelORBTradeByOrderID(ctx, s.db, opp.OrderID); err != nil {
		slog.Error("orb: fill reconcile: mark opposing cancelled failed",
			"ticker", opp.Ticker, "side", opp.Side, "err", err)
		return
	}
	s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
		OrderID: opp.OrderID, Ticker: opp.Ticker, Side: opp.Side, Status: "cancelled",
	})
	s.clearEntryID(opp.Ticker, opp.OrderID)
}

// matchOrphanEntry finds the pending ORB entry a broker-truth orphan finding
// most likely represents: same ticker, side matching the position's sign
// (short position ⇒ 'sell' entry), and quantity within tolerance of the
// unaccounted residue. Returns nil when nothing matches (a true orphan).
func matchOrphanEntry(f orphanFinding, pending []store.ORBTrade, tolerance float64) *store.ORBTrade {
	residue := math.Abs(f.BrokerQty) - math.Abs(f.ManagedQty)
	wantSell := f.BrokerQty < 0
	for i := range pending {
		t := &pending[i]
		if t.Ticker != f.Ticker {
			continue
		}
		if strings.EqualFold(t.Side, "sell") != wantSell {
			continue
		}
		if math.Abs(float64(t.Qty)-residue) <= tolerance {
			return t
		}
	}
	return nil
}

// opposingPendingLeg returns the still-pending entry on the opposite side of the
// same ticker (the OCO leg to cancel once the winner is confirmed), or nil.
func opposingPendingLeg(ticker, filledSide string, pending []store.ORBTrade) *store.ORBTrade {
	for i := range pending {
		t := &pending[i]
		if t.Ticker == ticker && !strings.EqualFold(t.Side, filledSide) {
			return t
		}
	}
	return nil
}

// avgCostFor returns the broker's average cost for a ticker's position, or 0.
func avgCostFor(positions []execution.Position, ticker string) float64 {
	for _, p := range positions {
		if p.Ticker == ticker {
			return p.AvgCostBasis
		}
	}
	return 0
}
