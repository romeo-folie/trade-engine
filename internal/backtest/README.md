# internal/backtest — Historical Replay Engine

Implements the backtest pipeline: load candles → run analysis via the LLM pipeline (with prompt caching) → simulate entry/exit internally (SL/TP, signal deferred close) → compute performance metrics → persist the completed run.

> **Note:** The runner simulates trades entirely in memory; it does not use `execution.PaperExecutor`.

## Components

| File | Purpose |
|---|---|
| `runner.go` | `Runner` struct, `Run()` orchestration, candle loading, SL/TP simulation, prompt query |
| `cache.go` | `PromptCache` — keyed by `(prompt_hash, provider, model)`; hit/miss counters; `RequestHash()` |
| `metrics.go` | `Compute()` — win rate, profit factor, Sharpe, Sortino, max drawdown, expectancy |

## Data Flow

```
te backtest run AAPL --from 2024-01-01 --to 2024-06-30
      │
      ▼
Runner.Run(ctx, RunOptions, progressFn)
      │
      ├── LoadCandles()          DB cache → Finnhub backfill if needed
      │
      └── for each candle window (≥30 candles):
            │
            ├── buildRequest()   candles + indicators → AnalysisRequest
            │
            ├── query()          PromptCache.Get() → hit or Analyzer.Analyze()
            │                    PromptCache.Put() on miss
            │
            ├── simulate SL/TP  checkSLTP() / closePosition()
            │
            └── persist         InsertBacktestTrade() per closed position
      │
      ▼
Compute(trades, initialCash, finalCash, from, to, riskFreeRate)
      │
      ▼
RunResult{Label, Trades, Metrics, Hits, Misses}
```

## Prompt Cache

`PromptCache` wraps the `prompt_cache` table. The cache key is a SHA-256 hash of the serialised `AnalysisRequest`. On a cache hit the stored JSON response is returned without calling the LLM. This makes backtest re-runs near-instant for the same parameters and eliminates token costs for repeated analysis.

```go
hash, _ := backtest.RequestHash(req)
entry, hit, _ := cache.Get(ctx, hash, "anthropic", "claude-sonnet-4-5")
```

Pass `RunOptions.NoCache = true` to bypass the cache (useful for testing prompt changes).

## Metrics

`Compute()` returns a `Metrics` struct covering:

- **Returns**: total return %, annualised return %
- **Risk-adjusted**: Sharpe ratio, Sortino ratio (√252 annualisation)
- **Drawdown**: max drawdown %, max drawdown duration
- **Trade stats**: win rate, profit factor, expectancy, best/worst trade
- **Duration**: average hold time

## Candle Coverage

`LoadCandles()` uses a two-step approach: query the DB first, then backfill from the data provider only if the cached slice does not cover the full `[from, to]` window. Coverage is checked with a `2 × candleDuration(tf)` boundary tolerance to account for market holidays.
