package scanner

import (
	"math"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

func TestDailyReturnPearsonUsesTrailing60Returns(t *testing.T) {
	old := make([]float64, 10)
	for i := range old {
		old[i] = 0.01
		if i%2 == 0 {
			old[i] = -0.01
		}
	}
	recent := make([]float64, correlationLookbackDays)
	for i := range recent {
		recent[i] = 0.01
		if i%2 == 0 {
			recent[i] = -0.005
		}
	}
	returnsA := append(old, recent...)
	returnsB := append([]float64(nil), returnsA...)

	a := correlationTestCandlesFromReturns("A", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), returnsA)
	b := correlationTestCandlesFromReturns("B", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), returnsB)
	correlation, ok := dailyReturnPearson(a, b)
	if !ok {
		t.Fatal("dailyReturnPearson returned no result")
	}
	if math.Abs(correlation-1) > 1e-12 {
		t.Fatalf("correlation = %.16f, want 1", correlation)
	}
}

func TestDailyReturnPearsonRequires30OverlappingReturns(t *testing.T) {
	returns := make([]float64, correlationMinOverlap)
	for i := range returns {
		returns[i] = 0.01
		if i%2 == 0 {
			returns[i] = -0.005
		}
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tooShortA := correlationTestCandlesFromReturns("A", start, returns[:correlationMinOverlap-1])
	tooShortB := correlationTestCandlesFromReturns("B", start, returns[:correlationMinOverlap-1])
	if _, ok := dailyReturnPearson(tooShortA, tooShortB); ok {
		t.Fatal("expected fewer than 30 returns to fail closed")
	}

	a := correlationTestCandlesFromReturns("A", start, returns)
	b := correlationTestCandlesFromReturns("B", start, returns)
	if correlation, ok := dailyReturnPearson(a, b); !ok || math.Abs(correlation-1) > 1e-12 {
		t.Fatalf("30-return correlation = %.16f, ok=%v; want 1,true", correlation, ok)
	}
}

func TestDailyReturnPearsonSkipsUnalignedMiddleBars(t *testing.T) {
	returns := make([]float64, correlationMinOverlap)
	for i := range returns {
		returns[i] = .01
		if i%2 == 0 {
			returns[i] = -.005
		}
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := correlationTestCandlesFromReturns("A", start, returns)
	b := correlationTestCandlesFromReturns("B", start, returns)
	// Remove one middle bar from B. The two neighboring observations must not
	// be turned into a single multi-day return.
	b = append(b[:16], b[17:]...)
	if _, ok := dailyReturnPearson(a, b); ok {
		t.Fatal("expected unaligned middle bar to reduce usable overlap below the minimum")
	}
}

func TestDailyReturnPearsonFailsClosedWithoutOverlappingDays(t *testing.T) {
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	a := correlationTestCandlesFromReturns("A", start, []float64{0.01, -0.005})
	b := correlationTestCandlesFromReturns("B", start.AddDate(0, 0, 10), []float64{0.01, -0.005})
	if _, ok := dailyReturnPearson(a, b); ok {
		t.Fatal("expected no overlapping days to fail closed")
	}
}

func TestDailyReturnCorrelationIsOrderIndependentAndHandlesDuplicateDays(t *testing.T) {
	returns := make([]float64, 35)
	for i := range returns {
		returns[i] = 0.01
		if i%3 == 0 {
			returns[i] = -0.004
		}
	}
	start := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	a := correlationTestCandlesFromReturns("A", start, returns)
	b := correlationTestCandlesFromReturns("B", start, returns)

	// A same-day duplicate with a later timestamp is selected consistently,
	// regardless of whether the duplicate appears before or after the original.
	duplicate := a[12]
	duplicate.Timestamp = duplicate.Timestamp.Add(6 * time.Hour)
	duplicate.Close = a[12].Close
	a = append(a, duplicate)

	correlation, ok := dailyReturnCorrelation(a, b, correlationMinOverlap)
	if !ok || math.Abs(correlation-1) > 1e-12 {
		t.Fatalf("correlation = %.16f, ok=%v; want 1,true", correlation, ok)
	}

	// Reverse both inputs to pin that callers may provide provider output in
	// either order without changing the result.
	reverseCandles(a)
	reverseCandles(b)
	reversed, reversedOK := dailyReturnCorrelation(a, b, correlationMinOverlap)
	if !reversedOK || math.Abs(reversed-correlation) > 1e-12 {
		t.Fatalf("reversed correlation = %.16f, ok=%v; got %.16f,true", reversed, reversedOK, correlation)
	}
}

func TestDailyReturnPearsonRejectsConstantAndInvalidSeries(t *testing.T) {
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	constant := make([]float64, correlationMinOverlap)
	for i := range constant {
		constant[i] = 0.01
	}
	a := correlationTestCandlesFromReturns("A", start, constant)
	b := correlationTestCandlesFromReturns("B", start, constant)
	if _, ok := dailyReturnPearson(a, b); ok {
		t.Fatal("expected constant return series to fail closed")
	}

	varied := make([]float64, correlationMinOverlap)
	for i := range varied {
		varied[i] = 0.01
		if i%2 == 0 {
			varied[i] = -0.005
		}
	}
	a = correlationTestCandlesFromReturns("A", start, varied)
	b = correlationTestCandlesFromReturns("B", start, varied)
	a[10].Close = math.NaN()
	// Invalid closes are ignored; after dropping one date this has only 29
	// usable returns and therefore cannot meet the minimum overlap.
	if _, ok := dailyReturnPearson(a, b); ok {
		t.Fatal("expected insufficient overlap to fail closed")
	}
}

func correlationTestCandlesFromReturns(ticker string, start time.Time, returns []float64) []data.Candle {
	candles := make([]data.Candle, 0, len(returns)+1)
	price := 100.0
	for i := 0; i <= len(returns); i++ {
		candles = append(candles, data.Candle{
			Ticker: ticker, Timeframe: data.Timeframe1Day,
			Timestamp: start.AddDate(0, 0, i), Close: price,
		})
		if i < len(returns) {
			price *= 1 + returns[i]
		}
	}
	return candles
}

func reverseCandles(candles []data.Candle) {
	for left, right := 0, len(candles)-1; left < right; left, right = left+1, right-1 {
		candles[left], candles[right] = candles[right], candles[left]
	}
}
