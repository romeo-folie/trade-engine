package scanner

import (
	"math"
	"sort"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

const (
	// Correlation lookback is expressed in return observations. Computing a
	// return needs the close from the preceding day, so a complete window has
	// lookback+1 closes.
	correlationLookbackDays = 60
	correlationMinOverlap   = 30
)

// dailyReturnCorrelation returns the Pearson correlation of daily close-to-
// close returns over the trailing correlationLookbackDays shared observations.
// It returns false when the pair has fewer than minOverlap shared returns or
// either return series has no variance. The inputs are not mutated, and their
// order does not affect the result.
//
// minOverlap is the number of paired returns (rather than the number of price
// bars) required for a result. For the promotion policy use
// dailyReturnPearson, which applies the 60-day/30-return defaults.
func dailyReturnCorrelation(a, b []data.Candle, minOverlap int) (float64, bool) {
	if minOverlap <= 0 {
		return 0, false
	}
	return dailyReturnPearsonWindow(a, b, correlationLookbackDays, minOverlap)
}

// dailyReturnPearson applies the scanner's standard correlation policy: up to
// 60 trailing daily return observations and at least 30 shared observations.
func dailyReturnPearson(a, b []data.Candle) (float64, bool) {
	return dailyReturnPearsonWindow(a, b, correlationLookbackDays, correlationMinOverlap)
}

// dailyReturnPearsonWindow is kept separate from the policy wrapper so the
// minimum-overlap behavior can be exercised independently in package tests.
func dailyReturnPearsonWindow(a, b []data.Candle, lookback, minOverlap int) (float64, bool) {
	if lookback <= 0 || minOverlap <= 0 || minOverlap > lookback {
		return 0, false
	}

	pricesA := dailyCloseMap(a)
	pricesB := dailyCloseMap(b)
	if len(pricesA) == 0 || len(pricesB) == 0 {
		return 0, false
	}

	// Build the intersection before applying the lookback. This makes the
	// window genuinely pairwise: a missing bar in one ticker cannot cause the
	// other ticker's older, non-overlapping bars to consume the window.
	days := make([]time.Time, 0, correlationMinInt(len(pricesA), len(pricesB)))
	for day := range pricesA {
		if _, ok := pricesB[day]; ok {
			days = append(days, day)
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	if len(days) > lookback+1 {
		days = days[len(days)-(lookback+1):]
	}
	if len(days) < minOverlap+1 {
		return 0, false
	}

	returnsA := make([]float64, 0, len(days)-1)
	returnsB := make([]float64, 0, len(days)-1)
	for i := 1; i < len(days); i++ {
		prior, current := days[i-1], days[i]
		previousA, currentA := pricesA[prior], pricesA[current]
		previousB, currentB := pricesB[prior], pricesB[current]
		if previousA <= 0 || currentA <= 0 || previousB <= 0 || currentB <= 0 {
			continue
		}
		returnA := currentA/previousA - 1
		returnB := currentB/previousB - 1
		if !correlationFinite(returnA) || !correlationFinite(returnB) {
			continue
		}
		returnsA = append(returnsA, returnA)
		returnsB = append(returnsB, returnB)
	}
	if len(returnsA) < minOverlap {
		return 0, false
	}

	// The slices are already in date order. Keep only the latest observations
	// if invalid prices reduced the usable count inside the selected window.
	if len(returnsA) > lookback {
		returnsA = returnsA[len(returnsA)-lookback:]
		returnsB = returnsB[len(returnsB)-lookback:]
	}
	return pearson(returnsA, returnsB, minOverlap)
}

func dailyCloseMap(candles []data.Candle) map[time.Time]float64 {
	selected := make(map[time.Time]data.Candle, len(candles))
	for _, candle := range candles {
		if candle.Timestamp.IsZero() || candle.Close <= 0 || !correlationFinite(candle.Close) {
			continue
		}
		day := utcDay(candle.Timestamp)
		if previous, ok := selected[day]; !ok || candleAfter(previous, candle) {
			selected[day] = candle
		}
	}
	prices := make(map[time.Time]float64, len(selected))
	for day, candle := range selected {
		prices[day] = candle.Close
	}
	return prices
}

// candleAfter selects a duplicate-day value without depending on input order.
// Daily providers should return one bar per day; when they do not, use the
// latest timestamp and then stable OHLCV fields as tie-breakers.
func candleAfter(previous, candidate data.Candle) bool {
	previousTimestamp, candidateTimestamp := previous.Timestamp.UTC(), candidate.Timestamp.UTC()
	if !candidateTimestamp.Equal(previousTimestamp) {
		return candidateTimestamp.After(previousTimestamp)
	}
	if candidate.Close != previous.Close {
		return candidate.Close > previous.Close
	}
	if candidate.Open != previous.Open {
		return candidate.Open > previous.Open
	}
	if candidate.High != previous.High {
		return candidate.High > previous.High
	}
	if candidate.Low != previous.Low {
		return candidate.Low > previous.Low
	}
	if candidate.Volume != previous.Volume {
		return candidate.Volume > previous.Volume
	}
	return candidate.VWAP > previous.VWAP
}

func utcDay(timestamp time.Time) time.Time {
	utc := timestamp.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

func pearson(x, y []float64, minOverlap int) (float64, bool) {
	if len(x) != len(y) || len(x) < minOverlap || len(x) < 2 {
		return 0, false
	}
	var meanX, meanY float64
	for i := range x {
		meanX += x[i]
		meanY += y[i]
	}
	meanX /= float64(len(x))
	meanY /= float64(len(y))

	var covariance, varianceX, varianceY float64
	for i := range x {
		deltaX, deltaY := x[i]-meanX, y[i]-meanY
		covariance += deltaX * deltaY
		varianceX += deltaX * deltaX
		varianceY += deltaY * deltaY
	}
	if varianceX <= 0 || varianceY <= 0 {
		return 0, false
	}
	correlation := covariance / math.Sqrt(varianceX*varianceY)
	if !correlationFinite(correlation) {
		return 0, false
	}
	// Round-off can put a mathematically bounded value just outside [-1, 1].
	if correlation > 1 {
		correlation = 1
	} else if correlation < -1 {
		correlation = -1
	}
	return correlation, true
}

func correlationFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func correlationMinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
