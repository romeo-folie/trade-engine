package store

import (
	"context"
	"encoding/json"
	"time"
)

// ── Types ─────────────────────────────────────────────────────────────────────

// PortfolioSnapshot mirrors a row from the portfolio_snapshots table.
type PortfolioSnapshot struct {
	ID               string
	TotalValue       float64
	Cash             float64
	Invested         float64
	UnrealizedPnL    float64
	RealizedPnLToday float64
	OpenPositions    int
	Source           string // "engine" | "poller" | "backfill"
	Mode             string // "live" | "paper" (empty for pre-migration rows)
	Broker           string // "alpaca" | "ibkr" (empty for pre-migration rows)
	CreatedAt        time.Time
}

// InsertPortfolioSnapshot writes a snapshot row. source/mode/broker tag the
// origin of the data (added by migration 021). Pass empty strings for
// pre-migration compatibility — Postgres will use the column DEFAULT.
func InsertPortfolioSnapshot(ctx context.Context, totalValue, cash, invested, unrealizedPnL, realizedPnLToday float64, openPositions int, source, mode, broker string) error {
	if source == "" {
		source = "engine"
	}
	_, err := GetDB().Exec(ctx, `
		INSERT INTO portfolio_snapshots
		  (total_value, cash, invested, unrealized_pnl, realized_pnl_today, open_positions, source, mode, broker)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		totalValue, cash, invested, unrealizedPnL, realizedPnLToday, openPositions,
		source, nullableText(mode), nullableText(broker),
	)
	return err
}

// nullableText converts an empty string to nil so Postgres stores NULL.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// GetPortfolioSnapshotsByMode returns snapshots within [from, to] filtered by
// mode ("live"/"paper"/"" for all), ordered by time ascending.
func GetPortfolioSnapshotsByMode(ctx context.Context, from, to time.Time, mode string) ([]PortfolioSnapshot, error) {
	var query string
	var args []any
	if mode == "" || mode == "all" {
		query = `SELECT id, total_value, cash, invested, unrealized_pnl, realized_pnl_today,
		                open_positions, COALESCE(source,'engine'), COALESCE(mode,''), COALESCE(broker,''), created_at
		         FROM portfolio_snapshots
		         WHERE created_at >= $1 AND created_at <= $2
		         ORDER BY created_at ASC`
		args = []any{from.UTC(), to.UTC()}
	} else {
		query = `SELECT id, total_value, cash, invested, unrealized_pnl, realized_pnl_today,
		                open_positions, COALESCE(source,'engine'), COALESCE(mode,''), COALESCE(broker,''), created_at
		         FROM portfolio_snapshots
		         WHERE created_at >= $1 AND created_at <= $2 AND mode = $3
		         ORDER BY created_at ASC`
		args = []any{from.UTC(), to.UTC(), mode}
	}
	rows, err := GetDB().Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snaps []PortfolioSnapshot
	for rows.Next() {
		var s PortfolioSnapshot
		if err := rows.Scan(
			&s.ID, &s.TotalValue, &s.Cash, &s.Invested,
			&s.UnrealizedPnL, &s.RealizedPnLToday, &s.OpenPositions,
			&s.Source, &s.Mode, &s.Broker, &s.CreatedAt,
		); err != nil {
			return nil, err
		}
		snaps = append(snaps, s)
	}
	return snaps, rows.Err()
}

// DailySummary mirrors a row from the daily_summaries table.
type DailySummary struct {
	ID                string
	Date              time.Time
	StartingValue     float64
	EndingValue       float64
	PnL               float64
	PnLPct            float64
	TradesOpened      int
	TradesClosed      int
	Wins              int
	Losses            int
	SignalsGenerated  int
	SignalsBlocked    int
	ModelTokensInput  int64
	ModelTokensOutput int64
	CreatedAt         time.Time

	// Issue #325 — observability counters. Populated by the rollup writer in
	// portfolio.MaybeDailySummary. SignalsTotal is the strict daily total
	// (any status). SignalsExecuted is the subset that reached
	// status='executed'. ExecutedWinning/ExecutedLosing aggregate trades by
	// pnl sign for trades closed today. BlockedByReason is a JSONB map of
	// block_reason -> count for signals blocked today; nil when the column
	// is empty/missing.
	SignalsTotal     int
	SignalsExecuted  int
	ExecutedWinning  int
	ExecutedLosing   int
	BlockedByReason  map[string]int
}

// ── Portfolio Snapshots ───────────────────────────────────────────────────────

// GetPortfolioSnapshots returns snapshots within [from, to] ordered by time.
func GetPortfolioSnapshots(ctx context.Context, from, to time.Time) ([]PortfolioSnapshot, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, total_value, cash, invested,
		       unrealized_pnl, realized_pnl_today, open_positions, created_at
		FROM portfolio_snapshots
		WHERE created_at >= $1 AND created_at <= $2
		ORDER BY created_at ASC`,
		from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snaps []PortfolioSnapshot
	for rows.Next() {
		var s PortfolioSnapshot
		if err := rows.Scan(
			&s.ID, &s.TotalValue, &s.Cash, &s.Invested,
			&s.UnrealizedPnL, &s.RealizedPnLToday, &s.OpenPositions, &s.CreatedAt,
		); err != nil {
			return nil, err
		}
		snaps = append(snaps, s)
	}
	return snaps, rows.Err()
}

// GetLatestPortfolioSnapshot returns the most recent snapshot, or nil if none.
func GetLatestPortfolioSnapshot(ctx context.Context) (*PortfolioSnapshot, error) {
	var s PortfolioSnapshot
	err := GetDB().QueryRow(ctx, `
		SELECT id, total_value, cash, invested,
		       unrealized_pnl, realized_pnl_today, open_positions, created_at
		FROM portfolio_snapshots
		ORDER BY created_at DESC LIMIT 1`).Scan(
		&s.ID, &s.TotalValue, &s.Cash, &s.Invested,
		&s.UnrealizedPnL, &s.RealizedPnLToday, &s.OpenPositions, &s.CreatedAt,
	)
	if err != nil {
		if IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

// GetFirstSnapshotOfDay returns the earliest snapshot on the given UTC date, or
// nil if none exists. Used to determine the true beginning-of-day equity when
// no daily_summaries row has been written yet.
func GetFirstSnapshotOfDay(ctx context.Context, day time.Time) (*PortfolioSnapshot, error) {
	start := day.UTC().Truncate(24 * time.Hour)
	end := start.Add(24 * time.Hour)
	var s PortfolioSnapshot
	err := GetDB().QueryRow(ctx, `
		SELECT id, total_value, cash, invested,
		       unrealized_pnl, realized_pnl_today, open_positions, created_at
		FROM portfolio_snapshots
		WHERE created_at >= $1 AND created_at < $2
		ORDER BY created_at ASC LIMIT 1`,
		start, end).Scan(
		&s.ID, &s.TotalValue, &s.Cash, &s.Invested,
		&s.UnrealizedPnL, &s.RealizedPnLToday, &s.OpenPositions, &s.CreatedAt,
	)
	if err != nil {
		if IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

// GetLastSnapshotBeforeDay returns the most recent snapshot written before the
// given UTC date, used as the previous-day closing equity for PnL baseline.
func GetLastSnapshotBeforeDay(ctx context.Context, day time.Time) (*PortfolioSnapshot, error) {
	cutoff := day.UTC().Truncate(24 * time.Hour)
	var s PortfolioSnapshot
	err := GetDB().QueryRow(ctx, `
		SELECT id, total_value, cash, invested,
		       unrealized_pnl, realized_pnl_today, open_positions, created_at
		FROM portfolio_snapshots
		WHERE created_at < $1
		ORDER BY created_at DESC LIMIT 1`,
		cutoff).Scan(
		&s.ID, &s.TotalValue, &s.Cash, &s.Invested,
		&s.UnrealizedPnL, &s.RealizedPnLToday, &s.OpenPositions, &s.CreatedAt,
	)
	if err != nil {
		if IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

// ── Daily Summaries ───────────────────────────────────────────────────────────

// GetDailySummaries returns summaries within [from, to] ordered by date.
func GetDailySummaries(ctx context.Context, from, to time.Time) ([]DailySummary, error) {
	rows, err := GetDB().Query(ctx, dailySummarySelect+`
		WHERE date >= $1 AND date <= $2
		ORDER BY date ASC`,
		from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDailySummaries(rows)
}

// GetDailySummaryByDate returns the summary for a specific date, or nil.
func GetDailySummaryByDate(ctx context.Context, date time.Time) (*DailySummary, error) {
	rows, err := GetDB().Query(ctx, dailySummarySelect+`
		WHERE date = $1`, date.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanDailySummaries(rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// ── Trades by date range ──────────────────────────────────────────────────────
// GetRecentDailySummaries returns the last N days of summaries.
func GetRecentDailySummaries(ctx context.Context, limit int) ([]DailySummary, error) {
	rows, err := GetDB().Query(ctx, dailySummarySelect+`
		ORDER BY date DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDailySummaries(rows)
}

// dailySummarySelect is the canonical column list/order used by every
// DailySummary read path so adding columns is a single-edit change. Issue
// #325 added signals_total / signals_executed / executed_winning /
// executed_losing / blocked_by_reason. COALESCE keeps reads working against
// older rows that predate the migration.
const dailySummarySelect = `
	SELECT id, date, starting_value, ending_value, pnl, COALESCE(pnl_pct,0),
	       trades_opened, trades_closed, wins, losses,
	       signals_generated, signals_blocked,
	       model_tokens_input, model_tokens_output, created_at,
	       COALESCE(signals_total,0), COALESCE(signals_executed,0),
	       COALESCE(executed_winning,0), COALESCE(executed_losing,0),
	       COALESCE(blocked_by_reason,'{}'::jsonb)
	FROM daily_summaries`

// scanDailySummaries materialises rows into DailySummary values.
// blocked_by_reason is always a JSON object (possibly empty): the
// dailySummarySelect COALESCE rewrites NULL to '{}'::jsonb, and the
// portfolio writer always emits at least '{}'. As an optimisation we still
// leave the map nil when the column scans as empty (`{}`) so callers can
// cheaply check `if s.BlockedByReason == nil` to mean "no blocks today" —
// the previous "not yet populated" distinction the old comment claimed is
// not actually possible from the SQL.
func scanDailySummaries(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]DailySummary, error) {
	var summaries []DailySummary
	for rows.Next() {
		var s DailySummary
		var blockedRaw []byte
		if err := rows.Scan(
			&s.ID, &s.Date, &s.StartingValue, &s.EndingValue, &s.PnL, &s.PnLPct,
			&s.TradesOpened, &s.TradesClosed, &s.Wins, &s.Losses,
			&s.SignalsGenerated, &s.SignalsBlocked,
			&s.ModelTokensInput, &s.ModelTokensOutput, &s.CreatedAt,
			&s.SignalsTotal, &s.SignalsExecuted,
			&s.ExecutedWinning, &s.ExecutedLosing,
			&blockedRaw,
		); err != nil {
			return nil, err
		}
		if len(blockedRaw) > 0 && string(blockedRaw) != "{}" {
			m := map[string]int{}
			if err := json.Unmarshal(blockedRaw, &m); err == nil && len(m) > 0 {
				s.BlockedByReason = m
			}
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}

// DailyCounters holds the aggregated counters written to daily_summaries by
// the rollup job (Issue #325). Computed by AggregateDailyCounters from the
// signals + trades tables for a single calendar date.
type DailyCounters struct {
	Date            time.Time
	SignalsTotal    int
	SignalsBlocked  int
	SignalsExecuted int
	ExecutedWinning int
	ExecutedLosing  int
	BlockedByReason map[string]int
}

// AggregateDailyCounters scans signals and closed trades for the UTC calendar
// day containing `day` and returns the rollup. Used by the rollup writer
// (portfolio.MaybeDailySummary) and by the new /api/portfolio/daily-counters
// handler when fetching a backfill window.
//
// Signals are bucketed by status; "executed" is the canonical successful
// outcome. blocked_by_reason groups blocked rows by their canonical
// block_reason text (NULL/empty rows bucket as "unspecified" so a missing
// reason is visible rather than silently dropped).
//
// Closed trades are joined by closed_at::date — wins are pnl > 0, losses are
// pnl < 0. A breakeven trade (pnl = 0) counts as neither.
func AggregateDailyCounters(ctx context.Context, day time.Time) (DailyCounters, error) {
	dayUTC := day.UTC().Truncate(24 * time.Hour)
	dc := DailyCounters{Date: dayUTC, BlockedByReason: map[string]int{}}

	// Signals total + per-status counts.
	rows, err := GetDB().Query(ctx, `
		SELECT status, COUNT(*) FROM signals
		WHERE created_at >= $1 AND created_at < $1::timestamptz + interval '1 day'
		GROUP BY status`, dayUTC)
	if err != nil {
		return dc, err
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return dc, err
		}
		dc.SignalsTotal += n
		switch status {
		case "blocked":
			dc.SignalsBlocked += n
		case "executed":
			dc.SignalsExecuted += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return dc, err
	}

	// Block-reason histogram (only for blocked rows).
	rows, err = GetDB().Query(ctx, `
		SELECT COALESCE(NULLIF(block_reason,''), 'unspecified'), COUNT(*)
		FROM signals
		WHERE status = 'blocked'
		  AND created_at >= $1 AND created_at < $1::timestamptz + interval '1 day'
		GROUP BY 1`, dayUTC)
	if err != nil {
		return dc, err
	}
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			rows.Close()
			return dc, err
		}
		dc.BlockedByReason[reason] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return dc, err
	}

	// Closed-trade win/loss counts for the day.
	rows, err = GetDB().Query(ctx, `
		SELECT COALESCE(pnl,0) FROM trades
		WHERE status = 'closed'
		  AND closed_at >= $1 AND closed_at < $1::timestamptz + interval '1 day'`,
		dayUTC)
	if err != nil {
		return dc, err
	}
	for rows.Next() {
		var pnl float64
		if err := rows.Scan(&pnl); err != nil {
			rows.Close()
			return dc, err
		}
		switch {
		case pnl > 0:
			dc.ExecutedWinning++
		case pnl < 0:
			dc.ExecutedLosing++
		}
	}
	rows.Close()
	return dc, rows.Err()
}

type PortfolioStats struct {
	WinRatePct   float64
	ProfitFactor float64
	TotalTrades  int
	BestDayPnl   float64
	WorstDayPnl  float64
}

// GetPortfolioStats calculates aggregate performance metrics from all daily summaries.
func GetPortfolioStats(ctx context.Context) (*PortfolioStats, error) {
	var stats PortfolioStats
	var totalWins, totalLosses int
	var grossProfit, grossLoss float64

	rows, err := GetDB().Query(ctx, `
		SELECT wins, losses, pnl
		FROM daily_summaries`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var w, l int
		var pnl float64
		if err := rows.Scan(&w, &l, &pnl); err != nil {
			return nil, err
		}
		totalWins += w
		totalLosses += l
		stats.TotalTrades += (w + l)
		if pnl > 0 {
			grossProfit += pnl
			if pnl > stats.BestDayPnl {
				stats.BestDayPnl = pnl
			}
		} else if pnl < 0 {
			grossLoss += (-pnl)
			if pnl < stats.WorstDayPnl {
				stats.WorstDayPnl = pnl
			}
		}
	}

	if stats.TotalTrades > 0 {
		stats.WinRatePct = float64(totalWins) / float64(stats.TotalTrades) * 100
	}
	if grossLoss > 0 {
		stats.ProfitFactor = grossProfit / grossLoss
	} else if grossProfit > 0 {
		stats.ProfitFactor = 99.9 // effectively infinite
	}

	return &stats, nil
}

// GetClosedTradesByDateRange returns closed trades whose closed_at falls in
// the half-open interval [from, to). The exclusive upper bound matches the
// convention used by AggregateDailyCounters (which keys windows off
// `created_at < day + interval '1 day'`), so "all trades closed on day D"
// is `(D, D + 24h)` from both sides — a trade closed at exactly midnight UTC
// counts in the day starting at that midnight, never in the previous day.
//
// Callers that want a single day pass `to = from.Add(24 * time.Hour)`.
// Range callers (performance / weekly reports) pass `to` as the moment they
// want excluded — typically `time.Now()` for "everything up to right now".
func GetClosedTradesByDateRange(ctx context.Context, from, to time.Time) ([]Trade, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, COALESCE(signal_id::text,''), ticker, direction, strategy,
		       entry_price, COALESCE(exit_price,0),
		       quantity, COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(pnl,0), COALESCE(pnl_pct,0), status,
		       COALESCE(model_provider,''), COALESCE(model_name,''),
		       opened_at, COALESCE(closed_at, opened_at), created_at,
		       COALESCE(high_water_mark,0)
		FROM trades
		WHERE status='closed'
		  AND closed_at >= $1 AND closed_at < $2
		ORDER BY closed_at ASC`,
		from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrades(rows)
}
