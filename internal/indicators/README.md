# internal/indicators — Technical Analysis Engine

Pure computation — no I/O, no side effects. Takes a slice of `data.Candle` and returns an `IndicatorSet` with all 11 indicators computed. Used by the engine loop before building the LLM prompt.

## Components

| File | Purpose |
|------|---------|
| `indicators.go` | `IndicatorSet` struct + `Compute(candles)` entry point |
| `technical.go` | All indicator computation functions (unexported, tested directly) |
| `technical_test.go` | Unit tests for every indicator with known inputs/outputs |

## IndicatorSet

```go
type IndicatorSet struct {
    EMA20, EMA50, EMA200          float64  // Exponential Moving Average
    SMA20, SMA50, SMA200          float64  // Simple Moving Average
    RSI                           float64  // Relative Strength Index (14)
    MACDLine, MACDSignal, MACDHistogram float64  // MACD (12,26,9)
    BBUpper, BBMiddle, BBLower    float64  // Bollinger Bands (20, 2σ)
    ADX                           float64  // Average Directional Index (14)
    ATR                           float64  // Average True Range (14)
    OBV                           float64  // On-Balance Volume
    StochK, StochD                float64  // Stochastic Oscillator (14,3,3)
    VWAP                          float64  // Volume-Weighted Average Price
    VolumeSMA20                   float64  // Volume Simple Moving Average (20)
}
```

Zero values indicate insufficient candle history for that indicator.

## Indicator Implementations

All use standard formulations:

| Indicator | Method | Minimum Candles |
|-----------|--------|----------------|
| SMA | Arithmetic mean of last N closes | N |
| EMA | Wilder/exponential decay, seeded with SMA | N |
| RSI | Wilder's smoothing of avg gain/loss | 15 |
| MACD | EMA(fast) − EMA(slow); signal = EMA(signal) of MACD | 35 |
| Bollinger | SMA ± 2×population stddev | 20 |
| ATR | True Range, Wilder-smoothed | 15 |
| ADX | +DI/−DI from smoothed directional movement | 29 |
| OBV | Cumulative ±volume based on close direction | 1 |
| Stochastic | %K over 14-period range, smoothed twice | 20 |
| VWAP | Cumulative (TP × Volume) / cumulative Volume | 1 |
| Volume SMA | SMA(20) of volume | 20 |

> **Recommended minimum:** 200 candles for EMA200 and ADX to be meaningful. The `BackfillHistory` function in the engine ensures this is satisfied on watchlist add.

## Usage

```go
candles, _ := store.GetCandles(ctx, "AAPL", data.Timeframe1Day, from, to)
set, err := indicators.Compute(candles)
// set.RSI, set.EMA20, set.MACDLine, ... all populated
```

## Testing

```bash
go test ./internal/indicators/...
```

Tests cover: SMA/EMA correctness, RSI bounds (0–100), MACD sign in uptrend, Bollinger ordering (upper > mid > lower), ATR positivity, OBV accumulation, Stochastic bounds, VWAP on single bar, ADX range, and a full `Compute` integration test on 220 synthetic candles.
