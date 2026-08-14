# internal/portfolio

Portfolio state tracking, snapshots, daily summaries, independent poller, and Alpaca backfill.

## Purpose

Maintains an in-memory view of the current portfolio (total value, cash, positions, P&L) and
persists periodic snapshots and daily summaries to the database for historical analysis and
reporting. Snapshots are also written by an independent poller goroutine that
runs regardless of engine tick cadence — so the equity chart has no gaps during sleep windows.

## Components

| Component | Description |
|-----------|-------------|
| `Tracker` | In-memory portfolio state machine. Tracks total value, cash, invested amount, unrealized P&L, peak value (for drawdown), and last loss timestamp. |
| `State` | Snapshot of current portfolio values (exported for reads by risk manager and CLI). |
| `MaybeSnapshot` | Writes a `portfolio_snapshots` row at the configured interval (default: 5 min). Tags rows with `source='engine'`, plus `mode` and `broker` set via `SetTags`. |
| `MaybeDailySummary` | Writes a `daily_summaries` row once per calendar day. Uses `ON CONFLICT` upsert. |
| `Poller` | Background goroutine. Calls broker `AccountSnapshot` on a configurable interval (default 60 s) and writes extra snapshot rows tagged `source='poller'`. Also writes when equity moves >0.05% (configurable). Runs independently of the engine tick loop. |
| `Backfill` | One-time job. Fetches up to 1 Y of Alpaca equity history and seeds `portfolio_snapshots` for any calendar-day gaps, tagged `source='backfill'`. Idempotent — existing rows are never modified. |

## Portfolio Snapshot Sources

Each row in `portfolio_snapshots` carries a `source` column indicating its origin:

| `source` | Written by | Frequency |
|----------|-----------|-----------|
| `engine` | `Tracker.MaybeSnapshot` | Every 5 min (engine tick) |
| `poller` | `Poller.poll` | Every 60 s OR on >0.05% equity move |
| `backfill` | `Backfill` | Once at startup (fills historical gaps) |

The `mode` column (`live`/`paper`) and `broker` column (`alpaca`/`ibkr`) are set by `SetTags`
and passed through by the poller and backfill.

## Data Flow

```
Engine tick loop:
  Engine → Tracker.Update(positions, cash, dailyPnL)
  Engine → Tracker.MaybeSnapshot(ctx)          → portfolio_snapshots [source=engine]
  Engine → Tracker.MaybeDailySummary(ctx, ...) → daily_summaries

Independent of engine:
  Poller.Run(ctx) → AccountSnapshot → portfolio_snapshots [source=poller]
  Backfill(ctx)   → EquityCurve     → portfolio_snapshots [source=backfill, gaps only]

Web layer:
  GET /api/node/portfolio       → BrokerView.AccountSnapshot  (broker-first, 3 s cache)
  GET /api/portfolio/positions  → BrokerView.PositionsSnapshot (broker-first, 3 s cache)
  GET /api/portfolio/history    → BrokerView.EquityCurve + GetDailySummaries (30 s cache)
  GET /api/portfolio/performance→ BrokerView.AccountSnapshot + GetDailySummaries
```

All broker-facing web handlers include `source: "broker"|"tracker"|"db"` and
`stale_at` (ISO 8601, non-null only on DB fallback) so the UI can render a live/stale badge.

## Configuration

```yaml
portfolio:
  poller:
    enabled: true
    interval_sec: 60              # poll the broker every 60 s
    significant_change_pct: 0.05  # also write on >0.05% equity move
  cache:
    account_ttl_sec: 3
    positions_ttl_sec: 3
    history_ttl_sec: 30
    orders_ttl_sec: 10
```

## Database Tables

- `portfolio_snapshots` — time-series of portfolio equity with `source`, `mode`, `broker` tags (migration 021).
- `daily_summaries` — one row per day with trade counts, win/loss, token usage, net P&L.
