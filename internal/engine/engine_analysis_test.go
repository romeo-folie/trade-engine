package engine

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

func TestVolRatioUsesLatestCandleVolume(t *testing.T) {
	candles := []data.Candle{
		{Timestamp: time.Unix(1, 0), Volume: 100},
		{Timestamp: time.Unix(2, 0), Volume: 250},
	}
	got := volRatio(candles, 125)
	want := 2.0 // latest(250) / 125
	if got != want {
		t.Fatalf("volRatio = %.2f, want %.2f", got, want)
	}
}

func TestVolRatioFallbackWhenNoData(t *testing.T) {
	if got := volRatio(nil, 0); got != 1.0 {
		t.Fatalf("volRatio fallback = %.2f, want 1.0", got)
	}
}
