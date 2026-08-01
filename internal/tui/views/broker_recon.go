package views

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// brokerReconAbsFloor is the absolute signed-quantity difference (in shares)
// below which a broker-vs-DB gap is treated as noise regardless of the relative
// tolerance. Guards against float dust on fractional-share positions.
const brokerReconAbsFloor = 0.01

// PositionDivergence is one ticker whose broker-reported net position disagrees
// with the app's open-trade book. Quantities are signed (short is negative).
type PositionDivergence struct {
	Ticker    string
	BrokerQty float64
	DBQty     float64
	Kind      string // broker_only | db_only | qty_mismatch | side_mismatch | exit_pending
}

// BrokerReconResult is the outcome of comparing live broker positions against
// the DB open-trade book. Available is false when no broker is configured or
// the live call failed (Err then explains why).
type BrokerReconResult struct {
	Available    bool
	CheckedAt    time.Time
	Err          string
	InSync       bool
	Divergences  []PositionDivergence
	PendingExits []PositionDivergence
	BrokerCount  int
	DBCount      int
}

// ReconcilePositions compares broker-reported positions against the DB open
// trades, aggregating signed net quantity per ticker (short negative), and
// returns the tickers that diverge beyond `tol` (a fraction of the larger side,
// floored at brokerReconAbsFloor). Malformed DB lots (entry or qty <= 0) are
// ignored, matching the dashboard table. Pure and deterministic — the caller
// stamps CheckedAt.
func ReconcilePositions(broker []execution.BrokerPosition, trades []store.Trade, tol float64) BrokerReconResult {
	brokerQty := make(map[string]float64)
	for _, p := range broker {
		q := math.Abs(p.Quantity)
		if p.Side == "short" {
			q = -q
		}
		brokerQty[p.Symbol] += q
	}

	dbQty := make(map[string]float64)
	for _, t := range trades {
		if t.EntryPrice <= 0 || t.Quantity <= 0 {
			continue
		}
		q := t.Quantity
		if t.Direction == "short" {
			q = -q
		}
		dbQty[t.Ticker] += q
	}

	// Union of tickers, sorted for stable output.
	seen := make(map[string]bool)
	var tickers []string
	for k := range brokerQty {
		seen[k] = true
		tickers = append(tickers, k)
	}
	for k := range dbQty {
		if !seen[k] {
			tickers = append(tickers, k)
		}
	}
	sort.Strings(tickers)

	res := BrokerReconResult{Available: true, BrokerCount: len(brokerQty), DBCount: len(dbQty)}
	for _, tk := range tickers {
		b, d := brokerQty[tk], dbQty[tk]
		if positionsMatch(b, d, tol) {
			continue
		}
		res.Divergences = append(res.Divergences, PositionDivergence{
			Ticker: tk, BrokerQty: b, DBQty: d, Kind: classifyDivergence(b, d),
		})
	}
	res.InSync = len(res.Divergences) == 0
	return res
}

// positionsMatch reports whether two signed quantities agree within tolerance.
func positionsMatch(broker, db, tol float64) bool {
	threshold := math.Max(brokerReconAbsFloor, tol*math.Max(math.Abs(broker), math.Abs(db)))
	return math.Abs(broker-db) <= threshold
}

// classifyDivergence labels a mismatch by which side holds the position and
// whether the signs (long/short) agree.
func classifyDivergence(broker, db float64) string {
	switch {
	case math.Abs(db) <= brokerReconAbsFloor:
		return "broker_only" // broker holds something the app isn't tracking
	case math.Abs(broker) <= brokerReconAbsFloor:
		return "db_only" // app thinks it holds something the broker doesn't
	case (broker > 0) != (db > 0):
		return "side_mismatch"
	default:
		return "qty_mismatch"
	}
}

// ParsePersistedRecon converts the engine's persisted reconciliation JSON (see
// engine.PersistedReconciliation / EngineStateBrokerDrift) into a
// BrokerReconResult for rendering. Used as the dashboard's fallback source when
// it cannot reconcile the broker live. A parse error yields an unavailable
// result. BrokerCount is unknown from the persisted form (0).
func ParsePersistedRecon(blob string) BrokerReconResult {
	var pr struct {
		CheckedAt time.Time `json:"checked_at"`
		Drifts    []struct {
			Ticker    string  `json:"ticker"`
			BrokerQty float64 `json:"broker_qty"`
			DBQty     float64 `json:"db_qty"`
			Source    string  `json:"source"`
		} `json:"drifts"`
	}
	if err := json.Unmarshal([]byte(blob), &pr); err != nil {
		return BrokerReconResult{Err: "invalid persisted reconciliation"}
	}
	res := BrokerReconResult{Available: true, CheckedAt: pr.CheckedAt}
	for _, d := range pr.Drifts {
		item := PositionDivergence{
			Ticker: d.Ticker, BrokerQty: d.BrokerQty, DBQty: d.DBQty, Kind: d.Source,
		}
		if d.Source == "exit_pending" {
			res.PendingExits = append(res.PendingExits, item)
		} else {
			res.Divergences = append(res.Divergences, item)
		}
	}
	res.InSync = len(res.Divergences) == 0 && len(res.PendingExits) == 0
	return res
}

// renderBrokerReconLine renders the dashboard's broker-vs-DB status line. It
// returns "" when no broker is configured (Available false, no error) so the
// dashboard stays clean in DB-only setups.
func renderBrokerReconLine(r BrokerReconResult) string {
	if !r.Available {
		if r.Err != "" {
			return scrollHintStyle.Render("  ⚠ broker check failed: " + truncate(r.Err, 60))
		}
		return ""
	}
	if len(r.Divergences) == 0 && len(r.PendingExits) == 0 {
		detail := "checked " + brokerReconAge(r.CheckedAt) + " ago"
		if r.BrokerCount > 0 {
			detail = fmt.Sprintf("%d positions, %s", r.BrokerCount, detail)
		}
		return greenText.Render("  ✓ broker in sync") + dimText.Render("  ("+detail+")")
	}

	if len(r.Divergences) == 0 {
		parts := make([]string, 0, len(r.PendingExits))
		for _, pending := range r.PendingExits {
			parts = append(parts, pending.Ticker)
		}
		shown, extra := parts, 0
		if len(shown) > 4 {
			extra, shown = len(shown)-4, shown[:4]
		}
		line := scrollHintStyle.Render(fmt.Sprintf("  ↻ broker exits finalizing (%d):", len(parts))) +
			" " + strings.Join(shown, dimText.Render(" · "))
		if extra > 0 {
			line += dimText.Render(fmt.Sprintf(" +%d more", extra))
		}
		line += dimText.Render("  (checked " + brokerReconAge(r.CheckedAt) + " ago)")
		return line
	}

	parts := make([]string, 0, len(r.Divergences))
	for _, d := range r.Divergences {
		parts = append(parts, fmt.Sprintf("%s %s", d.Ticker, divergenceDetail(d)))
	}
	shown := parts
	extra := 0
	if len(shown) > 4 {
		extra = len(shown) - 4
		shown = shown[:4]
	}
	line := redText.Render(fmt.Sprintf("  ⚠ broker divergence (%d):", len(r.Divergences))) +
		" " + strings.Join(shown, dimText.Render(" · "))
	if extra > 0 {
		line += dimText.Render(fmt.Sprintf(" +%d more", extra))
	}
	if len(r.PendingExits) > 0 {
		line += dimText.Render(fmt.Sprintf(" · %d exit finalizing", len(r.PendingExits)))
	}
	return line
}

// divergenceDetail renders the per-ticker quantity gap for one divergence.
func divergenceDetail(d PositionDivergence) string {
	switch d.Kind {
	case "broker_only":
		return dimText.Render(fmt.Sprintf("broker-only %.4g", d.BrokerQty))
	case "db_only":
		return dimText.Render(fmt.Sprintf("db-only %.4g", d.DBQty))
	case "side_mismatch":
		return dimText.Render(fmt.Sprintf("side broker %.4g/db %.4g", d.BrokerQty, d.DBQty))
	default:
		return dimText.Render(fmt.Sprintf("broker %.4g/db %.4g", d.BrokerQty, d.DBQty))
	}
}

func brokerReconAge(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return formatDuration(time.Since(t))
}
