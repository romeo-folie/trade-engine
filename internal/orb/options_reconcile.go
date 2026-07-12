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
// alerts when the broker holds MORE option contracts than the managed trades
// explain — the same detection-only guarantee as the forex orphan detector, keyed
// on OCC and compared by QUANTITY (so a 3-contract broker position explained by a
// 1-contract row surfaces the 2-contract surplus, not "fully managed").

const optionOrphanCheckInterval = 60 * time.Second

// optionSurplus is a broker option holding the managed rows don't fully explain.
type optionSurplus struct {
	OCC        string
	BrokerQty  float64 // contracts held at the broker
	ManagedQty float64 // contracts summed over managed (pending/filled) rows
}

// findOrphanOptionPositions returns every OCC whose broker-held quantity exceeds the
// managed quantity (unmanaged or under-recorded surplus), sorted by OCC.
func findOrphanOptionPositions(brokerHeld, managed map[string]float64) []optionSurplus {
	const eps = 1e-9
	var out []optionSurplus
	for occ, bq := range brokerHeld {
		if occ == "" || bq <= 0 {
			continue
		}
		if mq := managed[occ]; bq > mq+eps {
			out = append(out, optionSurplus{OCC: occ, BrokerQty: bq, ManagedQty: mq})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OCC < out[j].OCC })
	return out
}

// detectOrphanOptionPositions is the per-tick options orphan sweep. Detection-only
// (it never places or cancels orders). No-op unless options-ORB is active. Alerts
// are de-duplicated per OCC and re-armed once the surplus resolves.
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

	// Intentionally a SEPARATE positions read from enforceOptionExits' (which runs just
	// before this on the same tick): that sweep SELLS to close, so the orphan detector
	// must see a POST-sell snapshot. Sharing the sweep's pre-sell snapshot would make a
	// position it just closed look broker-held-but-unmanaged → a false orphan alert. The
	// cost is one extra GetPositions when the 60s orphan throttle aligns with a sweep.
	brokerHeld := s.heldOptionQuantities(ctx) // long option contracts per OCC
	if brokerHeld == nil {
		return // positions read failed; try again next sweep
	}
	if len(brokerHeld) == 0 {
		s.alertedOrphanOptions = nil // no option positions → clear/re-arm all alerts
		return
	}

	managed, err := store.GetManagedOptionQuantities(ctx, s.db)
	if err != nil {
		slog.Error("orb options: orphan detector managed-set read failed", "err", err)
		return
	}
	surpluses := findOrphanOptionPositions(brokerHeld, managed)

	active := make(map[string]struct{}, len(surpluses))
	for _, sp := range surpluses {
		active[sp.OCC] = struct{}{}
		if _, alerted := s.alertedOrphanOptions[sp.OCC]; alerted {
			continue // already alerted; don't spam
		}
		oc, _ := execution.ParseOCCSymbol(sp.OCC)
		msg := fmt.Sprintf("ORB options ORPHAN: broker holds %.0f of %s (%s %s %.2f exp %s); managed rows explain only %.0f (%.0f-contract surplus)",
			sp.BrokerQty, sp.OCC, oc.Underlying, oc.Right, oc.Strike, oc.Expiry.Format("2006-01-02"), sp.ManagedQty, sp.BrokerQty-sp.ManagedQty)
		slog.Warn("orb options: orphan option position",
			"occ", sp.OCC, "underlying", oc.Underlying, "broker_qty", sp.BrokerQty, "managed_qty", sp.ManagedQty)
		if s.notifier != nil {
			if nerr := s.notifier.Notify(ctx, msg); nerr != nil {
				slog.Warn("orb options: orphan notify failed", "err", nerr)
			}
		}
	}
	s.alertedOrphanOptions = active // re-arm alerts for surpluses that resolved
}
