package orb

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

const (
	// orphanPositionCheckInterval throttles the broker-position orphan detector.
	// Positions change far more slowly than working orders, so a 2-minute cadence
	// is ample and keeps the extra GetPositions call cheap.
	orphanPositionCheckInterval = 2 * time.Minute
	// orphanPositionTolerance is the share tolerance when comparing broker signed
	// quantity to the summed managed quantity. ORB quantities are whole units, so
	// anything above a half-unit is real drift, not rounding.
	orphanPositionTolerance = 0.5
)

// detectOrphanedPositions is a DETECTION-ONLY reconciler: it compares the
// broker's open positions to the ORB trades the engine believes it is managing
// (status='filled') and alerts on any broker position with no — or insufficient —
// managing row. It places no orders and mutates nothing.
//
// This is the gap the July metals incident fell through: two IBKR metals shorts
// filled but, because the positions endpoint 404'd for months (fixed in #396),
// fill verification failed and their orb_trades rows were marked 'cancelled'.
// Every existing ORB reconciler (orphaned-leg, done-session, bar-replay) guards
// on status='filled', so a 'cancelled' row is invisible to all of them — the
// positions drifted unmanaged and unprotected for days. This detector keys on
// the BROKER's position, not the DB status, so it catches exactly that shape:
// something is held that no filled row explains.
//
// Forex/metals mode only. Comparison keys on ticker, and IBKRBroker.reverseConid
// resolves only forex/metals conids (the static ibkrConids map + the dynamic
// conidCache) — statically-mapped futures conids (ES/GC/…) fall through to the
// raw conid, so GetPositions would return a numeric ticker that never matches
// the symbol in orb_trades and every managed futures position would look
// orphaned (Codex #397). We therefore scope the comparison to this scheduler's
// own forex/metals instruments (forexOnlyPositions): on a shared IBKR broker
// (orb-newyork runs a forex and a futures scheduler on one broker) the futures
// scheduler's positions are ignored here and managed by its own brackets. Paper
// equities has no broker positions either way. Alerts fire once per ticker until
// the drift resolves, then re-arm.
func (s *ORBScheduler) detectOrphanedPositions(ctx context.Context) {
	if s.forexCfg == nil || !s.forexCfg.Enabled || s.db == nil || s.executor == nil {
		return
	}
	if time.Since(s.lastOrphanPosCheckAt) < orphanPositionCheckInterval {
		return
	}
	s.lastOrphanPosCheckAt = s.now()

	positions, err := s.executor.GetPositions(ctx)
	if err != nil {
		slog.Warn("orb: orphan detector: get positions failed", "err", err)
		return
	}
	positions = s.forexOnlyPositions(positions)

	filled, err := store.GetOpenFilledORBTrades(ctx, s.db)
	if err != nil {
		slog.Warn("orb: orphan detector: load filled trades failed", "err", err)
		return
	}

	findings := findOrphanedPositions(positions, filled, orphanPositionTolerance)
	current := make(map[string]struct{}, len(findings))
	for _, f := range findings {
		current[f.Ticker] = struct{}{}
		if _, already := s.alertedOrphanPositions[f.Ticker]; already {
			continue // still orphaned — alerted already, don't spam
		}
		s.alertedOrphanPositions[f.Ticker] = struct{}{}

		slog.Error("orb: orphan detector: unmanaged broker position — no stop/TP protection",
			"ticker", f.Ticker,
			"broker_qty", f.BrokerQty,
			"managed_qty", f.ManagedQty,
			"detail", f.Detail,
		)
		s.notify(ctx, fmt.Sprintf(
			"⚠️ *ORB unmanaged position*: `%s` broker=%.0f, %s.\nNo engine stop/TP — flatten manually or investigate.",
			f.Ticker, f.BrokerQty, f.Detail,
		))
	}

	// Re-arm tickers whose drift has resolved, so a future recurrence alerts again.
	for ticker := range s.alertedOrphanPositions {
		if _, still := current[ticker]; !still {
			delete(s.alertedOrphanPositions, ticker)
			slog.Info("orb: orphan detector: position drift resolved", "ticker", ticker)
		}
	}
}

// forexOnlyPositions filters broker positions to this scheduler's configured
// forex/metals instruments. On a shared IBKR broker, a concurrent futures
// scheduler (orb-newyork) holds its own positions whose conids do NOT reverse-
// resolve to symbols — GetPositions returns them as raw numeric conids — so an
// unfiltered orphan/reconcile comparison would treat every managed futures
// position as unexplained and fire false alerts. The detector was always
// documented forex/metals-only; this enforces it.
func (s *ORBScheduler) forexOnlyPositions(positions []execution.Position) []execution.Position {
	scoped := make([]execution.Position, 0, len(positions))
	for _, p := range positions {
		if s.isForexInstrument(p.Ticker) {
			scoped = append(scoped, p)
		}
	}
	return scoped
}

// orphanFinding is one unmanaged broker position surfaced by the detector.
type orphanFinding struct {
	Ticker     string
	BrokerQty  float64 // signed
	ManagedQty float64 // signed, summed over filled ORB rows
	Detail     string
}

// findOrphanedPositions is the pure comparison core: given the broker's
// positions and the ORB trades believed managed (status='filled'), return every
// broker position the managed rows don't fully explain. Deterministic order.
//
// Detected shapes: clean orphan (no managed row), partial (broker holds more
// than the summed managed quantity), and side flip (broker short while the DB
// records a long, or vice versa) — the most dangerous drift.
func findOrphanedPositions(positions []execution.Position, filled []store.ORBTrade, tolerance float64) []orphanFinding {
	managed := make(map[string]float64, len(filled))
	for _, t := range filled {
		q := float64(t.Qty)
		if strings.EqualFold(t.Side, "sell") {
			q = -q
		}
		managed[t.Ticker] += q
	}

	brokerQty := make(map[string]float64, len(positions))
	for _, p := range positions {
		if p.Ticker == "" || p.Quantity == 0 {
			continue
		}
		brokerQty[p.Ticker] += p.Quantity
	}

	tickers := make([]string, 0, len(brokerQty))
	for t := range brokerQty {
		tickers = append(tickers, t)
	}
	sort.Strings(tickers)

	var out []orphanFinding
	for _, ticker := range tickers {
		bq := brokerQty[ticker]
		mq := managed[ticker]
		residue := math.Abs(bq) - math.Abs(mq)
		sideFlip := mq != 0 && (bq < 0) != (mq < 0)
		if residue <= tolerance && !sideFlip {
			continue
		}
		detail := "no filled ORB trade explains it"
		if mq != 0 {
			detail = fmt.Sprintf("managed=%.0f — %.0f unit(s) unaccounted", mq, math.Abs(residue))
		}
		out = append(out, orphanFinding{Ticker: ticker, BrokerQty: bq, ManagedQty: mq, Detail: detail})
	}
	return out
}
