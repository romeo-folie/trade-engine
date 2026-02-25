package indicators

import "math"

// ── SMA ───────────────────────────────────────────────────────────────────────

// sma returns the Simple Moving Average of the last `period` values.
// Returns 0 if len(data) < period.
func sma(data []float64, period int) float64 {
	n := len(data)
	if n < period {
		return 0
	}
	sum := 0.0
	for _, v := range data[n-period:] {
		sum += v
	}
	return sum / float64(period)
}

// ── EMA ───────────────────────────────────────────────────────────────────────

// ema returns the Exponential Moving Average of data with the given period.
// Uses seed = SMA of first `period` values, then applies the EMA multiplier forward.
// Returns 0 if len(data) < period.
func ema(data []float64, period int) float64 {
	n := len(data)
	if n < period {
		return 0
	}
	k := 2.0 / float64(period+1)

	// Seed with SMA of the first `period` values.
	seed := 0.0
	for _, v := range data[:period] {
		seed += v
	}
	seed /= float64(period)

	curr := seed
	for _, v := range data[period:] {
		curr = v*k + curr*(1-k)
	}
	return curr
}

// emaFull returns a full EMA series (same length as data) for use in MACD.
// Values before the seed window are filled with the seed SMA.
func emaFull(data []float64, period int) []float64 {
	n := len(data)
	out := make([]float64, n)
	if n < period {
		return out
	}
	k := 2.0 / float64(period+1)

	seed := 0.0
	for _, v := range data[:period] {
		seed += v
	}
	seed /= float64(period)

	for i := 0; i < period; i++ {
		out[i] = seed
	}
	curr := seed
	for i := period; i < n; i++ {
		curr = data[i]*k + curr*(1-k)
		out[i] = curr
	}
	return out
}

// ── RSI ───────────────────────────────────────────────────────────────────────

// rsi returns the Relative Strength Index using Wilder's smoothing.
// Requires at least period+1 data points. Returns 0 on insufficient data.
func rsi(data []float64, period int) float64 {
	n := len(data)
	if n < period+1 {
		return 0
	}

	// Compute initial average gain/loss over the first `period` changes.
	var avgGain, avgLoss float64
	for i := 1; i <= period; i++ {
		change := data[i] - data[i-1]
		if change > 0 {
			avgGain += change
		} else {
			avgLoss += -change
		}
	}
	avgGain /= float64(period)
	avgLoss /= float64(period)

	// Wilder smooth the rest.
	for i := period + 1; i < n; i++ {
		change := data[i] - data[i-1]
		gain, loss := 0.0, 0.0
		if change > 0 {
			gain = change
		} else {
			loss = -change
		}
		avgGain = (avgGain*float64(period-1) + gain) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + loss) / float64(period)
	}

	if avgLoss == 0 {
		return 100
	}
	rs := avgGain / avgLoss
	return 100 - (100 / (1 + rs))
}

// ── MACD ──────────────────────────────────────────────────────────────────────

// macd returns (macd_line, signal_line, histogram) for the given periods.
// Standard is fast=12, slow=26, signal=9.
// Returns all zeros on insufficient data.
func macd(data []float64, fast, slow, signalPeriod int) (line, signal, histogram float64) {
	n := len(data)
	if n < slow {
		return 0, 0, 0
	}
	emaFast := emaFull(data, fast)
	emaSlow := emaFull(data, slow)

	macdLine := make([]float64, n)
	for i := range data {
		macdLine[i] = emaFast[i] - emaSlow[i]
	}

	// Signal = EMA(signalPeriod) of the MACD line.
	// Use only the portion from the slow period onward (where MACD is meaningful).
	if n-slow < signalPeriod {
		return macdLine[n-1], 0, macdLine[n-1]
	}

	meaningfulMacd := macdLine[slow-1:]
	signalVal := ema(meaningfulMacd, signalPeriod)

	line = macdLine[n-1]
	signal = signalVal
	histogram = line - signal
	return
}

// ── Bollinger Bands ───────────────────────────────────────────────────────────

// bollingerBands returns (upper, middle, lower) for the given period and std dev multiplier.
func bollingerBands(data []float64, period int, stdDevMult float64) (upper, middle, lower float64) {
	n := len(data)
	if n < period {
		return 0, 0, 0
	}
	slice := data[n-period:]
	avg := 0.0
	for _, v := range slice {
		avg += v
	}
	avg /= float64(period)

	variance := 0.0
	for _, v := range slice {
		d := v - avg
		variance += d * d
	}
	stdDev := math.Sqrt(variance / float64(period))

	middle = avg
	upper = avg + stdDevMult*stdDev
	lower = avg - stdDevMult*stdDev
	return
}

// ── ATR ───────────────────────────────────────────────────────────────────────

// atr returns the Average True Range using Wilder's smoothing.
// Requires at least period+1 data points.
func atr(highs, lows, closes []float64, period int) float64 {
	n := len(closes)
	if n < period+1 {
		return 0
	}

	trs := make([]float64, n-1)
	for i := 1; i < n; i++ {
		hl := highs[i] - lows[i]
		hpc := math.Abs(highs[i] - closes[i-1])
		lpc := math.Abs(lows[i] - closes[i-1])
		trs[i-1] = math.Max(hl, math.Max(hpc, lpc))
	}

	if len(trs) < period {
		return 0
	}

	// Seed ATR with simple average of first `period` TRs.
	curr := 0.0
	for _, v := range trs[:period] {
		curr += v
	}
	curr /= float64(period)

	for _, tr := range trs[period:] {
		curr = (curr*float64(period-1) + tr) / float64(period)
	}
	return curr
}

// ── ADX ───────────────────────────────────────────────────────────────────────

// adx returns the Average Directional Index (Wilder's, period 14).
// Requires at least 2*period data points for a meaningful result.
func adx(highs, lows, closes []float64, period int) float64 {
	n := len(closes)
	if n < period+1 {
		return 0
	}

	plusDMs := make([]float64, n-1)
	minusDMs := make([]float64, n-1)
	trs := make([]float64, n-1)

	for i := 1; i < n; i++ {
		upMove := highs[i] - highs[i-1]
		downMove := lows[i-1] - lows[i]
		if upMove > downMove && upMove > 0 {
			plusDMs[i-1] = upMove
		}
		if downMove > upMove && downMove > 0 {
			minusDMs[i-1] = downMove
		}
		hl := highs[i] - lows[i]
		hpc := math.Abs(highs[i] - closes[i-1])
		lpc := math.Abs(lows[i] - closes[i-1])
		trs[i-1] = math.Max(hl, math.Max(hpc, lpc))
	}

	m := len(trs)
	if m < period {
		return 0
	}

	// Seed using first `period` values.
	smoothTR, smoothPlus, smoothMinus := 0.0, 0.0, 0.0
	for i := 0; i < period; i++ {
		smoothTR += trs[i]
		smoothPlus += plusDMs[i]
		smoothMinus += minusDMs[i]
	}

	// Wilder smooth forward.
	dxVals := make([]float64, 0, m)
	for i := period; i < m; i++ {
		smoothTR = smoothTR - smoothTR/float64(period) + trs[i]
		smoothPlus = smoothPlus - smoothPlus/float64(period) + plusDMs[i]
		smoothMinus = smoothMinus - smoothMinus/float64(period) + minusDMs[i]

		plusDI := 0.0
		minusDI := 0.0
		if smoothTR != 0 {
			plusDI = 100 * smoothPlus / smoothTR
			minusDI = 100 * smoothMinus / smoothTR
		}
		denom := plusDI + minusDI
		if denom == 0 {
			dxVals = append(dxVals, 0)
		} else {
			dxVals = append(dxVals, 100*math.Abs(plusDI-minusDI)/denom)
		}
	}

	if len(dxVals) < period {
		return 0
	}

	// ADX = Wilder smoothing of DX series.
	curr := 0.0
	for _, v := range dxVals[:period] {
		curr += v
	}
	curr /= float64(period)
	for _, v := range dxVals[period:] {
		curr = (curr*float64(period-1) + v) / float64(period)
	}
	return curr
}

// ── OBV ───────────────────────────────────────────────────────────────────────

// obv returns the On-Balance Volume for the most recent bar.
func obv(closes, volumes []float64) float64 {
	n := len(closes)
	if n == 0 {
		return 0
	}
	curr := volumes[0]
	for i := 1; i < n; i++ {
		if closes[i] > closes[i-1] {
			curr += volumes[i]
		} else if closes[i] < closes[i-1] {
			curr -= volumes[i]
		}
	}
	return curr
}

// ── Stochastic ────────────────────────────────────────────────────────────────

// stochastic returns slow %K and %D for the Stochastic Oscillator (kPeriod, dPeriod, smoothing).
// Standard: kPeriod=14, dPeriod=3, smoothPeriod=3.
// Returns 0, 0 on insufficient data.
func stochastic(highs, lows, closes []float64, kPeriod, dPeriod, smoothPeriod int) (slowK, d float64) {
	n := len(closes)
	if n < kPeriod {
		return 0, 0
	}

	// Raw %K series.
	rawK := make([]float64, n-kPeriod+1)
	for i := kPeriod - 1; i < n; i++ {
		highK := highs[i-kPeriod+1]
		lowK := lows[i-kPeriod+1]
		for j := i - kPeriod + 2; j <= i; j++ {
			if highs[j] > highK {
				highK = highs[j]
			}
			if lows[j] < lowK {
				lowK = lows[j]
			}
		}
		if highK == lowK {
			rawK[i-kPeriod+1] = 50 // avoid division by zero
		} else {
			rawK[i-kPeriod+1] = 100 * (closes[i] - lowK) / (highK - lowK)
		}
	}

	// Slow %K = SMA(smoothPeriod) of raw %K.
	slowKSeries := smaSeries(rawK, smoothPeriod)
	if len(slowKSeries) == 0 {
		return 0, 0
	}

	// %D = SMA(dPeriod) of slow %K.
	dSeries := smaSeries(slowKSeries, dPeriod)
	if len(dSeries) == 0 {
		return slowKSeries[len(slowKSeries)-1], 0
	}

	return slowKSeries[len(slowKSeries)-1], dSeries[len(dSeries)-1]
}

// smaSeries computes a rolling SMA of the given period, returning a series.
func smaSeries(data []float64, period int) []float64 {
	n := len(data)
	if n < period {
		return nil
	}
	out := make([]float64, n-period+1)
	for i := period - 1; i < n; i++ {
		sum := 0.0
		for j := i - period + 1; j <= i; j++ {
			sum += data[j]
		}
		out[i-period+1] = sum / float64(period)
	}
	return out
}

// ── VWAP ─────────────────────────────────────────────────────────────────────

// vwap returns the cumulative Volume-Weighted Average Price over all input candles.
// Typical price = (High + Low + Close) / 3.
func vwap(highs, lows, closes, volumes []float64) float64 {
	n := len(closes)
	if n == 0 {
		return 0
	}
	cumTPV := 0.0
	cumVol := 0.0
	for i := 0; i < n; i++ {
		tp := (highs[i] + lows[i] + closes[i]) / 3.0
		cumTPV += tp * volumes[i]
		cumVol += volumes[i]
	}
	if cumVol == 0 {
		return 0
	}
	return cumTPV / cumVol
}
