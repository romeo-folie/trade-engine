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

// ── Prediction Market Types ───────────────────────────────────────────────────

// Contract represents a prediction market contract retrieved from a platform.
type Contract struct {
	ID                 string
	Platform           string // "polymarket" | "kalshi"
	Title              string
	Categories         []string
	Price              float64 // mid-market implied probability (0–1)
	YesPrice           float64
	NoPrice            float64
	Volume24h          float64
	OpenInterest       float64
	Bid                float64
	Ask                float64
	ResolutionDeadline time.Time
	Resolved           bool
}

// ContractSnapshot is a point-in-time record for a contract, corresponding to
// a row in the prediction_snapshots table.
type ContractSnapshot struct {
	Platform           string
	ContractID         string
	ContractTitle      string
	Price              float64
	YesPrice           float64
	NoPrice            float64
	Volume24h          float64
	OpenInterest       float64
	Bid                float64
	Ask                float64
	ResolutionDeadline time.Time
	Resolved           bool
	ResolutionValue    *bool
	Categories         []string
	CreatedAt          time.Time
}

// PredictionDataProvider is the interface all prediction market adapters must
// implement. It mirrors MarketDataProvider's role for stock data.
type PredictionDataProvider interface {
	// FetchContracts retrieves active contracts matching the given categories,
	// filtered by minimum 24h volume, limited to maxContracts results.
	FetchContracts(ctx context.Context, categories []string, minVolume float64, maxContracts int) ([]Contract, error)

	// FetchSnapshot retrieves the current state of a single contract by its
	// platform-specific ID.
	FetchSnapshot(ctx context.Context, contractID string) (*ContractSnapshot, error)
}
