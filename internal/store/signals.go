package store

import (
	"context"
	"time"
)

// Signal mirrors a row from the signals table.
type Signal struct {
	ID                string
	Ticker            string
	Action            string // BUY | SELL
	Strategy          string
	SignalPrice        float64
	StopLoss          float64
	TakeProfit        float64
	Confidence        float64
	SizePct           float64
	Status            string // pending | approved | blocked | executed | expired | stale
	BlockReason       string
	AnalysisIDs       []string
	ExpiresAt         time.Time
	StalePriceMovePct float64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// InsertSignal creates a new signal record and returns its UUID.
func InsertSignal(ctx context.Context, s Signal) (string, error) {
	var id string
	err := GetDB().QueryRow(ctx, `
		INSERT INTO signals (
			ticker, action, strategy, signal_price, stop_loss, take_profit,
			confidence, size_pct, status, block_reason,
			expires_at, stale_threshold_pct
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING id`,
		s.Ticker, s.Action, s.Strategy, s.SignalPrice,
		nullableFloat(s.StopLoss), nullableFloat(s.TakeProfit),
		s.Confidence, s.SizePct, s.Status, nullableString(s.BlockReason),
		s.ExpiresAt, s.StalePriceMovePct,
	).Scan(&id)
	return id, err
}

// UpdateSignalStatus sets the status (and optional block_reason) for a signal.
func UpdateSignalStatus(ctx context.Context, id, status, blockReason string) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE signals SET status=$1, block_reason=$2, updated_at=NOW() WHERE id=$3`,
		status, nullableString(blockReason), id)
	return err
}

// GetPendingSignals returns all signals with status = 'pending' or 'approved'.
func GetPendingSignals(ctx context.Context) ([]Signal, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, ticker, action, strategy, signal_price,
		       COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       confidence, COALESCE(size_pct,0), status,
		       COALESCE(block_reason,''), expires_at, stale_threshold_pct,
		       created_at, updated_at
		FROM signals WHERE status IN ('pending','approved')
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSignals(rows)
}

// GetSignalsByTicker returns recent signals for a ticker.
func GetSignalsByTicker(ctx context.Context, ticker string, limit int) ([]Signal, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, ticker, action, strategy, signal_price,
		       COALESCE(stop_loss,0), COALESCE(take_profit,0),
		       confidence, COALESCE(size_pct,0), status,
		       COALESCE(block_reason,''), expires_at, stale_threshold_pct,
		       created_at, updated_at
		FROM signals WHERE ticker=$1
		ORDER BY created_at DESC LIMIT $2`, ticker, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSignals(rows)
}

func scanSignals(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]Signal, error) {
	var out []Signal
	for rows.Next() {
		var s Signal
		if err := rows.Scan(
			&s.ID, &s.Ticker, &s.Action, &s.Strategy, &s.SignalPrice,
			&s.StopLoss, &s.TakeProfit,
			&s.Confidence, &s.SizePct, &s.Status,
			&s.BlockReason, &s.ExpiresAt, &s.StalePriceMovePct,
			&s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
