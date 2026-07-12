package store

import (
	"context"
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/study"
)

// GetStudyTrades returns closed stock-path trades (prediction-market rows
// excluded) opened at or after since, joined with their originating signal
// for the execution study. Trades without a signals row (e.g. recovery
// adoptions) come back with HasSignal=false and SignalAt=OpenedAt.
func GetStudyTrades(ctx context.Context, since time.Time) ([]study.TradeCase, error) {
	rows, err := pool.Query(ctx, `
		SELECT t.id, t.ticker, t.direction::text, t.strategy,
		       t.entry_price, COALESCE(t.exit_price, 0), t.quantity,
		       COALESCE(t.stop_loss, 0), COALESCE(t.take_profit, 0),
		       COALESCE(t.pnl_pct, 0),
		       t.opened_at, COALESCE(t.closed_at, to_timestamp(0)),
		       COALESCE(s.signal_price, 0), COALESCE(s.created_at, t.opened_at),
		       (s.id IS NOT NULL)
		FROM trades t
		LEFT JOIN signals s ON s.id = t.signal_id
		WHERE t.opened_at >= $1
		  AND t.status = 'closed'
		  AND t.platform IS NULL
		ORDER BY t.opened_at`, since)
	if err != nil {
		return nil, fmt.Errorf("query study trades: %w", err)
	}
	defer rows.Close()

	var out []study.TradeCase
	for rows.Next() {
		var tc study.TradeCase
		if err := rows.Scan(&tc.ID, &tc.Ticker, &tc.Direction, &tc.Strategy,
			&tc.EntryPrice, &tc.ExitPrice, &tc.Quantity,
			&tc.StopLoss, &tc.TakeProfit, &tc.PnLPct,
			&tc.OpenedAt, &tc.ClosedAt,
			&tc.SignalPrice, &tc.SignalAt, &tc.HasSignal); err != nil {
			return nil, fmt.Errorf("scan study trade: %w", err)
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}
