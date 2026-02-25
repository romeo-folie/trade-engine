package indicators

import (
	"math"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

// approx returns true if a and b are within epsilon of each other.
func approx(a, b, epsilon float64) bool {
	return math.Abs(a-b) <= epsilon
}

// ── SMA ───────────────────────────────────────────────────────────────────────

func TestSMA_Basic(t *testing.T) {
	data := []float64{1, 2, 3, 4, 5}
	got := sma(data, 3)
	want := 4.0 // (3+4+5)/3
	if !approx(got, want, 0.0001) {
		t.Errorf("sma(3) = %.4f, want %.4f", got, want)
	}
}

func TestSMA_ExactPeriod(t *testing.T) {
	data := []float64{10, 20, 30}
	got := sma(data, 3)
	want := 20.0
	if !approx(got, want, 0.0001) {
		t.Errorf("sma full = %.4f, want %.4f", got, want)
	}
}

func TestSMA_InsufficientData(t *testing.T) {
	data := []float64{1, 2}
	got := sma(data, 5)
	if got != 0 {
		t.Errorf("sma with insufficient data = %.4f, want 0", got)
	}
}

// ── EMA ───────────────────────────────────────────────────────────────────────

func TestEMA_Basic(t *testing.T) {
	// EMA3 of [1,2,3,4,5]:
	// seed = (1+2+3)/3 = 2.0; k = 2/(3+1) = 0.5
	// EMA after 4: 4*0.5 + 2*0.5 = 3.0
	// EMA after 5: 5*0.5 + 3*0.5 = 4.0
	data := []float64{1, 2, 3, 4, 5}
	got := ema(data, 3)
	want := 4.0
	if !approx(got, want, 0.0001) {
		t.Errorf("ema(3) = %.4f, want %.4f", got, want)
	}
}

func TestEMA_InsufficientData(t *testing.T) {
	data := []float64{1, 2}
	got := ema(data, 5)
	if got != 0 {
		t.Errorf("ema with insufficient data = %.4f, want 0", got)
	}
}

func TestEMA_ConvergesNearPrice(t *testing.T) {
	// With enough constant data, EMA should converge to that constant.
	n := 100
	data := make([]float64, n)
	for i := range data {
		data[i] = 50.0
	}
	got := ema(data, 20)
	if !approx(got, 50.0, 0.01) {
		t.Errorf("ema on constant series = %.4f, want ~50", got)
	}
}

// ── RSI ───────────────────────────────────────────────────────────────────────

func TestRSI_AllGains(t *testing.T) {
	// Strictly increasing series → RSI = 100
	data := make([]float64, 20)
	for i := range data {
		data[i] = float64(i + 1)
	}
	got := rsi(data, 14)
	if !approx(got, 100, 0.01) {
		t.Errorf("rsi all gains = %.4f, want 100", got)
	}
}

func TestRSI_AllLosses(t *testing.T) {
	// Strictly decreasing → RSI approaches 0
	data := make([]float64, 20)
	for i := range data {
		data[i] = float64(20 - i)
	}
	got := rsi(data, 14)
	if got >= 10 {
		t.Errorf("rsi all losses = %.4f, want near 0", got)
	}
}

func TestRSI_InRange(t *testing.T) {
	// Oscillating series — RSI must be between 0 and 100.
	data := []float64{
		44.34, 44.09, 44.15, 43.61, 44.33, 44.83, 45.10, 45.15,
		43.61, 44.33, 44.83, 45.10, 45.15, 43.61, 44.34, 44.09,
		44.15, 43.61, 44.33, 44.83,
	}
	got := rsi(data, 14)
	if got < 0 || got > 100 {
		t.Errorf("rsi = %.4f, expected in [0, 100]", got)
	}
}

func TestRSI_InsufficientData(t *testing.T) {
	data := []float64{1, 2, 3}
	got := rsi(data, 14)
	if got != 0 {
		t.Errorf("rsi insufficient = %.4f, want 0", got)
	}
}

// ── MACD ──────────────────────────────────────────────────────────────────────

func TestMACD_ReturnsValues(t *testing.T) {
	// Build a 50-element series with enough data for MACD(12,26,9).
	data := make([]float64, 50)
	for i := range data {
		data[i] = 100 + float64(i)*0.5 // gentle uptrend
	}
	line, signal, hist := macd(data, 12, 26, 9)

	// In an uptrend, MACD line should be positive (fast EMA > slow EMA).
	if line <= 0 {
		t.Errorf("macd line = %.4f, want > 0 in uptrend", line)
	}
	// Histogram = line - signal
	if !approx(hist, line-signal, 0.001) {
		t.Errorf("histogram %.4f != line-signal %.4f", hist, line-signal)
	}
}

func TestMACD_InsufficientData(t *testing.T) {
	data := []float64{1, 2, 3}
	l, s, h := macd(data, 12, 26, 9)
	if l != 0 || s != 0 || h != 0 {
		t.Errorf("macd insufficient: got (%.4f,%.4f,%.4f), want (0,0,0)", l, s, h)
	}
}

// ── Bollinger Bands ───────────────────────────────────────────────────────────

func TestBollingerBands_Structure(t *testing.T) {
	data := make([]float64, 30)
	for i := range data {
		data[i] = 100 + float64(i%5)*0.1
	}
	upper, mid, lower := bollingerBands(data, 20, 2.0)

	if upper <= mid {
		t.Errorf("upper (%.4f) should be > middle (%.4f)", upper, mid)
	}
	if mid <= lower {
		t.Errorf("middle (%.4f) should be > lower (%.4f)", mid, lower)
	}
}

func TestBollingerBands_ConstantSeries(t *testing.T) {
	// Constant price → stddev = 0, all three bands == price.
	data := make([]float64, 25)
	for i := range data {
		data[i] = 42.0
	}
	upper, mid, lower := bollingerBands(data, 20, 2.0)
	if !approx(upper, 42, 0.001) || !approx(mid, 42, 0.001) || !approx(lower, 42, 0.001) {
		t.Errorf("constant series BB: upper=%.4f mid=%.4f lower=%.4f, all want 42", upper, mid, lower)
	}
}

// ── ATR ───────────────────────────────────────────────────────────────────────

func TestATR_Positive(t *testing.T) {
	n := 30
	highs := make([]float64, n)
	lows := make([]float64, n)
	closes := make([]float64, n)
	for i := 0; i < n; i++ {
		closes[i] = 100 + float64(i)
		highs[i] = closes[i] + 2
		lows[i] = closes[i] - 2
	}
	got := atr(highs, lows, closes, 14)
	if got <= 0 {
		t.Errorf("atr = %.4f, want > 0", got)
	}
}

func TestATR_InsufficientData(t *testing.T) {
	h := []float64{1, 2}
	l := []float64{0.5, 1.5}
	c := []float64{0.8, 1.8}
	got := atr(h, l, c, 14)
	if got != 0 {
		t.Errorf("atr insufficient = %.4f, want 0", got)
	}
}

// ── OBV ───────────────────────────────────────────────────────────────────────

func TestOBV_AllUpDays(t *testing.T) {
	closes := []float64{10, 11, 12, 13, 14}
	volumes := []float64{100, 200, 150, 300, 250}
	// OBV = 100 + 200 + 150 + 300 + 250 = 1000
	got := obv(closes, volumes)
	want := 1000.0
	if !approx(got, want, 0.01) {
		t.Errorf("obv all up = %.2f, want %.2f", got, want)
	}
}

func TestOBV_MixedDays(t *testing.T) {
	closes := []float64{10, 11, 10, 12}
	volumes := []float64{100, 200, 150, 300}
	// OBV = 100 + 200 - 150 + 300 = 450
	got := obv(closes, volumes)
	want := 450.0
	if !approx(got, want, 0.01) {
		t.Errorf("obv mixed = %.2f, want %.2f", got, want)
	}
}

// ── Stochastic ────────────────────────────────────────────────────────────────

func TestStochastic_InRange(t *testing.T) {
	n := 30
	highs := make([]float64, n)
	lows := make([]float64, n)
	closes := make([]float64, n)
	for i := 0; i < n; i++ {
		closes[i] = 100 + float64(i%10)
		highs[i] = closes[i] + 3
		lows[i] = closes[i] - 3
	}
	k, d := stochastic(highs, lows, closes, 14, 3, 3)
	if k < 0 || k > 100 {
		t.Errorf("stoch %%K = %.4f, expected [0, 100]", k)
	}
	if d < 0 || d > 100 {
		t.Errorf("stoch %%D = %.4f, expected [0, 100]", d)
	}
}

// ── VWAP ─────────────────────────────────────────────────────────────────────

func TestVWAP_Simple(t *testing.T) {
	// One bar: H=12, L=10, C=11 → TP=11, Vol=100 → VWAP=11
	highs := []float64{12}
	lows := []float64{10}
	closes := []float64{11}
	volumes := []float64{100}
	got := vwap(highs, lows, closes, volumes)
	want := 11.0
	if !approx(got, want, 0.001) {
		t.Errorf("vwap single bar = %.4f, want %.4f", got, want)
	}
}

func TestVWAP_ZeroVolume(t *testing.T) {
	got := vwap([]float64{12}, []float64{10}, []float64{11}, []float64{0})
	if got != 0 {
		t.Errorf("vwap zero volume = %.4f, want 0", got)
	}
}

// ── ADX ───────────────────────────────────────────────────────────────────────

func TestADX_InRange(t *testing.T) {
	n := 60
	highs := make([]float64, n)
	lows := make([]float64, n)
	closes := make([]float64, n)
	for i := 0; i < n; i++ {
		closes[i] = 100 + float64(i)*0.5
		highs[i] = closes[i] + 1.5
		lows[i] = closes[i] - 1.5
	}
	got := adx(highs, lows, closes, 14)
	if got < 0 || got > 100 {
		t.Errorf("adx = %.4f, expected [0, 100]", got)
	}
}

// ── Compute integration test ──────────────────────────────────────────────────

func TestCompute_WithCandles(t *testing.T) {
	// Build 220 synthetic candles — enough history for EMA200 and ADX.
	n := 220
	candles := make([]data.Candle, n)
	base := time.Now().Add(-time.Duration(n) * 24 * time.Hour)
	for i := 0; i < n; i++ {
		c := 100 + float64(i%20)*0.5 // gentle oscillating uptrend
		candles[i] = data.Candle{
			Ticker:    "TEST",
			Timeframe: data.Timeframe1Day,
			Timestamp: base.Add(time.Duration(i) * 24 * time.Hour),
			Open:      c - 0.5,
			High:      c + 2,
			Low:       c - 2,
			Close:     c,
			Volume:    int64(1_000_000 + i*1000),
			VWAP:      c,
		}
	}

	set, err := Compute(candles)
	if err != nil {
		t.Fatalf("Compute returned error: %v", err)
	}

	// All EMAs should be non-zero and close to the recent close (~100–110).
	for _, pair := range []struct {
		name string
		val  float64
	}{
		{"EMA20", set.EMA20}, {"EMA50", set.EMA50}, {"EMA200", set.EMA200},
		{"SMA20", set.SMA20}, {"SMA50", set.SMA50}, {"SMA200", set.SMA200},
	} {
		if pair.val <= 0 {
			t.Errorf("%s = %.4f, want > 0", pair.name, pair.val)
		}
	}

	if set.RSI < 0 || set.RSI > 100 {
		t.Errorf("RSI = %.4f, expected [0, 100]", set.RSI)
	}
	if set.ADX < 0 || set.ADX > 100 {
		t.Errorf("ADX = %.4f, expected [0, 100]", set.ADX)
	}
	if set.ATR <= 0 {
		t.Errorf("ATR = %.4f, expected > 0", set.ATR)
	}
	if set.VWAP <= 0 {
		t.Errorf("VWAP = %.4f, expected > 0", set.VWAP)
	}
	if set.VolumeSMA20 <= 0 {
		t.Errorf("VolumeSMA20 = %.4f, expected > 0", set.VolumeSMA20)
	}

	t.Logf("RSI=%.2f EMA20=%.2f EMA200=%.2f MACD=%.4f ADX=%.2f ATR=%.4f OBV=%.0f VWAP=%.2f",
		set.RSI, set.EMA20, set.EMA200, set.MACDLine, set.ADX, set.ATR, set.OBV, set.VWAP)
}
