package orb

import (
	"context"
	"log/slog"
	"time"

	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// splitOrphanInterval throttles the orphaned-split-entry sweep.
const splitOrphanInterval = time.Minute

// cancelOrphanedSplitEntries cancels a split-bracket entry that never filled once another
// leg of its logical trade has already exited. Both legs of a split enter on the same
// breakout trigger, so the OCO block deliberately leaves a not-yet-filled winning-side entry
// working (it normally fills a moment later). But if that entry genuinely doesn't fill (thin
// liquidity at the trigger) and its filled sibling then closes (TP / stop / time exit), the
// pending entry is orphaned: a later retest of the breakout would fill it and open a fresh
// position AFTER the logical trade ended. No exit path cancels it, so this sweep does — well
// before the end-of-session cleanup it would otherwise wait for.
//
// A no-op unless a split pair was placed this session (splitPlaced) and an entry is still
// pending. Throttled.
func (s *ORBScheduler) cancelOrphanedSplitEntries(ctx context.Context) {
	if !s.splitPlaced || s.db == nil || s.executor == nil {
		return
	}
	// Pending entry orders still being polled (a filled entry is cleared from these maps by
	// the OCO block, so anything here has not been recorded as filled).
	pending := make(map[string]string) // entry order ID → ticker
	for _, m := range []map[string][2]string{s.orderIDs, s.scaleOutEntryIDs} {
		for ticker, ids := range m {
			for _, id := range ids {
				if id != "" {
					pending[id] = ticker
				}
			}
		}
	}
	if len(pending) == 0 {
		return
	}
	if time.Since(s.lastSplitOrphanCheckAt) < splitOrphanInterval {
		return
	}
	s.lastSplitOrphanCheckAt = s.now()

	s.mu.Lock()
	var sessionID string
	if s.session != nil {
		sessionID = s.session.ID
	}
	s.mu.Unlock()
	if sessionID == "" {
		return
	}

	trades, err := store.GetORBTrades(ctx, s.db, sessionID)
	if err != nil {
		slog.Warn("orb: split orphan: load trades failed", "err", err)
		return
	}

	for _, orderID := range orphanedSplitEntries(pending, trades) {
		ticker := pending[orderID]
		// Guard: if the entry has in fact filled — fully OR partially — at the broker, it is a
		// REAL position, not an orphaned pending entry. Skip it: cancelling here would cancel
		// the live remainder and (worse) mark the DB row cancelled, hiding the filled exposure
		// from exit reconciliation. The OCO / exit reconcilers own a filled position.
		if res, err := s.executor.GetOrderStatus(ctx, orderID); err == nil &&
			(res.Status == "filled" || res.Status == "partial") {
			continue
		}
		slog.Info("orb: cancelling orphaned split entry — a sibling leg already exited",
			"ticker", ticker, "order_id", orderID)
		if err := CancelOrder(ctx, s.executor, orderID); err != nil {
			slog.Warn("orb: split orphan: cancel failed", "ticker", ticker, "order_id", orderID, "err", err)
			continue
		}
		if dbErr := store.CancelORBTradeByOrderID(ctx, s.db, orderID); dbErr != nil {
			slog.Error("orb: split orphan: mark cancelled failed", "ticker", ticker, "order_id", orderID, "err", dbErr)
		}
		s.publish(events.ORBTradeUpdate, events.ORBTradeUpdatePayload{
			OrderID: orderID, Ticker: ticker, Status: "cancelled",
		})
		s.clearEntryID(ticker, orderID)
	}
}

// orphanedSplitEntries returns the pending entry order IDs that should be cancelled: split
// legs (scale_out_role set) whose logical group already has a closed (exited) leg. Pure, so
// the group-membership decision is unit-testable without a broker or DB.
func orphanedSplitEntries(pending map[string]string, trades []store.ORBTrade) []string {
	keyOf := func(t store.ORBTrade) string {
		if t.ScaleOutParentID != nil && *t.ScaleOutParentID != "" {
			return *t.ScaleOutParentID
		}
		return t.ID
	}
	closedGroup := make(map[string]bool)
	rowByOrderID := make(map[string]store.ORBTrade)
	for _, t := range trades {
		if t.Status == "closed" {
			closedGroup[keyOf(t)] = true
		}
		if t.OrderID != "" {
			rowByOrderID[t.OrderID] = t
		}
	}
	var out []string
	for orderID := range pending {
		row, ok := rowByOrderID[orderID]
		if !ok || row.ScaleOutRole == "" {
			continue // not a tracked split leg
		}
		if closedGroup[keyOf(row)] {
			out = append(out, orderID)
		}
	}
	return out
}
