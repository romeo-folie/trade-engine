# TradeEngine

LLM-powered algorithmic trading system. Monitors stock markets, runs AI analysis via Claude (and optionally other models), manages a paper or live portfolio, and delivers signals via a terminal dashboard and Telegram.

> **Note:** Prediction market functionality (Polymarket, Kalshi) has been moved to [`polymarket-geopolitical-agent`](https://github.com/romeo-folie/polymarket-geopolitical-agent).

**Status:** Phases 1–13 complete — full trading system live, paper trading in progress ahead of live deployment.

## Architecture

```mermaid
graph TD
    CLI[te CLI<br/>cmd/te/] --> Engine[Engine Loop<br/>internal/engine/]
    Engine --> DataPipeline[Data Pipeline<br/>internal/data/]
    DataPipeline -->|candles| Alpaca[Alpaca API]
    DataPipeline -->|news| Finnhub[Finnhub API]
    Engine --> Indicators[Indicator Engine<br/>internal/indicators/]
    Engine --> Store[PostgreSQL + pgvector<br/>internal/store/]
    Engine --> Analysis[Ensemble Analysis<br/>internal/analysis/]
    Analysis --> LLM[Claude · GPT-4o · Gemini · Ollama]
    Analysis --> Risk[Risk Manager<br/>internal/risk/]
    Risk --> Executor[Order Executor<br/>internal/execution/]
    Executor -->|paper / live| AlpacaBroker[Alpaca Broker API]
    Executor -->|forex / CFD| IBKRBroker[IBKR Client Portal API]
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
  data/              — Market data providers (Alpaca) + news (Finnhub)
  engine/            — Core engine loop, scheduler
  indicators/        — 20 technical indicators (EMA, RSI, MACD, Bollinger, ADX, ATR, OBV, Stochastic, VWAP, …)
  store/             — PostgreSQL + pgvector access layer (pgx/v5)
  logger/            — Structured slog dual-output (stdout + file)
  analysis/          — Multi-model ensemble analysis (Claude · GPT-4o · Gemini · Ollama)
  risk/              — 7-check risk manager (confidence, R/R, position size, drawdown, …)
  execution/         — PaperExecutor + AlpacaBroker + IBKRBroker executors
  orb/               — Opening Range Breakout strategy (equities + forex/CFD)
  feedback/          — Performance feedback pipeline (nightly review + prompt enrichment)
  portfolio/         — Portfolio tracker + position monitoring
  tui/               — Bubble Tea terminal dashboard
  notify/            — Telegram notifications + signal approval buttons
  backtest/          — Historical replay engine + prompt cache + metrics
  embeddings/        — pgvector embedding pipeline (OpenAI / Ollama)
  knowledge/         — Entity relationship graph + news enrichment
  reports/           — Self-contained HTML report generation (go-echarts)
migrations/          — SQL migration files (001–020)
docs/                — PRD + implementation plan + local-first embeddings guide
```

## Quick Start

```bash
# Prerequisites: Go 1.22+, Docker (for local DB), Alpaca + Finnhub API keys

# 1) Start PostgreSQL + pgvector
docker compose up -d db

# 2) Configure
cp config.example.yaml ~/.tradeengine/config.yaml
export DATABASE_URL="postgres://postgres:postgres@localhost:5432/tradeengine?sslmode=disable"
export ALPACA_API_KEY="your-key"
export ALPACA_API_SECRET="your-secret"
export FINNHUB_API_KEY="your-key"

# 3) Run migrations
te migrate

# 4) Add a ticker to the watchlist (triggers 2y daily + 30d hourly backfill)
te watchlist add AAPL --strategies=trend_following --timeframes=1h,1d

# 5) Run the engine (paper trading mode by default)
te run
```

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

# ── Equities ORB (optional) ──────────────────────────────────────
# Pre-market gap scanning starts at 10:00 UTC (6 AM ET).
# Market open is 13:30 UTC (9:30 AM ET). Auto-exits by 14:30 UTC.
0 10 * * 1-5  $TE orb

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
- Equities ORB (`orb`) uses Alpaca. It can run concurrently with forex mode since they use different brokers.
- Standard mode and equities ORB should not overlap — standard mode starts after ORB's 14:30 UTC cutoff.
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
te status                            Show engine, database, and active-mode status
te mode <manual|auto|paper>          Persist the startup mode override; restart required
te dashboard                         Open the terminal dashboard (aliases: ui, tui)
te web                               Start the standalone web observability interface
te logs tail                         Tail the engine log
te logs search <query>               Search the engine log
te notify test                       Send a Telegram connectivity test
```

Supported `te run --mode` values are `manual`, `auto`, `paper`, `orb`,
`forex` (`orb-forex` compatibility alias), `orb-futures`,
`orb-futures-metals`, and `orb-newyork`. Forex mode accepts
`--session london|newyork`.

### Configuration and database

```text
te config show                       Print resolved config with secrets masked
te config set <key> <value>          Persist a value to config.yaml
te config set <key> <value> --runtime
                                      Persist a DB override
te config validate                   Validate the resolved configuration
te migrate                           Apply pending database migrations
te db --help                         Database reset and summary-backfill operations
```

Runtime overrides persist to the database without editing YAML. Components
that read configuration during startup require an engine restart to apply them.

### Trading data and operations

```text
te watchlist add|remove|show          Manage scanner and engine watchlist items
te trades list|show|pending           Inspect trades and pending manual signals
te trades approve|reject <signal-id>  Resolve a manual-mode signal
te universe refresh|list              Build or inspect immutable swing universes
te earnings sync|seed|list            Manage the options earnings calendar
te options backfill-iv                Backfill options implied-volatility history
te orb stats                          Inspect opening-range performance
```

### Models and knowledge

```text
te models list                         List configured analyst models
te models test <prompt>                Send a prompt to each enabled model
te models compare <ticker>             Compare model analysis for one ticker
te models performance                  Summarize realized performance by model
te graph seed                          Seed the knowledge graph
te graph entities                      List graph entities
te graph relationships <ticker>        Inspect a ticker's graph relationships
te graph review                        Review extracted relationships
te embeddings status                   Show embedding provider and index status
te embeddings reindex                  Rebuild stored embeddings
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

Use `te report <command> --help` for date ranges, filters, output paths, and
report-specific options.

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
```

### Offline studies

Studies audit recorded data without starting the trading engine.

```text
te study execution                      Replay stock trades against 1m bars
te study pullback-frequency             Measure deterministic pullback frequency
```

## Build Phase Status

| Phase | Description | Status |
|-------|-------------|--------|
| 1 | Foundation (scaffold, config, migrations, CLI) | ✅ Complete |
| 2 | Data pipeline (market data, indicators, news) | ✅ Complete |
| 3 | Analysis engine (LLM integration, prompt engineering) | ✅ Complete |
| 4 | Trading core (signals, risk, execution, portfolio) | ✅ Complete |
| 5 | Terminal dashboard (Bubble Tea TUI) | ✅ Complete |
| 6 | Telegram notifications + signal approval flow | ✅ Complete |
| 7 | Backtesting engine + prompt cache + metrics | ✅ Complete |
| 8 | HTML reports (go-echarts, offline self-contained) | ✅ Complete |
| 9 | ~~Prediction markets~~ (moved to `polymarket-geopolitical-agent`) | ➡️ Moved |
| 10 | Knowledge graph (entity relationships + news enrichment) | ✅ Complete |
| 11 | Vector embeddings (pgvector, similar-setup injection) | ✅ Complete |
| 12 | Multi-model ensemble deliberation (Claude · GPT-4o · Gemini · Ollama) | ✅ Complete |
| 13 | Go-live operational (paper trading run → live switch) | 🔄 In progress |
| 14 | Web visualisation dashboard | ⏳ Planned |
| 15 | Opening Range Breakout — equities (Alpaca) | ✅ Complete |
| 16 | IBKR integration — forex + CFD ORB (EUR/USD, XAU/USD, etc.) | ✅ Complete |
| 17 | Feedback pipeline — nightly review, flags, prompt enrichment | ✅ Complete |

## Module READMEs

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
