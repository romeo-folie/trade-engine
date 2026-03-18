package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ── Types ─────────────────────────────────────────────────────────────────────

// BacktestRun represents a completed (or in-progress) backtest run.
// Mirrors the backtest_runs table from migrations/008_backtesting.sql.
type BacktestRun struct {
	ID                  string
	Label               string
	Ticker              string
	Strategy            string
	ModelProvider       string
	ModelName           string
	FromDate            time.Time
	ToDate              time.Time
	ConfigSnapshot      json.RawMessage
	TotalReturnPct      *float64
	AnnualizedReturnPct *float64
	SharpeRatio         *float64
	SortinoRatio        *float64
	CalmarRatio         *float64
	MaxDrawdownPct      *float64
	WinRatePct          *float64
	ProfitFactor        *float64
	Expectancy          *float64
	TotalTrades         int
	AvgHoldHours        *float64
	BestTradePct        *float64
	WorstTradePct       *float64
	CacheHitRate        *float64
	CreatedAt           time.Time
}

// BacktestTrade represents a single simulated trade within a backtest run.
// Mirrors the backtest_trades table.
type BacktestTrade struct {
	ID            string
	RunID         string
	Ticker        string
	Direction     string // buy | sell
	EntryTS       time.Time
	ExitTS        *time.Time
	EntryPrice    float64
	ExitPrice     *float64
	Quantity      float64
	PnL           *float64
	PnLPct        *float64
	ExitReason    string // stop_loss | take_profit | signal | end_of_data
	CreatedAt     time.Time
}

// ── Insert ────────────────────────────────────────────────────────────────────

// InsertRun persists a completed backtest run and returns its assigned label.
//
// The BT-NNNNN label is generated atomically inside the INSERT using a CTE that
// reads MAX(label) and increments it, eliminating the COUNT→INSERT race that
// exists when label generation and insertion are separate statements.
func InsertRun(ctx context.Context, r BacktestRun) (string, error) {
	var label string
	err := GetDB().QueryRow(ctx, `
		WITH next_label AS (
			SELECT 'BT-' || LPAD(
				(COALESCE(
					MAX(SUBSTRING(label FROM 4)::INTEGER),
					0
				) + 1)::TEXT,
				5, '0'
			) AS label
			FROM backtest_runs
		)
		INSERT INTO backtest_runs (
			label, ticker, strategy, model_provider, model_name,
			from_date, to_date, config_snapshot,
			total_return_pct, annualized_return_pct,
			sharpe_ratio, sortino_ratio, calmar_ratio,
			max_drawdown_pct, win_rate_pct, profit_factor,
			expectancy, total_trades, avg_hold_hours,
			best_trade_pct, worst_trade_pct, cache_hit_rate
		)
		SELECT
			next_label.label, $1,$2,$3,$4,$5,$6,$7,
			$8,$9,$10,$11,$12,$13,$14,$15,
			$16,$17,$18,$19,$20,$21
		FROM next_label
		RETURNING label`,
		r.Ticker, r.Strategy, r.ModelProvider, r.ModelName,
		r.FromDate.UTC(), r.ToDate.UTC(), r.ConfigSnapshot,
		r.TotalReturnPct, r.AnnualizedReturnPct,
		r.SharpeRatio, r.SortinoRatio, r.CalmarRatio,
		r.MaxDrawdownPct, r.WinRatePct, r.ProfitFactor,
		r.Expectancy, r.TotalTrades, r.AvgHoldHours,
		r.BestTradePct, r.WorstTradePct, r.CacheHitRate,
	).Scan(&label)
	if err != nil {
		return "", fmt.Errorf("insert backtest run: %w", err)
	}
	return label, nil
}

// InsertBacktestTrade persists a single simulated trade for a run.
func InsertBacktestTrade(ctx context.Context, t BacktestTrade) error {
	_, err := GetDB().Exec(ctx, `
		INSERT INTO backtest_trades (
			run_id, ticker, direction,
			entry_candle_ts, exit_candle_ts,
			entry_price, exit_price, quantity,
			pnl, pnl_pct, exit_reason
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		t.RunID, t.Ticker, t.Direction,
		t.EntryTS.UTC(), t.ExitTS,
		t.EntryPrice, t.ExitPrice, t.Quantity,
		t.PnL, t.PnLPct, t.ExitReason,
	)
	if err != nil {
		return fmt.Errorf("insert backtest trade: %w", err)
	}
	return nil
}

// ── Retrieve ──────────────────────────────────────────────────────────────────

// GetRunByLabel fetches a backtest run by its sequential label (e.g. "BT-00001").
// GetRun returns a single backtest run by its UUID.
func GetRun(ctx context.Context, id string) (BacktestRun, error) {
	row := GetDB().QueryRow(ctx, `
		SELECT id, label, ticker, strategy, model_provider, model_name,
		       from_date, to_date, config_snapshot,
		       total_return_pct, annualized_return_pct,
		       sharpe_ratio, sortino_ratio, calmar_ratio,
		       max_drawdown_pct, win_rate_pct, profit_factor,
		       expectancy, total_trades, avg_hold_hours,
		       best_trade_pct, worst_trade_pct, cache_hit_rate, created_at
		FROM backtest_runs WHERE id = $1`, id)
	r, err := scanRun(row)
	if err != nil {
		return BacktestRun{}, err
	}
	return *r, nil
}

func GetRunByLabel(ctx context.Context, label string) (*BacktestRun, error) {
	row := GetDB().QueryRow(ctx, `
		SELECT id, label, ticker, strategy, model_provider, model_name,
		       from_date, to_date, config_snapshot,
		       total_return_pct, annualized_return_pct,
		       sharpe_ratio, sortino_ratio, calmar_ratio,
		       max_drawdown_pct, win_rate_pct, profit_factor,
		       expectancy, total_trades, avg_hold_hours,
		       best_trade_pct, worst_trade_pct, cache_hit_rate, created_at
		FROM backtest_runs WHERE label = $1`, label)
	return scanRun(row)
}

// ListRuns returns the most recent backtest runs ordered by creation time desc.
func ListRuns(ctx context.Context, limit int) ([]BacktestRun, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := GetDB().Query(ctx, `
		SELECT id, label, ticker, strategy, model_provider, model_name,
		       from_date, to_date, config_snapshot,
		       total_return_pct, annualized_return_pct,
		       sharpe_ratio, sortino_ratio, calmar_ratio,
		       max_drawdown_pct, win_rate_pct, profit_factor,
		       expectancy, total_trades, avg_hold_hours,
		       best_trade_pct, worst_trade_pct, cache_hit_rate, created_at
		FROM backtest_runs
		ORDER BY created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list backtest runs: %w", err)
	}
	defer rows.Close()

	var runs []BacktestRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, *r)
	}
	return runs, rows.Err()
}

// ListTradesForRun returns all simulated trades for a run, ordered by entry timestamp.
func ListTradesForRun(ctx context.Context, runID string) ([]BacktestTrade, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, run_id, ticker, direction,
		       entry_candle_ts, exit_candle_ts,
		       entry_price, exit_price, quantity,
		       pnl, pnl_pct, exit_reason, created_at
		FROM backtest_trades
		WHERE run_id = $1
		ORDER BY entry_candle_ts ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list backtest trades: %w", err)
	}
	defer rows.Close()

	var trades []BacktestTrade
	for rows.Next() {
		var t BacktestTrade
		err := rows.Scan(
			&t.ID, &t.RunID, &t.Ticker, &t.Direction,
			&t.EntryTS, &t.ExitTS,
			&t.EntryPrice, &t.ExitPrice, &t.Quantity,
			&t.PnL, &t.PnLPct, &t.ExitReason, &t.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan backtest trade: %w", err)
		}
		trades = append(trades, t)
	}
	return trades, rows.Err()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

type scanner interface {
	Scan(dest ...any) error
}

func scanRun(s scanner) (*BacktestRun, error) {
	var r BacktestRun
	err := s.Scan(
		&r.ID, &r.Label, &r.Ticker, &r.Strategy, &r.ModelProvider, &r.ModelName,
		&r.FromDate, &r.ToDate, &r.ConfigSnapshot,
		&r.TotalReturnPct, &r.AnnualizedReturnPct,
		&r.SharpeRatio, &r.SortinoRatio, &r.CalmarRatio,
		&r.MaxDrawdownPct, &r.WinRatePct, &r.ProfitFactor,
		&r.Expectancy, &r.TotalTrades, &r.AvgHoldHours,
		&r.BestTradePct, &r.WorstTradePct, &r.CacheHitRate, &r.CreatedAt,
	)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan backtest run: %w", err)
	}
	return &r, nil
}
