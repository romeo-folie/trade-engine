package store

import (
	"context"
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

// UpsertCandles bulk-inserts candles for any mix of tickers/timeframes.
// Uses ON CONFLICT DO NOTHING — reinserting the same candle is a no-op.
// Batches into chunks of 500 rows to stay well within Postgres parameter limits.
func UpsertCandles(ctx context.Context, candles []data.Candle) error {
	if len(candles) == 0 {
		return nil
	}
	const batchSize = 500
	for i := 0; i < len(candles); i += batchSize {
		end := i + batchSize
		if end > len(candles) {
			end = len(candles)
		}
		if err := upsertCandleBatch(ctx, candles[i:end]); err != nil {
			return fmt.Errorf("upsert candles batch [%d:%d]: %w", i, end, err)
		}
	}
	return nil
}

func upsertCandleBatch(ctx context.Context, candles []data.Candle) error {
	q := `INSERT INTO candles (ticker, timeframe, ts, open, high, low, close, volume, vwap) VALUES `
	args := make([]any, 0, len(candles)*9)
	for i, c := range candles {
		if i > 0 {
			q += ", "
		}
		base := i * 9
		q += fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9)
		args = append(args,
			c.Ticker, string(c.Timeframe), c.Timestamp.UTC(),
			c.Open, c.High, c.Low, c.Close, c.Volume, c.VWAP,
		)
	}
	q += " ON CONFLICT (ticker, timeframe, ts) DO NOTHING"
	_, err := GetDB().Exec(ctx, q, args...)
	return err
}

// GetCandles retrieves candles for a ticker+timeframe within an inclusive time range,
// ordered by timestamp ascending.
func GetCandles(ctx context.Context, ticker string, tf data.Timeframe, from, to time.Time) ([]data.Candle, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT ticker, timeframe, ts, open, high, low, close, volume, vwap
		FROM candles
		WHERE ticker = $1 AND timeframe = $2 AND ts >= $3 AND ts <= $4
		ORDER BY ts ASC
	`, ticker, string(tf), from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candles []data.Candle
	for rows.Next() {
		var c data.Candle
		var tfStr string
		if err := rows.Scan(&c.Ticker, &tfStr, &c.Timestamp,
			&c.Open, &c.High, &c.Low, &c.Close, &c.Volume, &c.VWAP); err != nil {
			return nil, err
		}
		c.Timeframe = data.Timeframe(tfStr)
		candles = append(candles, c)
	}
	return candles, rows.Err()
}

// GetLatestCandle returns the most recent candle for a ticker+timeframe pair.
// Returns nil, nil if no candle exists yet.
// GetFreshestCandle returns the most recent candle for a ticker across ALL
// timeframes — the freshest available mark-to-market price. The engine scans
// equities on intraday timeframes (1h/4h) while daily candles are written far
// less often, so pinning to a single timeframe (e.g. "1d") leaves many tickers
// with stale or missing prices. Returns (nil, nil) when no candle exists.
func GetFreshestCandle(ctx context.Context, ticker string) (*data.Candle, error) {
	row := GetDB().QueryRow(ctx, `
		SELECT ticker, timeframe, ts, open, high, low, close, volume, vwap
		FROM candles
		WHERE ticker = $1
		ORDER BY ts DESC
		LIMIT 1
	`, ticker)

	var c data.Candle
	var tfStr string
	err := row.Scan(&c.Ticker, &tfStr, &c.Timestamp,
		&c.Open, &c.High, &c.Low, &c.Close, &c.Volume, &c.VWAP)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	c.Timeframe = data.Timeframe(tfStr)
	return &c, nil
}

// GetPrevDailyClose returns the close of the last daily candle strictly
// before the given instant (pass midnight of the trading day in the market's
// timezone to get the prior session's close). Returns 0, nil when no such
// candle exists — callers treat that as "unknown", not as a price.
func GetPrevDailyClose(ctx context.Context, ticker string, before time.Time) (float64, error) {
	row := GetDB().QueryRow(ctx, `
		SELECT close FROM candles
		WHERE ticker = $1 AND timeframe = '1d' AND ts < $2 AND close > 0
		ORDER BY ts DESC
		LIMIT 1
	`, ticker, before)

	var close float64
	if err := row.Scan(&close); err != nil {
		if isNoRows(err) {
			return 0, nil
		}
		return 0, err
	}
	return close, nil
}

func GetLatestCandle(ctx context.Context, ticker string, tf data.Timeframe) (*data.Candle, error) {
	row := GetDB().QueryRow(ctx, `
		SELECT ticker, timeframe, ts, open, high, low, close, volume, vwap
		FROM candles
		WHERE ticker = $1 AND timeframe = $2
		ORDER BY ts DESC
		LIMIT 1
	`, ticker, string(tf))

	var c data.Candle
	var tfStr string
	err := row.Scan(&c.Ticker, &tfStr, &c.Timestamp,
		&c.Open, &c.High, &c.Low, &c.Close, &c.Volume, &c.VWAP)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	c.Timeframe = data.Timeframe(tfStr)
	return &c, nil
}
