package store

import (
	"context"
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/logger"
)

// UpsertCandles bulk-inserts candles for any mix of tickers/timeframes.
// Same-timestamp bars are refreshed because providers return the current
// session's still-forming candle on every poll. Completed bars may also be
// revised for corrections or corporate-action adjustments.
// Batches into chunks of 500 rows to stay well within Postgres parameter limits.
//
// Corrupt provider bars are dropped here (issue #519). This is the single
// persistence chokepoint for every provider and every caller, so the guard
// cannot be bypassed by adding a new ingest path. Rejections are logged rather
// than swallowed — a bar disappearing silently would turn provider degradation
// into an invisible failure.
func UpsertCandles(ctx context.Context, candles []data.Candle) error {
	if len(candles) == 0 {
		return nil
	}
	// PostgreSQL rejects an INSERT ... ON CONFLICT DO UPDATE statement that
	// would affect the same target row twice. Collapse duplicate provider
	// snapshots before batching; the newest occurrence is the one callers
	// expect to persist.
	//
	// This MUST run before sanitisation. The guard counts consecutive rejected
	// entries toward its re-rating threshold, and repeated snapshots of ONE
	// timestamp are not evidence of a sustained level shift — three copies of a
	// single corrupt bar would reach the threshold, be "restored" as a genuine
	// move, and then survive the collapse below and reach the database. Sizing
	// the run in unique bars closes that.
	type candleKey struct {
		ticker    string
		timeframe data.Timeframe
		timestamp time.Time
	}
	unique := make([]data.Candle, 0, len(candles))
	indexByKey := make(map[candleKey]int, len(candles))
	for _, candle := range candles {
		key := candleKey{
			ticker:    candle.Ticker,
			timeframe: candle.Timeframe,
			timestamp: candle.Timestamp.UTC(),
		}
		if index, ok := indexByKey[key]; ok {
			unique[index] = candle
			continue
		}
		indexByKey[key] = len(unique)
		unique = append(unique, candle)
	}
	candles = unique

	candles, rejected := data.SanitizeCandles(candles)
	for _, r := range rejected {
		logger.Warn("rejected corrupt candle",
			"ticker", r.Candle.Ticker,
			"timeframe", string(r.Candle.Timeframe),
			"ts", r.Candle.Timestamp.UTC().Format(time.RFC3339),
			"reason", r.Reason,
			"detail", r.Detail,
		)
	}
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
	q += ` ON CONFLICT (ticker, timeframe, ts) DO UPDATE SET
		open=EXCLUDED.open,
		high=EXCLUDED.high,
		low=EXCLUDED.low,
		close=EXCLUDED.close,
		volume=EXCLUDED.volume,
		vwap=EXCLUDED.vwap
		WHERE (candles.open, candles.high, candles.low, candles.close,
		       candles.volume, candles.vwap)
		      IS DISTINCT FROM
		      (EXCLUDED.open, EXCLUDED.high, EXCLUDED.low, EXCLUDED.close,
		       EXCLUDED.volume, EXCLUDED.vwap)`
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
