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

type orbRowHealthFinding struct {
	TradeID string
	Ticker  string
	Code    string
	Detail  string
}

func findORBRowHealth(rows []store.ORBTrade, ownerScope, assetClass string) []orbRowHealthFinding {
	var out []orbRowHealthFinding
	for _, t := range rows {
		if !store.IsValidORBOwnerScope(t.OwnerScope) {
			// Invalid ownership is global state. Assign it deterministically to
			// the owner this row's persisted asset/ticker would otherwise use so
			// concurrent schedulers do not emit duplicate alerts.
			inferredOwner := store.DefaultORBOwnerScope("", t.AssetClass, t.Ticker)
			if inferredOwner == "" && t.AssetClass == "futures" {
				inferredOwner = store.ORBOwnerFuturesIndex // one deterministic global auditor
			}
			if inferredOwner != ownerScope {
				continue
			}
			out = append(out, orbRowHealthFinding{t.ID, t.Ticker, "owner_scope_invalid", fmt.Sprintf("owner_scope=%q", t.OwnerScope)})
			continue
		}
		if t.OwnerScope != ownerScope {
			continue
		}
		if t.FillPrice <= 0 || t.Qty <= 0 || (t.Side != "buy" && t.Side != "sell") {
			out = append(out, orbRowHealthFinding{t.ID, t.Ticker, "entry_economics_invalid",
				fmt.Sprintf("side=%q fill_price=%v qty=%d", t.Side, t.FillPrice, t.Qty)})
		}
		if t.AssetClass != assetClass {
			out = append(out, orbRowHealthFinding{t.ID, t.Ticker, "asset_owner_mismatch",
				fmt.Sprintf("asset_class=%q owner_scope=%q", t.AssetClass, t.OwnerScope)})
		}
		if strings.HasPrefix(ownerScope, "futures_") && t.PointValue <= 0 {
			out = append(out, orbRowHealthFinding{t.ID, t.Ticker, "point_value_missing", "futures close is blocked until point_value is repaired"})
		}
		if strings.HasPrefix(ownerScope, "futures_") && t.ConID == "" {
			out = append(out, orbRowHealthFinding{t.ID, t.Ticker, "conid_missing", "deconfigured-contract recovery may require broker conid"})
		}
	}
	return out
}

// detectUnhealthyORBRows surfaces DB-only stale/orphan rows even when the broker
// is flat. The position-driven orphan detector cannot see that shape.
func (s *ORBScheduler) detectUnhealthyORBRows(ctx context.Context) {
	if s.db == nil || time.Since(s.lastRowHealthCheckAt) < orphanPositionCheckInterval {
		return
	}
	s.lastRowHealthCheckAt = s.now()
	rows, err := store.GetOpenFilledORBTrades(ctx, s.db)
	if err != nil {
		slog.Warn("orb: row health: load filled trades failed", "err", err)
		return
	}
	findings := findORBRowHealth(rows, s.ownerScope(), s.tradeAssetClass())
	current := make(map[string]struct{}, len(findings))
	for _, f := range findings {
		key := f.TradeID + "|" + f.Code
		current[key] = struct{}{}
		if _, seen := s.alertedRowHealth[key]; seen {
			continue
		}
		s.alertedRowHealth[key] = struct{}{}
		slog.Error("orb: filled-row identity is unhealthy — automatic close deferred",
			"trade_id", f.TradeID, "ticker", f.Ticker, "code", f.Code, "detail", f.Detail)
		s.notify(ctx, fmt.Sprintf("⚠️ *ORB row health*: `%s` (%s) — %s", f.Ticker, f.Code, f.Detail))
	}
	for key := range s.alertedRowHealth {
		if _, still := current[key]; !still {
			delete(s.alertedRowHealth, key)
		}
	}
}

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
	filled = s.ownedTradeRows(filled)
	positions = normalizeLegacyPositionIdentities(positions, filled)

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

// normalizeLegacyPositionIdentities aligns broker positions with pre-migration
// rows that lack ConID without dropping truly unmanaged positions. The orphan
// detector must retain unmatched broker truth; filtering belongs only in
// owner-specific lifecycle reconcilers.
func normalizeLegacyPositionIdentities(positions []execution.Position, trades []store.ORBTrade) []execution.Position {
	legacyTickers := make(map[string]struct{})
	for _, t := range trades {
		if t.ConID == "" {
			legacyTickers[t.Ticker] = struct{}{}
		}
	}
	out := append([]execution.Position(nil), positions...)
	for i := range out {
		if _, legacy := legacyTickers[out[i].Ticker]; legacy {
			out[i].ConID = ""
		}
	}
	return out
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

// positionsForOwnedTrades scopes broker truth to the durable rows being
// reconciled, rather than today's entry configuration. ConID is preferred for
// deconfigured contracts whose broker position no longer reverse-resolves to a
// symbol after restart; ticker remains the legacy/configured fallback.
func positionsForOwnedTrades(positions []execution.Position, trades []store.ORBTrade) []execution.Position {
	legacyTickers := make(map[string]struct{}, len(trades))
	conids := make(map[string]string, len(trades))
	for _, t := range trades {
		if t.ConID != "" {
			conids[t.ConID] = t.Ticker
		} else {
			legacyTickers[t.Ticker] = struct{}{}
		}
	}
	out := make([]execution.Position, 0, len(positions))
	for _, p := range positions {
		brokerConID := p.ConID
		if brokerConID == "" {
			brokerConID = p.Ticker // legacy IBKR fakes/raw unresolved positions
		}
		if ticker, ok := conids[brokerConID]; ok {
			p.Ticker = ticker
			p.ConID = brokerConID
			out = append(out, p)
			continue
		}
		if _, ok := legacyTickers[p.Ticker]; ok {
			// Legacy rows are keyed by ticker because they have no durable ConID.
			// Normalize the broker position to the same identity; otherwise callers
			// key it by the broker-supplied ConID and fail to see the held position.
			p.ConID = ""
			out = append(out, p)
		}
	}
	return out
}

// orphanFinding is one unmanaged broker position surfaced by the detector.
type orphanFinding struct {
	Ticker     string
	ConID      string
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
		managed[positionIdentity(t.Ticker, t.ConID)] += q
	}

	brokerQty := make(map[string]float64, len(positions))
	brokerIdentity := make(map[string]execution.Position, len(positions))
	for _, p := range positions {
		if p.Ticker == "" || p.Quantity == 0 {
			continue
		}
		key := positionIdentity(p.Ticker, p.ConID)
		brokerQty[key] += p.Quantity
		brokerIdentity[key] = p
	}

	identities := make([]string, 0, len(brokerQty))
	for identity := range brokerQty {
		identities = append(identities, identity)
	}
	sort.Strings(identities)

	var out []orphanFinding
	for _, identity := range identities {
		position := brokerIdentity[identity]
		bq := brokerQty[identity]
		mq := managed[identity]
		residue := math.Abs(bq) - math.Abs(mq)
		sideFlip := mq != 0 && (bq < 0) != (mq < 0)
		if residue <= tolerance && !sideFlip {
			continue
		}
		detail := "no filled ORB trade explains it"
		if mq != 0 {
			detail = fmt.Sprintf("managed=%.0f — %.0f unit(s) unaccounted", mq, math.Abs(residue))
		}
		out = append(out, orphanFinding{Ticker: position.Ticker, ConID: position.ConID, BrokerQty: bq, ManagedQty: mq, Detail: detail})
	}
	return out
}
