# TradeEngine

LLM-powered algorithmic trading system for US equities, options, forex/CFDs,
and futures. TradeEngine discovers candidates, computes technical evidence,
runs bounded multi-model analysis, applies deterministic risk and entry controls,
and manages broker-authoritative execution in paper or live deployments.

**Status:** The core engine is operational in paper trading. Live-stock controls,
broker preflight, durable order intents, reconciliation, and emergency buy
controls are implemented; enabling a live deployment remains an explicit
operator decision.

## Architecture

```mermaid
graph TD
    CLI[te CLI<br/>cmd/te/] --> Engine[Engine Loop<br/>internal/engine/]
    Universe[Universe + Scanner<br/>internal/universe · internal/scanner] --> Profiles[Discovery Context + Execution Profile]
    Profiles --> Engine
    Engine --> DataPipeline[Data Pipeline<br/>internal/data/]
    DataPipeline -->|stocks + options| Alpaca[Alpaca APIs]
    Engine -->|forex + futures data| IBKR[IBKR Client Portal]
    DataPipeline -->|news| Finnhub[Finnhub API]
    Engine --> Indicators[Indicator Engine<br/>internal/indicators/]
    Engine --> Store[PostgreSQL + pgvector<br/>internal/store/]
    Engine --> Analysis[Profile-bound Analysis<br/>internal/analysis/]
    Analysis --> LLM[Claude · GPT-4o · Gemini · Ollama]
    Analysis --> Gates[Timeframe Confirmation + Entry Gates]
    Gates --> Risk[Portfolio Risk Manager<br/>internal/risk/]
    Risk --> Executor[Order Executor<br/>internal/execution/]
    Executor -->|equities + options| AlpacaBroker[Alpaca Broker API]
    Executor -->|forex · CFDs · futures| IBKRBroker[IBKR Client Portal API]
    Engine --> Reconcile[Intent Recovery + Broker Reconciliation]
    Engine --> Backtest[Backtesting Engine<br/>internal/backtest/]
    Engine --> Knowledge[Knowledge Graph<br/>internal/knowledge/]
    Engine --> Embeddings[Vector Embeddings<br/>internal/embeddings/]
    Engine --> TUI[Terminal Dashboard<br/>internal/tui/]
    Engine --> Notify[Telegram Notifications<br/>internal/notify/]
    Engine --> Reports[HTML Reports<br/>internal/reports/]
```

## Project Layout

```
cmd/te/              — CLI entry point (Cobra commands)
internal/
  config/            — YAML + env var + DB-override config system
  data/              — Candle, quote, trade, and news provider interfaces
  universe/          — Immutable swing-universe construction
  scanner/           — Multi-source candidate discovery, scoring, and promotion
  tradeprofile/      — Discovery provenance + bounded execution-profile contract
  engine/            — Core engine loop, timeframe workflow, and entry gates
  indicators/        — Technical indicator calculations and evidence snapshots
  store/             — PostgreSQL + pgvector access layer (pgx/v5)
  logger/            — Structured slog dual-output (stdout + file)
  analysis/          — Single, ensemble, specialist, and confirmation analysis
  risk/              — Portfolio, exposure, drawdown, sizing, and admission controls
  execution/         — Broker execution, trailing exits, and bracket management
  stockorders/       — Durable stock-order intents, attempts, and crash recovery
  recon/             — Broker/database drift reconciliation
  events/            — Typed internal event bus and observability payloads
  cost/              — Model-cost accounting and budget enforcement
  orb/               — Shared options, forex/CFD, and futures strategy lifecycle
  strategy/          — Pullback-strength and currency-strength evaluators
  earnings/          — Earnings calendar and options blackout support
  options/           — Options market-data and IV workflows
  feedback/          — Performance feedback pipeline (nightly review + prompt enrichment)
  portfolio/         — Portfolio tracker + position monitoring
  tui/               — Bubble Tea terminal dashboard
  notify/            — Telegram notifications + signal approval buttons
  backtest/          — Historical replay engine + prompt cache + metrics
  embeddings/        — pgvector embedding pipeline (OpenAI / Ollama)
  knowledge/         — Entity relationship graph + news enrichment
  reports/           — Self-contained HTML report generation (go-echarts)
migrations/          — Ordered, versioned SQL migrations
docs/                — Architecture notes, research results, and operator runbooks
```

## Implemented Strategies

TradeEngine keeps discovery sources separate from execution strategies. A
weekly leader, premarket catalyst, potent industry, or graph/news-sympathy match
explains **why** a stock entered the pipeline; it does not become the strategy
used to trade it.

### Stock analysis lenses

Scanner-promoted stocks receive a bounded execution profile. Analysts may select
only an admissible lens from that profile (or select none):

| Lens | Intended use | Deterministic controls |
|------|--------------|------------------------|
| `trend_following` | Established directional trends | Daily trend alignment and portfolio/risk admission |
| `mean_reversion` | Reversion toward a statistically stretched range | Profile validation and standard risk admission |
| `momentum` | Continuation with confirmed strength | Anti-chase/run-up limits and standard risk admission |
| `news_sentiment` | Catalyst-driven directional setups | Minimum tape reaction and standard risk admission |
| `pullback_strength` | Strong trend pulling back into defined support | Daily AVWAP/EMA/structure qualification, refreshed before entry |

Swing profiles normally form the thesis on `1D` and may require a named `4H`
entry-confirmation pass. Event-driven profiles can use `4H` with optional `1H`
timing. The selected lens, rejected alternatives, timeframe workflow, and entry
gate outcomes are retained as audit lineage.

### Session and market strategies

| Strategy | Market / venue | State |
|----------|----------------|-------|
| Opening Range Breakout (`orb`) | Alpaca options; IBKR forex/CFDs and futures | Implemented |
| Trend continuation | IBKR forex/CFDs and futures | Implemented per instrument |
| Sweep/reclaim | IBKR forex/CFDs and futures | Implemented per instrument |
| Opening shock | IBKR forex/CFDs and futures | Implemented per instrument |
| Currency-strength rotation | IBKR forex | Implemented in paper mode; shadow skips execution and live mode is rejected |
| Equity ORB | Alpaca equities | Retired; direct-equity ORB entries are disabled |

Strategy availability is configuration-driven. An implemented strategy is not
necessarily enabled on a particular host; inspect the resolved configuration
with `te config show` before operating it.

## Core Capabilities

- Immutable swing universes and multi-source scanner snapshots with evidence,
  scores, promotion lineage, and expiring watchlist ownership.
- Profile-bound single, ensemble, adaptive-depth, or sequential-specialist LLM
  analysis with deterministic lens validation and optional entry confirmation.
- Technical evidence covering moving averages, momentum, volatility, volume,
  VWAP, and deterministic pullback structure.
- Broker-authoritative stock sizing, durable intents and attempts, idempotent
  recovery, whole-share protection, trailing/bracket exits, and exit ownership.
- Account-aware risk baselines, drawdown and daily-loss controls, exposure and
  concentration limits, buy pause/resume controls, and broker preflight.
- ORB/session execution for options, forex/CFDs, and futures, with research
  variants and replay tooling for continuation, sweep/reclaim, and opening shock.
- PostgreSQL/pgvector persistence, knowledge-graph enrichment, similar-setup
  retrieval, performance feedback, reports, Telegram, and terminal operations.

## Quick Start

```bash
# Prerequisites: Go 1.25+, Docker (for local DB), Alpaca + Finnhub API keys

# 1) Install the development launcher (once, from the repository root)
make install-dev

# Ensure ~/.local/bin is on PATH for this shell
export PATH="$HOME/.local/bin:$PATH"

# 2) Start PostgreSQL + pgvector
docker compose up -d db

# 3) Configure
cp config.example.yaml ~/.tradeengine/config.yaml
export DATABASE_URL="postgres://postgres:postgres@localhost:5432/tradeengine?sslmode=disable"
export ALPACA_API_KEY="your-key"
export ALPACA_API_SECRET="your-secret"
export FINNHUB_API_KEY="your-key"

# 4) Run migrations
te migrate

# 5) Add a ticker to the watchlist (triggers 2y daily + 30d hourly backfill)
te watchlist add AAPL --strategies=trend_following --timeframes=1h,1d

# 6) Run the engine (paper trading mode by default)
te run
```

`make install-dev` links `~/.local/bin/te` to the repository's development
launcher. Each command except `te stop` performs a cached Go build from the current
checkout before it runs, so pulling or merging backend and terminal-dashboard
changes does not require another install. Run `make build` separately when changing
the React frontend locally so its embedded assets are regenerated. Persist the
`PATH` export above in your shell profile (for example, `~/.zshrc`) if
`~/.local/bin` is not already configured there. The launcher keeps a last-known-good
executable in the user cache so `te stop` remains available without rebuilding when
the checkout temporarily cannot compile.

### Embeddings provider (API vs local)

Embeddings are always stored in your Postgres/pgvector DB. The provider only changes
where vectors are computed:

- `openai` → cloud embedding inference (counts toward API usage)
- `ollama` → local embedding inference (no embedding API usage)

For local embeddings with Ollama:

```bash
ollama serve
ollama pull nomic-embed-text
curl -s http://localhost:11434/api/embeddings \
  -d '{"model":"nomic-embed-text","prompt":"hello world"}' | jq .
```

> `nomic-embed-text` is 768-dim. Ensure your vector schema dimension matches before enabling.

### Extraction throttling (news → knowledge graph)

Extraction now has dedicated guardrails under `markets.news`:

- `extraction_enabled`
- `extraction_provider`
- `extraction_model`
- `extraction_max_articles_per_tick`
- `extraction_rpm`

Recommended defaults for cost control:

```yaml
markets:
  news:
    extraction_enabled: true
    extraction_provider: openai
    extraction_model: gpt-4o
    extraction_max_articles_per_tick: 10
    extraction_rpm: 20
```

This keeps extraction on a single provider and prevents burst traffic from tripping RPM limits.

### Docker notes

If you previously started a container named `tradeengine-db` manually and see:

`Conflict. The container name "/tradeengine-db" is already in use ...`

list and remove it, then retry:

```bash
docker ps -a --filter name=tradeengine-db
docker rm -f tradeengine-db
docker compose up -d db
```

Check DB health:

```bash
docker compose ps
```

If port `5432` is already in use, override the host port in `.env`:

```bash
cp .env.example .env
# then edit .env
POSTGRES_PORT=5434
```

And use matching `DATABASE_URL`:

```bash
export DATABASE_URL="postgres://postgres:postgres@localhost:5434/tradeengine?sslmode=disable"
```

### Manual approvals (TUI / CLI / Telegram)

Manual approval actions are available **only when `engine.mode=manual`**.

- In `paper` or `auto` mode, signals should execute automatically (subject to risk checks).
- If a signal is created but execution logs show `signal execution skipped — zero quantity`, that is a sizing/cash outcome, not an approval failure.

Approve/reject options:

```bash
# list pending manual-approval signals
te trades pending

# approve / reject by signal id
te trades approve <signal-id>
te trades reject <signal-id>
```

When Telegram is enabled (`notifications.telegram.enabled: true`), pending manual signals include inline buttons:
- ✅ Approve
- ❌ Reject
- 📋 Full Analysis

## Cron Schedule (Production)

The engine runs in distinct modes that cover different markets and sessions. Each cron entry uses `te-switch.sh` which stops any running process before starting the new mode. All times are **UTC**.

```
TE=/path/to/scripts/te-switch.sh

# ── Forex strategies: London session ─────────────────────────────
# Starts 30 min before London open (08:00 UTC) for pre-session setup.
# te-switch.sh stops any running process, then starts the new mode.
30 7 * * 1-5  $TE forex --session london

# ── Forex strategies: New York session ───────────────────────────
# Starts 30 min before NY open (13:00 UTC).
30 12 * * 1-5  $TE forex --session newyork

# ── Options ORB (optional) ───────────────────────────────────────
# Fixed configured underlyings; no direct-equity ORB entries.
0 13 * * 1-5  $TE orb-options

# ── Standard mode: US equities session ───────────────────────────
# Runs the full analysis pipeline (ensemble LLM, risk checks, execution).
# US market hours: 13:30–20:00 UTC (9:30 AM – 4:00 PM ET).
0 15 * * 1-5  $TE regular

# ── Kill any leftover process before next day ────────────────────
0 21 * * 1-5  te stop
```

**Notes:**

- `te-switch.sh` runs `te stop` + waits 5 seconds before starting each mode, preventing process collisions.
- For `forex`, if `--session` is omitted, the script auto-detects from UTC hour (before 12:00 = london, after = newyork). `orb-forex` remains accepted as a compatibility alias.
- Forex mode requires IBKR Client Portal Gateway running on the host. See `markets.ibkr` in config.
- Options ORB (`orb-options`) uses Alpaca option data and can run alongside forex because they use different brokers.
- The feedback reviewer runs inside whichever process is active at `feedback.review_time_utc` (default 18:00 UTC).

## CLI Reference

The command tree evolves with the engine. Use `te --help` and
`te <command> --help` as the canonical flag reference.

### Engine and operator controls

```text
te run [--mode <mode>] [--session <session>] [--detach] [--dry-run]
                                      Start the engine
te run --web [--web-port 8080]       Start the engine with web observability
te stop                              Stop the running engine process (SIGTERM)
te status                            Show process, database, and configured/DB mode
te mode <manual|auto|paper>          Persist the startup mode override; restart required
te buys pause|resume|status          Control new stock buys without disabling exits
te dashboard                         Open the terminal dashboard (aliases: ui, tui)
te web                               Start the standalone web observability interface
te logs tail                         Tail the engine log
te logs search <query>               Search the engine log
te notify test                       Send an ungated Telegram API connectivity test
```

Supported `te run --mode` values are `manual`, `auto`, `paper`, `orb-options`,
`forex` (`orb-forex` compatibility alias), `orb-futures`,
`orb-futures-metals`, and `orb-newyork`. Forex mode accepts
`--session london|newyork`.

`te run --web` starts in-process web observability only on the standard
`manual`, `auto`, or `paper` path. Use standalone `te web` while a specialized
ORB, forex, futures, metals, or combined session is running.

### Configuration and database

```text
te config show                       Print resolved config with secrets masked
te config set <key> <value>          Persist a value to config.yaml
te config set engine.mode <mode> --runtime
                                      Persist the supported DB startup override
te config validate                   Validate the resolved configuration
te migrate                           Apply pending database migrations
te db --help                         Database reset and summary-backfill operations
te stocks preflight                  Verify stock broker identity and account readiness
te risk reset-baseline               Reset an account risk baseline after verified cash changes
```

`engine.mode` is currently the only DB runtime-override key consumed by the
engine and dashboard. It persists without editing YAML and requires an engine
restart to apply. Use ordinary `te config set` for every other key.

### Trading data and operations

```text
te watchlist add|remove|show          Manage scanner and engine watchlist items
te trades list|show|pending           Inspect trades and pending manual signals
te trades approve|reject <signal-id>  Resolve a manual-mode signal
te reconcile broker                    Read-only Alpaca-equities/DB drift report
te reconcile broker --ticker IBKR      Narrow the report to one ticker
te reconcile broker --json              Emit machine-readable drift JSON
te universe refresh|list              Build or inspect immutable swing universes
te earnings sync|seed|list            Manage the options earnings calendar
te options backfill-iv                Backfill options implied-volatility history
te orb stats                          Inspect opening-range performance
te orb repair-trades --plan <file>             Preview a reviewed YAML repair plan (default)
te orb repair-trades --plan <file> --apply     Apply the plan transactionally
```

`te reconcile broker` is intentionally limited to Alpaca equities and the
standard engine's legacy `trades` ledger. Forex, futures, options, and ORB
positions use separate ledgers and are not evaluated by this command.

### Models and knowledge

```text
te models list                         List configured analyst models
te models test <prompt>                Test available analyst providers (not arbiter)
te models compare <ticker>             Compare model analysis for one ticker
te models performance                  Summarize realized performance by model
te graph seed                          Seed the knowledge graph
te graph entities                      List graph entities
te graph relationships <ticker>        Inspect a ticker's graph relationships
te graph review                        Review extracted relationships
te embeddings status                   Show embedding provider and index status
te embeddings reindex                  Fill missing stored embeddings
te embeddings reindex --force          Clear and rebuild all stored embeddings
```

### Reports

```text
te report daily                        Generate the daily trading summary
te report weekly                       Generate the weekly trading summary
te report performance                  Report portfolio and model performance
te report scanner-funnel               Audit scanner conversion and outcomes
te report weekly-leaders               Review full-universe leadership studies
te report potent-industries            Review industry potency and breadth
te report premarket-episodic           Review premarket gap/catalyst observations
te report backtest <id>                Generate one backtest report
te report compare <id1> <id2>          Compare two backtest reports
```

Use `te report <command> --help` for date ranges, filters, and report-specific
options. Generated HTML output locations are controlled by the `reports`
configuration.

### Backtests

```text
te backtest run                         Run a general historical backtest
te backtest list                        List saved backtest runs
te backtest compare                     Compare saved runs
te backtest orb                         Replay the forex/CFD ORB strategy
te backtest orb-compare                 Compare ORB rule variants
te backtest orb-options                 Replay options ORB
te backtest orb-sweep                   Search ORB parameter robustness
te backtest orb-fade                    Replay failed-break fades
te backtest opening-shock               Replay opening-shock reversals
te backtest trend-continuation          Replay delayed trend continuation
te backtest sweep-reclaim               Replay liquidity sweep/reclaim entries
te backtest currency-strength           Replay FX currency-strength rotation
te backtest currency-strength-research  Run the walk-forward currency-strength research harness
```

### Offline studies

Studies audit recorded data without starting the trading engine.

```text
te study execution                      Replay stock trades against 1m bars
te study pullback-frequency             Measure deterministic pullback frequency
```

## Capability Status

| Area | Status |
|------|--------|
| Equity swing discovery, analysis, risk, and Alpaca execution | ✅ Operational in paper mode |
| Broker-authoritative live-stock safeguards and operator controls | ✅ Implemented; deployment-controlled |
| Options ORB via Alpaca | ✅ Implemented; real-time OPRA feed required |
| Forex/CFD ORB and specialized session strategies via IBKR | ✅ Implemented |
| Futures ORB and specialized per-contract strategies via IBKR | ✅ Implemented; configuration-gated |
| Currency-strength rotation | 🧪 Paper mode only; shadow does not execute |
| Equity ORB | ⛔ Retired |
| Backtesting, studies, reports, feedback, knowledge graph, and embeddings | ✅ Implemented |

## Further Documentation

- [`internal/config/`](internal/config/) — config system
- [`internal/data/`](internal/data/) — market data + news providers
- [`internal/indicators/`](internal/indicators/) — technical analysis engine
- [`internal/engine/`](internal/engine/) — core engine loop + scheduler
- [`internal/store/`](internal/store/) — database access layer
- [`internal/analysis/`](internal/analysis/) — multi-model ensemble analysis
- [`internal/backtest/`](internal/backtest/) — historical replay engine
- [`internal/embeddings/`](internal/embeddings/) — vector embedding pipeline
- [`internal/knowledge/`](internal/knowledge/) — entity relationship graph
- [`internal/logger/`](internal/logger/) — structured logger
- [`internal/reports/`](internal/reports/) — HTML report generation
- [`docs/issue-550-discovery-execution-profile.md`](docs/issue-550-discovery-execution-profile.md) — scanner provenance, execution profiles, timeframe confirmation, and gate audit
- [`docs/live-operator-runbook.md`](docs/live-operator-runbook.md) — live-stock preflight, risk controls, and emergency operations
- [`docs/swing-universe-foundation.md`](docs/swing-universe-foundation.md) — immutable swing-universe design
- [`docs/weekly-leader-scanner.md`](docs/weekly-leader-scanner.md) — weekly leadership discovery
- [`docs/potent-industry-scanner.md`](docs/potent-industry-scanner.md) — industry potency and breadth scanner
- [`docs/premarket-episodic-scanner.md`](docs/premarket-episodic-scanner.md) — premarket event discovery
- [`docs/options-orb.md`](docs/options-orb.md) — options ORB operations and safeguards
- [`docs/exit-policy-study.md`](docs/exit-policy-study.md) — stock exit-policy research and rollout history
- [`docs/currency-strength-research-2026-08.md`](docs/currency-strength-research-2026-08.md) — currency-strength walk-forward research
