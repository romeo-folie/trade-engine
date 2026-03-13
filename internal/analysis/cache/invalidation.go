package cache

import (
	"context"
	"time"

	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// InvalidationTrigger describes the reason a cache entry was invalidated.
type InvalidationTrigger string

const (
	TriggerPriceMove     InvalidationTrigger = "price_move"
	TriggerIndicatorFlip InvalidationTrigger = "indicator_regime_flip"
	TriggerHighImpactNews InvalidationTrigger = "high_impact_news"
	TriggerRiskEvent     InvalidationTrigger = "risk_event"
	TriggerTradeLifecycle InvalidationTrigger = "trade_lifecycle"
	TriggerTTLExpiry     InvalidationTrigger = "ttl_expiry"
)

// InvalidationInput carries the signals checked against an existing cache entry.
type InvalidationInput struct {
	Fingerprint string

	// PriceMoveThresholdPct: invalidate if abs(current-cached)/cached > threshold.
	CachedPrice     float64
	CurrentPrice    float64
	PriceMoveThresh float64 // default 0.75 %

	// IndicatorRegimeFlip: invalidate when RSI crosses a zone boundary.
	CachedRSIBucket  int
	CurrentRSIBucket int

	// High-impact news: invalidate when news hash changed and articles > threshold.
	NewsChanged         bool
	NewsArticleCount    int
	HighImpactNewsThresh int // default 3 articles

	// Risk event: open position count changed since cached.
	CachedOpenPositions  int
	CurrentOpenPositions int

	// Trade lifecycle: a trade was executed or closed since cached.
	TradeExecuted bool
}

// ShouldInvalidate checks all invalidation triggers and returns the first
// matching trigger (or "" if the entry is still valid).
func ShouldInvalidate(in InvalidationInput) InvalidationTrigger {
	// Price move.
	if in.CachedPrice > 0 {
		move := abs((in.CurrentPrice - in.CachedPrice) / in.CachedPrice * 100)
		thresh := in.PriceMoveThresh
		if thresh <= 0 {
			thresh = 0.75
		}
		if move >= thresh {
			return TriggerPriceMove
		}
	}

	// Indicator regime flip (RSI bucket crossed).
	if in.CachedRSIBucket != in.CurrentRSIBucket {
		return TriggerIndicatorFlip
	}

	// High-impact news burst.
	threshold := in.HighImpactNewsThresh
	if threshold <= 0 {
		threshold = 3
	}
	if in.NewsChanged && in.NewsArticleCount >= threshold {
		return TriggerHighImpactNews
	}

	// Risk event: position count changed.
	if in.CachedOpenPositions != in.CurrentOpenPositions {
		return TriggerRiskEvent
	}

	// Trade lifecycle.
	if in.TradeExecuted {
		return TriggerTradeLifecycle
	}

	return ""
}

// Sweeper periodically deletes expired cache entries.
type Sweeper struct {
	interval time.Duration
}

// NewSweeper creates a sweeper that runs every interval.
func NewSweeper(interval time.Duration) *Sweeper {
	return &Sweeper{interval: interval}
}

// Run starts the expiry sweep loop. Blocks until ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			n, err := store.DeleteExpiredCachedAnalyses(ctx, t)
			if err != nil {
				logger.Warn("cache sweep failed", "err", err.Error())
				RecordError()
			} else if n > 0 {
				logger.Info("cache sweep: deleted expired entries", "count", n)
				RecordEviction(int64(n))
			}
		}
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
