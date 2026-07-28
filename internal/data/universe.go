package data

import (
	"context"
	"fmt"
	"time"
)

// DailyBarsBatchError identifies a provider failure for one requested batch.
// Callers that need an audit denominator can attribute it only to Symbols;
// the error still propagates so safety-sensitive consumers fail closed.
type DailyBarsBatchError struct {
	Symbols []string
	Err     error
}

func (e *DailyBarsBatchError) Error() string { return fmt.Sprintf("daily-bars batch: %v", e.Err) }
func (e *DailyBarsBatchError) Unwrap() error { return e.Err }

// StockAsset is provider-neutral evidence that a US equity existed and was
// available to the provider at ObservedAt. AssetID is the provider's durable
// identity; callers must not treat a ticker as immutable across corporate
// actions.
type StockAsset struct {
	AssetID      string
	Symbol       string
	Name         string
	Status       string
	AssetClass   string
	Exchange     string
	Tradable     bool
	Marginable   bool
	Shortable    bool
	Fractionable bool
	Attributes   []string
	Provider     string
	ObservedAt   time.Time
}

// AssetUniverseProvider supplies the provider's complete active US-equity
// master list. It is deliberately separate from MarketDataProvider so existing
// single-symbol adapters do not need to implement universe discovery.
type AssetUniverseProvider interface {
	FetchStockAssets(ctx context.Context) ([]StockAsset, error)
}

// StockSnapshotEvidence captures the price/volume subset needed to make a
// point-in-time eligibility decision. ObservedAt is when the provider response
// was obtained; individual candles retain their exchange timestamps.
type StockSnapshotEvidence struct {
	Symbol           string
	MinuteBar        *Candle
	DailyBar         *Candle
	PreviousDailyBar *Candle
	LatestTradePrice float64
	LatestTradeAt    time.Time
	LatestBidPrice   float64
	LatestAskPrice   float64
	LatestQuoteAt    time.Time
	Provider         string
	ObservedAt       time.Time
}

// StockSnapshotBatchResult keeps successful evidence and symbol-level failures
// together. A transport failure for one caller-sized batch does not erase
// successful batches.
type StockSnapshotBatchResult struct {
	Snapshots map[string]StockSnapshotEvidence
	Failures  map[string]error
}

// BulkStockSnapshotProvider fetches snapshots in batches no larger than
// batchSize. Callers choose the bound to control URL size and provider load.
type BulkStockSnapshotProvider interface {
	FetchStockSnapshots(
		ctx context.Context, symbols []string, batchSize int,
	) (StockSnapshotBatchResult, error)
}

// DailyBarsBatchResult has the same partial-success contract as snapshots.
// Bars for each successful symbol are deduplicated and sorted oldest-first.
type DailyBarsBatchResult struct {
	Bars     map[string][]Candle
	Failures map[string]error
}

// BulkDailyBarsProvider retrieves split-adjusted daily bars for an entire
// caller-bounded universe without widening MarketDataProvider.
type BulkDailyBarsProvider interface {
	FetchDailyBars(
		ctx context.Context,
		symbols []string,
		from, to time.Time,
		batchSize int,
	) (DailyBarsBatchResult, error)
}
