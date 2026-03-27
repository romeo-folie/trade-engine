package engine

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

func TestResolveFallbackCandleRespectsFreshness(t *testing.T) {
	now := time.Date(2026, 3, 27, 12, 0, 0, 0, time.UTC)
	candles := map[data.Timeframe]*data.Candle{
		data.Timeframe5Min:  {Timeframe: data.Timeframe5Min, Close: 10, Timestamp: now.Add(-2 * time.Hour)},
		data.Timeframe15Min: {Timeframe: data.Timeframe15Min, Close: 11, Timestamp: now.Add(-90 * time.Minute)},
		data.Timeframe1Hour: {Timeframe: data.Timeframe1Hour, Close: 12, Timestamp: now.Add(-2 * time.Hour)},
		data.Timeframe1Day:  {Timeframe: data.Timeframe1Day, Close: 13, Timestamp: now.Add(-24 * time.Hour)},
	}

	got := resolveFallbackCandle(now, func(tf data.Timeframe) (*data.Candle, error) {
		return candles[tf], nil
	})
	if got == nil {
		t.Fatalf("expected fallback candle, got nil")
	}
	if got.Timeframe != data.Timeframe15Min {
		t.Fatalf("expected 15m candle, got %s", got.Timeframe)
	}
}

func TestResolveFallbackCandleReturnsNilWhenStale(t *testing.T) {
	now := time.Date(2026, 3, 27, 12, 0, 0, 0, time.UTC)
	candles := map[data.Timeframe]*data.Candle{
		data.Timeframe5Min:  {Timeframe: data.Timeframe5Min, Close: 10, Timestamp: now.Add(-3 * time.Hour)},
		data.Timeframe15Min: {Timeframe: data.Timeframe15Min, Close: 11, Timestamp: now.Add(-3 * time.Hour)},
		data.Timeframe1Hour: {Timeframe: data.Timeframe1Hour, Close: 12, Timestamp: now.Add(-10 * time.Hour)},
		data.Timeframe1Day:  {Timeframe: data.Timeframe1Day, Close: 13, Timestamp: now.Add(-72 * time.Hour)},
	}

	got := resolveFallbackCandle(now, func(tf data.Timeframe) (*data.Candle, error) {
		return candles[tf], nil
	})
	if got != nil {
		t.Fatalf("expected nil fallback, got %s", got.Timeframe)
	}
}

func TestIsFreshCandleRejectsFutureTimestamps(t *testing.T) {
	now := time.Date(2026, 3, 27, 12, 0, 0, 0, time.UTC)
	if isFreshCandle(data.Timeframe5Min, now.Add(10*time.Minute), now) {
		t.Fatalf("expected future candle to be rejected")
	}
}

func TestIsFreshCandleInclusiveAtMaxAge(t *testing.T) {
	now := time.Date(2026, 3, 27, 12, 0, 0, 0, time.UTC)
	maxAge := maxFallbackCandleAge(data.Timeframe1Hour)
	if maxAge <= 0 {
		t.Fatalf("expected max age to be positive")
	}
	if !isFreshCandle(data.Timeframe1Hour, now.Add(-maxAge), now) {
		t.Fatalf("expected candle at max age to be accepted")
	}
}
