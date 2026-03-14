package cache_test

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/analysis/cache"
)

func baseInvalidation() cache.InvalidationInput {
	return cache.InvalidationInput{
		Fingerprint:          "fp1",
		CachedPrice:         100.0,
		CurrentPrice:        100.0,
		PriceMoveThresh:     0.75,
		CachedRSIBucket:     50,
		CurrentRSIBucket:    50,
		NewsChanged:         false,
		NewsArticleCount:    0,
		HighImpactNewsThresh: 3,
		CachedOpenPositions: 2,
		CurrentOpenPositions: 2,
		TradeExecuted:       false,
	}
}

func TestNoInvalidationWhenAllStable(t *testing.T) {
	trigger := cache.ShouldInvalidate(baseInvalidation())
	if trigger != "" {
		t.Errorf("expected no invalidation, got %q", trigger)
	}
}

func TestPriceMoveInvalidation(t *testing.T) {
	in := baseInvalidation()
	in.CurrentPrice = 100.80 // 0.8 % > 0.75 % threshold
	trigger := cache.ShouldInvalidate(in)
	if trigger != cache.TriggerPriceMove {
		t.Errorf("expected price_move, got %q", trigger)
	}
}

func TestPriceMoveNoInvalidationBelow(t *testing.T) {
	in := baseInvalidation()
	in.CurrentPrice = 100.70 // 0.7 % < threshold
	trigger := cache.ShouldInvalidate(in)
	if trigger == cache.TriggerPriceMove {
		t.Error("should not invalidate below price move threshold")
	}
}

func TestIndicatorRegimeFlip(t *testing.T) {
	in := baseInvalidation()
	in.CurrentRSIBucket = 55 // crossed bucket boundary
	trigger := cache.ShouldInvalidate(in)
	if trigger != cache.TriggerIndicatorFlip {
		t.Errorf("expected indicator_regime_flip, got %q", trigger)
	}
}

func TestHighImpactNewsInvalidation(t *testing.T) {
	in := baseInvalidation()
	in.NewsChanged = true
	in.NewsArticleCount = 3
	trigger := cache.ShouldInvalidate(in)
	if trigger != cache.TriggerHighImpactNews {
		t.Errorf("expected high_impact_news, got %q", trigger)
	}
}

func TestHighImpactNewsNoInvalidationBelowThresh(t *testing.T) {
	in := baseInvalidation()
	in.NewsChanged = true
	in.NewsArticleCount = 2 // below threshold of 3
	trigger := cache.ShouldInvalidate(in)
	if trigger == cache.TriggerHighImpactNews {
		t.Error("should not invalidate when article count below threshold")
	}
}

func TestNewsChangedButLowCountNoInvalidation(t *testing.T) {
	in := baseInvalidation()
	in.NewsChanged = true
	in.NewsArticleCount = 1
	trigger := cache.ShouldInvalidate(in)
	if trigger != "" {
		t.Errorf("expected no invalidation for low-count news change, got %q", trigger)
	}
}

func TestRiskEventInvalidation(t *testing.T) {
	in := baseInvalidation()
	in.CurrentOpenPositions = 3 // changed from 2
	trigger := cache.ShouldInvalidate(in)
	if trigger != cache.TriggerRiskEvent {
		t.Errorf("expected risk_event, got %q", trigger)
	}
}

func TestTradeLifecycleInvalidation(t *testing.T) {
	in := baseInvalidation()
	in.TradeExecuted = true
	trigger := cache.ShouldInvalidate(in)
	if trigger != cache.TriggerTradeLifecycle {
		t.Errorf("expected trade_lifecycle, got %q", trigger)
	}
}

func TestDefaultThresholdsApplied(t *testing.T) {
	in := baseInvalidation()
	in.PriceMoveThresh = 0 // zero → default 0.75
	in.CurrentPrice = 100.80
	trigger := cache.ShouldInvalidate(in)
	if trigger != cache.TriggerPriceMove {
		t.Errorf("default price threshold should be 0.75%%, got trigger=%q", trigger)
	}
}
