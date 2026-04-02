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
	CreatedAt        time.Time
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
