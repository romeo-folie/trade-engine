package orb

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// Options-ORB reconciliation (issue #416): a dedicated OCC-keyed orphan detector.
// The nine existing reconcilers key on equity/forex tickers and are scoped out of
// options (the ORB ones are forex-scoped; the shared readers exclude asset_class
// 'option'; the engine reconciler skips OCC symbols). This one owns options: it
// alerts when the broker holds an option position no managed trade explains — the
// same detection-only guarantee as the forex orphan detector, keyed on OCC.

const optionOrphanCheckInterval = 60 * time.Second

// findOrphanOptionPositions returns the OCC symbols of broker option positions that
// no managed (pending/filled) option trade explains, de-duplicated and sorted.
func findOrphanOptionPositions(brokerOCCs []string, managed map[string]bool) []string {
	seen := make(map[string]bool)
	var orphans []string
	for _, occ := range brokerOCCs {
		if occ == "" || managed[occ] || seen[occ] {
			continue
		}
		seen[occ] = true
		orphans = append(orphans, occ)
	}
	sort.Strings(orphans)
	return orphans
}

// detectOrphanOptionPositions is the per-tick options orphan sweep. Detection-only
// (it never places or cancels orders). No-op unless options-ORB is active. Alerts
// are de-duplicated per OCC and re-armed once the position resolves.
func (s *ORBScheduler) detectOrphanOptionPositions(ctx context.Context) {
	if s.optionExit == nil && s.optionsRouter == nil {
		return // options-ORB not active
	}
	if s.executor == nil || s.db == nil {
		return
	}
	if time.Since(s.lastOptionOrphanCheckAt) < optionOrphanCheckInterval {
		return
	}
	s.lastOptionOrphanCheckAt = s.now()

	positions, err := s.executor.GetPositions(ctx)
	if err != nil {
		slog.Warn("orb options: orphan detector positions read failed", "err", err)
		return
	}
	var brokerOCCs []string
	for _, p := range positions {
		switch {
		case p.Option != nil && p.Option.OCCSymbol != "":
			brokerOCCs = append(brokerOCCs, p.Option.OCCSymbol)
		case execution.IsOCCSymbol(p.Ticker):
			brokerOCCs = append(brokerOCCs, p.Ticker)
		}
	}
	if len(brokerOCCs) == 0 {
		s.alertedOrphanOptions = nil // no option positions → clear/re-arm all alerts
		return
	}

	managed, err := store.GetManagedOptionOCCSymbols(ctx, s.db)
	if err != nil {
		slog.Error("orb options: orphan detector managed-set read failed", "err", err)
		return
	}
	orphans := findOrphanOptionPositions(brokerOCCs, managed)

	active := make(map[string]struct{}, len(orphans))
	for _, occ := range orphans {
		active[occ] = struct{}{}
		if _, alerted := s.alertedOrphanOptions[occ]; alerted {
			continue // already alerted; don't spam
		}
		oc, _ := execution.ParseOCCSymbol(occ)
		msg := fmt.Sprintf("ORB options ORPHAN: broker holds %s (%s %s %.2f exp %s) with no managed trade",
			occ, oc.Underlying, oc.Right, oc.Strike, oc.Expiry.Format("2006-01-02"))
		slog.Warn("orb options: orphan option position",
			"occ", occ, "underlying", oc.Underlying, "right", oc.Right, "strike", oc.Strike,
			"expiry", oc.Expiry.Format("2006-01-02"))
		if s.notifier != nil {
			if nerr := s.notifier.Notify(ctx, msg); nerr != nil {
				slog.Warn("orb options: orphan notify failed", "err", nerr)
			}
		}
	}
	s.alertedOrphanOptions = active // re-arm alerts for positions that resolved
}
