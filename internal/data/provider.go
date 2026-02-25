// Package data defines the market data provider interfaces and shared types
// used across the TradeEngine data pipeline.
package data

import (
	"context"
	"time"
)

// Timeframe represents a candle interval (e.g. "1m", "5m", "1h", "1d").
type Timeframe string

const (
	Timeframe1Min  Timeframe = "1m"
	Timeframe5Min  Timeframe = "5m"
	Timeframe15Min Timeframe = "15m"
	Timeframe1Hour Timeframe = "1h"
	Timeframe1Day  Timeframe = "1d"
)

// Candle represents a single OHLCV bar from any market data provider.
// Fields map directly to the candles table in the database.
type Candle struct {
	Ticker    string
	Timeframe Timeframe
	Timestamp time.Time
	Open      float64
	High      float64
	Low       float64
	Close     float64
	Volume    int64
	VWAP      float64
}

// Quote represents the latest market quote for a ticker.
type Quote struct {
	Ticker    string
	Bid       float64
	Ask       float64
	Last      float64
	Timestamp time.Time
}

// MarketDataProvider is the interface all stock/market data adapters must satisfy.
// It allows swapping between Alpaca, mock data, and any future provider without
// changing the engine code.
type MarketDataProvider interface {
	// FetchCandles retrieves OHLCV bars for a ticker within a time range.
	FetchCandles(ctx context.Context, ticker string, tf Timeframe, from, to time.Time) ([]Candle, error)

	// FetchQuote retrieves the latest bid/ask/last quote for a ticker.
	FetchQuote(ctx context.Context, ticker string) (*Quote, error)

	// FetchHistory retrieves the most recent N candles for a ticker.
	FetchHistory(ctx context.Context, ticker string, tf Timeframe, limit int) ([]Candle, error)
}
