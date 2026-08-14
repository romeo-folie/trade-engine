# internal/store — PostgreSQL Access Layer

Owns durable engine state through pgx/v5. Call `store.Connect(ctx)` before using
package-level helpers and `store.Close()` on shutdown. `te migrate` reads the
ordered SQL files from the repository/installed `migrations/` directory and
tracks them in `schema_migrations`. Migrations run transactionally unless their
first non-empty line is the explicit `-- no-transaction` directive; those files
must be idempotent because their DDL cannot be rolled back as a unit.

## Storage Areas

| Area | Representative files | Purpose |
|---|---|---|
| Market evidence | `candles.go`, `news.go`, `earnings.go`, `option_iv.go` | Price/news/event history and option-volatility inputs |
| Discovery lineage | `swing_universe.go`, `weekly_leaders.go`, `industry_rotation.go`, `premarket_episodic.go` | Immutable research/scanner observations |
| Scanner lifecycle | `scanner_snapshots.go`, `scanner_cycle_claims.go`, `scanner_source_promotions.go`, `watchlist.go` | Candidate snapshots, source ownership, promotion, and expiry |
| Decisions | `analyses.go`, `analysis_cache.go`, `signals.go` | Model audit, cache fingerprints, selected lenses, timeframe workflow, and entry-gate decisions |
| Stock execution | `stock_order_intents.go`, `orders.go`, `trades.go`, `closed_ledger.go` | Durable intents/attempts, broker orders, trade projection, and closed-trade ledger |
| Session strategies | `orb.go`, `currency_strength.go` | ORB and currency-strength lifecycle records |
| Risk/operations | `account_risk_baseline.go`, `buy_control.go`, `engine_state.go`, `portfolio.go` | Account baselines, buy pause, engine state, and snapshots |
| Knowledge/feedback | `entities.go`, `relationships.go`, `embeddings.go`, `feedback.go` | Graph, vector retrieval, and outcome feedback |
| Research/reporting | `backtests.go`, `study.go`, `sweep.go` | Replay runs, offline studies, and parameter sweeps |

## Important Semantics

### Watchlist and scanner lineage

- Watchlist removal is a soft delete; re-addition can reactivate the row.
- Scanner discovery context and execution profile are an atomic pair.
- Active, unexpired scanner-owned rows preserve their complete provenance
  envelope against legacy/manual refreshes; expired rows can be replaced.
- Candidate snapshots are immutable and retain the exact promotion-time
  discovery/profile contract.
- Invalid persisted profile JSON is isolated to that row and never blocks
  unrelated watchlist rows from loading.

### Analysis and signals

- Analysis rows retain raw prompts/responses, token usage, indicator evidence,
  configured and selected lenses, rejected alternatives, regime, scanner run,
  and the compact timeframe-workflow audit.
- Signals link to all contributing analysis IDs and persist ordered entry-gate
  decisions plus the effective execution strategy.
- Raw provider output remains available even when the validated live decision
  is downgraded.

### Durable stock orders

Stock entry and protection operations use immutable intent keys and per-attempt
client order IDs. Recovery can distinguish a definitive rejection from an
ambiguous submission, look up accepted broker orders, and project the trade
without submitting duplicate risk. Order/trade projection and broker lineage
are intentionally separate from the intent record.

### Candles and retention

- Candle conflict identity is `(ticker, timeframe, ts)`.
- Bulk inserts are bounded and idempotent.
- Sanity checks reject structurally invalid price series before they contaminate
  downstream analysis.
- Retention removes bounded intraday/prompt-cache history while preserving the
  durable trading and scanner lineage required for audit.

## Runtime Overrides

`config_overrides` is intentionally narrow. The engine currently consumes the
DB-backed `engine.mode` startup override; other settings remain YAML/env/CLI
configuration rather than arbitrary runtime database overrides.

## Error Handling

Single-row lookups generally return `nil, nil` when no record exists. Callers
must nil-check. Write paths return contextual errors and transaction boundaries
are kept around state transitions that must remain atomic.
