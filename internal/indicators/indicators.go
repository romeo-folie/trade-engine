// Package indicators computes technical analysis indicators from OHLCV candle data.
// All computation operates on a slice of candles; the last element is treated
// as the current bar. The caller is responsible for providing sufficient history
// (200 candles recommended for EMA200 to be meaningful).
package indicators

import "github.com/romeo-folie/trade-engine/internal/data"

// IndicatorSet holds all computed indicator values for a single bar.
// Zero values indicate insufficient data for that indicator.
type IndicatorSet struct {
	// EMA — Exponential Moving Average
	EMA20  float64
	EMA50  float64
	EMA200 float64

	// SMA — Simple Moving Average
	SMA20  float64
	SMA50  float64
	SMA200 float64

	// RSI — Relative Strength Index (period 14)
	RSI float64

	// MACD — (12, 26, 9)
	MACDLine      float64
	MACDSignal    float64
	MACDHistogram float64

	// Bollinger Bands — (20, 2σ)
	BBUpper  float64
	BBMiddle float64
	BBLower  float64

	// ADX — Average Directional Index (period 14)
	ADX float64

	// ATR — Average True Range (period 14)
	ATR float64

	// OBV — On-Balance Volume (running total)
	OBV float64

	// Stochastic Oscillator — (14, 3, 3): slow %K and %D
	StochK float64
	StochD float64

	// VWAP — Volume-Weighted Average Price (cumulative over input slice)
	VWAP float64

	// Volume SMA (period 20)
	VolumeSMA20 float64
}

// Compute calculates all indicators from the provided candle slice and returns
// an IndicatorSet. Indicators that require more data than is available are left
// as zero. candles must be in ascending time order.
func Compute(candles []data.Candle) (*IndicatorSet, error) {
	n := len(candles)
	if n == 0 {
		return &IndicatorSet{}, nil
	}

	set := &IndicatorSet{}

	// Pre-extract price/volume slices once.
	closes := make([]float64, n)
	highs := make([]float64, n)
	lows := make([]float64, n)
	volumes := make([]float64, n)

	for i, c := range candles {
		closes[i] = c.Close
		highs[i] = c.High
		lows[i] = c.Low
		volumes[i] = float64(c.Volume)
	}

	set.EMA20 = ema(closes, 20)
	set.EMA50 = ema(closes, 50)
	set.EMA200 = ema(closes, 200)

	set.SMA20 = sma(closes, 20)
	set.SMA50 = sma(closes, 50)
	set.SMA200 = sma(closes, 200)

	set.RSI = rsi(closes, 14)

	set.MACDLine, set.MACDSignal, set.MACDHistogram = macd(closes, 12, 26, 9)

	set.BBUpper, set.BBMiddle, set.BBLower = bollingerBands(closes, 20, 2.0)

	set.ADX = adx(highs, lows, closes, 14)

	set.ATR = atr(highs, lows, closes, 14)

	set.OBV = obv(closes, volumes)

	set.StochK, set.StochD = stochastic(highs, lows, closes, 14, 3, 3)

	set.VWAP = vwap(highs, lows, closes, volumes)

	set.VolumeSMA20 = sma(volumes, 20)

	return set, nil
}
