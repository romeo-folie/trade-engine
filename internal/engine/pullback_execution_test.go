package engine

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/data"
)

func TestMinimumCandleLowTracksInterveningBreach(t *testing.T) {
	got, ok := minimumCandleLow([]data.Candle{
		{Low: 101},
		{Low: 97.5},
		{Low: 103}, // later rebound must not erase the breach
	})
	if !ok || got != 97.5 {
		t.Fatalf("minimumCandleLow() = %.2f, %v; want 97.5, true", got, ok)
	}
}

func TestMinimumCandleLowRejectsMissingPricePath(t *testing.T) {
	if got, ok := minimumCandleLow([]data.Candle{{Low: 0}}); ok || got != 0 {
		t.Fatalf("minimumCandleLow() = %.2f, %v; want 0, false", got, ok)
	}
}
