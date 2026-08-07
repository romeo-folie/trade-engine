package store

import "context"

// closedLedgerPredicate defines "the closed book" in exactly one place: every
// trade that reached a terminal state carrying a realized P&L.
const closedLedgerPredicate = `status = 'closed' AND pnl IS NOT NULL`

// ClosedLedgerStats is the single definition of the closed-trade book, shared by
// the Portfolio screen (GetPortfolioStats) and the Performance screen
// (GetEntryPerformance) so the two can never disagree.
//
// Population: every closed trade with a realized P&L, joined to nothing. That
// is the load-bearing detail. Provenance queries reach the analysis through
// `JOIN signals s ON s.id = t.signal_id`, but trades.signal_id is nullable and
// the startup reconciler (#328) adopts broker positions with no signal at all —
// so any headline computed off that join silently drops them. They were real
// money: at the time this was written, 31 adopted `recovery` rows carried
// -$2,305.86 of realized loss that the Performance screen was not counting,
// which made net realized read +$3,340 against a book of +$1,035.
//
// Definition: win rate = wins / all-closed × 100, with break-even (pnl = 0)
// kept in the denominator. Rounded here rather than at each call site so both
// screens render byte-identical numbers.
type ClosedLedgerStats struct {
	N            int
	Wins         int
	Losses       int
	Breakeven    int
	WinRatePct   float64
	AvgPnLPct    *float64
	MedianPnLPct *float64
	NetPnL       *float64
	WindowStart  *string
	WindowEnd    *string
}

// GetClosedLedgerStats aggregates the closed-trade ledger. Every field is
// nullable-safe: an empty ledger yields N = 0 and NULL money columns rather
// than an error.
func GetClosedLedgerStats(ctx context.Context) (*ClosedLedgerStats, error) {
	var s ClosedLedgerStats
	if err := GetDB().QueryRow(ctx, `
		SELECT count(*)::int,
		       count(*) FILTER (WHERE pnl > 0)::int,
		       count(*) FILTER (WHERE pnl < 0)::int,
		       count(*) FILTER (WHERE pnl = 0)::int,
		       COALESCE(round(100.0 * count(*) FILTER (WHERE pnl > 0)::numeric / NULLIF(count(*), 0), 1), 0)::float8,
		       round(avg(pnl_pct)::numeric, 2)::float8,
		       round((percentile_cont(0.5) WITHIN GROUP (ORDER BY pnl_pct))::numeric, 2)::float8,
		       round(sum(pnl)::numeric, 2)::float8,
		       min(opened_at)::date::text,
		       max(opened_at)::date::text
		FROM trades WHERE `+closedLedgerPredicate).Scan(
		&s.N, &s.Wins, &s.Losses, &s.Breakeven, &s.WinRatePct,
		&s.AvgPnLPct, &s.MedianPnLPct, &s.NetPnL, &s.WindowStart, &s.WindowEnd,
	); err != nil {
		return nil, err
	}
	return &s, nil
}
