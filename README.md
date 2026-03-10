# TradeEngine

LLM-powered algorithmic trading system. Monitors stock and prediction markets, runs AI analysis via Claude (and optionally other models), manages a paper or live portfolio, and delivers signals via a terminal dashboard and Telegram.

**Status:** Phase 2 complete — data pipeline live (market data + indicators + news).

## Architecture

```mermaid
graph TD
    CLI[te CLI<br/>cmd/te/] --> Engine[Engine Loop<br/>internal/engine/]
    Engine --> DataPipeline[Data Pipeline<br/>internal/data/]
    DataPipeline -->|candles| Alpaca[Alpaca API]
    DataPipeline -->|news| Finnhub[Finnhub API]
    Engine --> Indicators[Indicator Engine<br/>internal/indicators/]
    Engine --> Store[PostgreSQL Store<br/>internal/store/]
    Engine --> Analysis[Analysis Engine<br/>internal/analysis/ — Phase 3]
    Analysis --> LLM[Claude / GPT / Ollama]
    Analysis --> Risk[Risk Manager<br/>internal/risk/ — Phase 4]
    Risk --> Executor[Order Executor<br/>internal/execution/ — Phase 4]
    Executor -->|paper| AlpacaPaper[Alpaca Paper API]
    Engine --> TUI[Terminal Dashboard<br/>internal/tui/ — Phase 5]
    Engine --> Notify[Telegram Bot<br/>internal/notify/ — Phase 6]
```

## Project Layout

```
cmd/te/              — CLI entry point (Cobra commands)
internal/
  config/            — YAML + env var + DB-override config system
  data/              — Market data providers (Alpaca) + news (Finnhub)
  engine/            — Core engine loop + market hours scheduler
  indicators/        — 11 technical indicators (EMA, RSI, MACD, Bollinger, ADX, ATR, OBV, Stochastic, VWAP, ...)
  store/             — PostgreSQL access layer (pgx/v5)
  logger/            — Structured slog dual-output (stdout + file)
  analysis/          — LLM prompt engine (Phase 3)
  risk/              — Risk manager (Phase 4)
  execution/         — Order execution (Phase 4)
  portfolio/         — Portfolio tracking (Phase 4)
  tui/               — Bubble Tea dashboard (Phase 5)
  notify/            — Telegram bot (Phase 6)
  embeddings/        — pgvector embedding pipeline (Phase 8)
  knowledge/         — Knowledge graph (Phase 8)
  backtest/          — Backtesting engine (Phase 7)
  reports/           — HTML/chart report generation (Phase 13)
  web/               — Data-flow visualisation server (Phase 14)
migrations/          — SQL migration files (001–012)
docs/                — PRD + implementation plan
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

## CLI Reference

```
te run                              Start the engine
te stop                             Stop the engine (SIGTERM)
te status                           Show engine + DB status
te mode <manual|auto|paper>         Switch trading mode at runtime
te watchlist add <ticker>           Add ticker + trigger backfill
te watchlist remove <ticker>        Remove ticker
te watchlist show                   List all watchlist items
te config show                      Show active config (masked secrets)
te config set <key> <value>         Set config value (persists to YAML)
te config set --runtime <key> <val> Set config override (persists to DB, no restart)
te config validate                  Check config validity
te migrate                          Run pending database migrations
te logs tail                        Tail engine log file
te logs search <query>              Search log file
```

## Build Phase Status

| Phase | Description | Status |
|-------|-------------|--------|
| 1 | Foundation (scaffold, config, migrations, CLI) | ✅ Complete |
| 2 | Data pipeline (market data, indicators, news) | ✅ Complete |
| 3 | Analysis engine (LLM integration, prompt engineering) | 🔜 Next |
| 4 | Trading core (signals, risk, execution, portfolio) | ⏳ Planned |
| 5 | Terminal dashboard (Bubble Tea TUI) | ⏳ Planned |
| 6 | Telegram bot + approval flow | ⏳ Planned |
| 7 | Backtesting engine | ⏳ Planned |
| 8 | Knowledge graph + embeddings | ⏳ Planned |
| 9 | Prediction markets (Polymarket + Kalshi) | ⏳ Planned |
| 10–14 | Consensus, web viz, live trading | ⏳ Planned |

## Module READMEs

- [`internal/config/`](internal/config/) — config system
- [`internal/data/`](internal/data/) — market data + news providers
- [`internal/indicators/`](internal/indicators/) — technical analysis engine
- [`internal/engine/`](internal/engine/) — core engine loop + scheduler
- [`internal/store/`](internal/store/) — database access layer
