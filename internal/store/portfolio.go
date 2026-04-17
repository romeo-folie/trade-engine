package store

import (
	"context"
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
	SignalsGenerated   int
	SignalsBlocked     int
	ModelTokensInput  int64
	ModelTokensOutput int64
	CreatedAt         time.Time
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
	rows, err := GetDB().Query(ctx, `
		SELECT id, date, starting_value, ending_value, pnl, COALESCE(pnl_pct,0),
		       trades_opened, trades_closed, wins, losses,
		       signals_generated, signals_blocked,
		       model_tokens_input, model_tokens_output, created_at
		FROM daily_summaries
		WHERE date >= $1 AND date <= $2
		ORDER BY date ASC`,
		from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []DailySummary
	for rows.Next() {
		var s DailySummary
		if err := rows.Scan(
			&s.ID, &s.Date, &s.StartingValue, &s.EndingValue, &s.PnL, &s.PnLPct,
			&s.TradesOpened, &s.TradesClosed, &s.Wins, &s.Losses,
			&s.SignalsGenerated, &s.SignalsBlocked,
			&s.ModelTokensInput, &s.ModelTokensOutput, &s.CreatedAt,
		); err != nil {
			return nil, err
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}

// GetDailySummaryByDate returns the summary for a specific date, or nil.
func GetDailySummaryByDate(ctx context.Context, date time.Time) (*DailySummary, error) {
	var s DailySummary
	err := GetDB().QueryRow(ctx, `
		SELECT id, date, starting_value, ending_value, pnl, COALESCE(pnl_pct,0),
		       trades_opened, trades_closed, wins, losses,
		       signals_generated, signals_blocked,
		       model_tokens_input, model_tokens_output, created_at
		FROM daily_summaries
		WHERE date = $1`, date.UTC()).Scan(
		&s.ID, &s.Date, &s.StartingValue, &s.EndingValue, &s.PnL, &s.PnLPct,
		&s.TradesOpened, &s.TradesClosed, &s.Wins, &s.Losses,
		&s.SignalsGenerated, &s.SignalsBlocked,
		&s.ModelTokensInput, &s.ModelTokensOutput, &s.CreatedAt,
	)
	if err != nil {
		if IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

// ── Trades by date range ──────────────────────────────────────────────────────
// GetRecentDailySummaries returns the last N days of summaries.
func GetRecentDailySummaries(ctx context.Context, limit int) ([]DailySummary, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, date, starting_value, ending_value, pnl, COALESCE(pnl_pct,0),
		       trades_opened, trades_closed, wins, losses,
		       signals_generated, signals_blocked,
		       model_tokens_input, model_tokens_output, created_at
		FROM daily_summaries
		ORDER BY date DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []DailySummary
	for rows.Next() {
		var s DailySummary
		if err := rows.Scan(
			&s.ID, &s.Date, &s.StartingValue, &s.EndingValue, &s.PnL, &s.PnLPct,
			&s.TradesOpened, &s.TradesClosed, &s.Wins, &s.Losses,
			&s.SignalsGenerated, &s.SignalsBlocked,
			&s.ModelTokensInput, &s.ModelTokensOutput, &s.CreatedAt,
		); err != nil {
			return nil, err
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
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

// GetClosedTradesByDateRange returns closed trades within [from, to].
func GetClosedTradesByDateRange(ctx context.Context, from, to time.Time) ([]Trade, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, COALESCE(signal_id::text,''), ticker, direction, strategy,
		       entry_price, COALESCE(exit_price,0),
		       quantity, COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       COALESCE(pnl,0), COALESCE(pnl_pct,0), status,
		       COALESCE(model_provider,''), COALESCE(model_name,''),
		       opened_at, COALESCE(closed_at, opened_at), created_at
		FROM trades
		WHERE status='closed'
		  AND closed_at >= $1 AND closed_at <= $2
		ORDER BY closed_at ASC`,
		from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrades(rows)
}
