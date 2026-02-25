# TradeEngine — Product Requirements Document

**Version:** 1.0
**Author:** Romeo
**Date:** February 2026
**Status:** Pre-development

---

## 1. Product Summary

TradeEngine is an LLM-powered algorithmic trading system that monitors financial markets (stocks and prediction markets), analyzes opportunities using AI models, and executes trades — either automatically or with manual approval. The system is a single Go binary backed by PostgreSQL with pgvector, operated via a CLI and TUI (Bubble Tea), with static HTML reports for deep analysis and a Telegram bot for on-the-go trade approvals.

The system is designed to be highly configurable. The user can swap AI models, toggle between manual and automatic execution, adjust risk parameters, enable multi-model consensus, and switch strategies — all without code changes. Configuration is YAML-driven with runtime overrides via CLI flags and TUI toggles.

---

## 2. Goals

1. Build a system that monitors a configurable watchlist of stocks and prediction market contracts on a scheduled loop.
2. Fetch market data (OHLCV candles for stocks, contract price/volume snapshots for prediction markets) and compute technical indicators.
3. Feed enriched data to one or more LLMs and receive structured JSON trade recommendations (buy/sell/hold with confidence, stop-loss, take-profit, reasoning).
4. Enforce risk management rules before any trade is executed.
5. Support three execution modes: **auto** (execute immediately), **manual** (queue and await user approval via TUI or Telegram), and **paper** (simulate with fake money).
6. Track all trades, analyses, portfolio state, and model performance in PostgreSQL.
7. Use pgvector embeddings to enable semantic search over past analyses and news (e.g., "find past analyses where the market setup looked like this").
8. Maintain a lightweight knowledge graph (entities + relationships) in PostgreSQL to give the LLM context about how companies, sectors, and events are related.
9. Support backtesting strategies against historical data with cached model responses to reduce API costs.
10. Generate static HTML reports (equity curves, drawdown charts, backtest comparisons, performance heatmaps) on demand.

---

## 3. Non-Goals

1. This is NOT a high-frequency trading system. The minimum analysis interval is minutes, not milliseconds. Scalping strategies are out of scope.
2. No web application as the primary operational interface. The operational dashboard is a TUI. Reports are static HTML files opened in the browser. A future web visualization layer (Phase 14) provides a data-flow observability interface but is not the control plane.
3. No mobile app. Telegram is the mobile interface.
4. No support for options, futures, or forex in v1. Stocks and prediction markets only.
5. No multi-user support. This is a single-user system.

---

## 4. Architecture Overview

### 4.1 System Components

The system is a **single Go binary** containing all components:

| Component | Description |
|---|---|
| **Core Engine** | The main trading loop: scheduler, data fetching, indicator computation, prompt building, model querying, risk checks, execution. |
| **CLI** (`te`) | Command-line interface for all operations: run, stop, config, backtest, watchlist, trades, models, reports. |
| **TUI** (`te dashboard`) | Real-time operational dashboard built with Bubble Tea. Shows live positions, signal feed, pending approvals, risk status. |
| **Report Generator** (`te report`) | Generates self-contained HTML files with interactive charts using go-echarts. |
| **Telegram Bot** | Sends trade alerts and receives approval/rejection for pending trades in manual mode. |
| **Event Bus** | Internal publish-subscribe channel for engine events (`DataFetched`, `IndicatorsComputed`, `AnalysisCompleted`, `RiskCheckPassed`, `TradeExecuted`, etc.). Consumed by TUI, Telegram, and Web visualization. |
| **Web Visualization** (`te web`) | Real-time data-flow observability dashboard. Displays live pipeline state, data particles flowing between system nodes, and decision outputs. Built with a Go WebSocket server and a React frontend. Phase 14 deliverable. |

### 4.2 External Dependencies

| Dependency | Purpose |
|---|---|
| **PostgreSQL + pgvector** | All persistent storage: market data, trades, analyses, knowledge graph, embeddings. |
| **Alpaca API** | Stock market data (OHLCV, quotes) and brokerage (paper + live trade execution). |
| **Polymarket API** | Prediction market data and trade execution. |
| **Kalshi API** | Prediction market data and trade execution. |
| **Anthropic API (Claude)** | Primary LLM for trade analysis. |
| **OpenAI API** | Secondary LLM (optional, for multi-model consensus). |
| **Ollama** | Local LLM (optional, for multi-model consensus or cost-free analysis). |
| **News/Sentiment API** | Financial news and sentiment data (e.g., Finnhub, Benzinga). |
| **Telegram Bot API** | Trade notifications and approval flow. |
| **Embedding Model** | For generating vector embeddings. Options: OpenAI text-embedding-3-small, Voyage AI, or local via Ollama (nomic-embed-text). |

### 4.3 Tech Stack

| Layer | Technology |
|---|---|
| Language | Go |
| Database | PostgreSQL + pgvector extension |
| TUI Framework | Bubble Tea (charmbracelet/bubbletea) |
| CLI Framework | Cobra |
| Chart Generation | go-echarts (static HTML output) |
| Broker | Alpaca Go SDK |
| Configuration | YAML (go-yaml) |

### 4.4 High-Level Data Flow

```
Scheduler triggers
    → Fetch market data (Alpaca / Polymarket / Kalshi)          → emit DataFetched
    → Fetch news and sentiment (Finnhub / Benzinga)             → emit NewsFetched
    → Compute indicators (RSI, MACD, EMA, Bollinger, etc.)      → emit IndicatorsComputed
    → Fetch context (portfolio state, trade history, knowledge graph relationships)
    → Build structured prompt
    → Query LLM(s)                                              → emit AnalysisCompleted
    → Parse structured JSON response
    → If consensus mode: resolve across models                  → emit ConsensusResolved
    → Risk check (confidence, position size, portfolio          → emit RiskCheckCompleted
        exposure, daily loss, drawdown)
    → If passed:
        → auto mode: execute trade via broker                   → emit TradeExecuted
        → manual mode: queue and notify via TUI + Telegram      → emit SignalQueued
        → paper mode: simulate fill                             → emit TradeSimulated
    → Log everything to PostgreSQL

All events flow through the internal Event Bus, consumed by:
    → TUI (real-time dashboard updates)
    → Telegram (notification dispatch)
    → Web Visualization (data-flow rendering via WebSocket)
```

---

## 5. Configuration

All system behavior is driven by a single YAML file at `~/.tradeengine/config.yaml`. Runtime overrides via CLI flags and TUI toggles take precedence over the file.

### 5.1 Configuration Priority (highest to lowest)

1. Runtime CLI flags (e.g., `te run --mode=auto`)
2. TUI toggles (persisted to a `config_overrides` table in PostgreSQL, override YAML at runtime)
3. YAML config file
4. Hardcoded defaults

The `config_overrides` table stores key-value pairs set via the TUI or `te config set --runtime`. On engine startup, the config loader merges: defaults → YAML → DB overrides → CLI flags. TUI toggles write to this table so they survive engine restarts without modifying the YAML file.

### 5.2 Configuration Structure

```yaml
engine:
  mode: manual              # manual | auto | paper
  schedule: market          # market (stock hours only) | 24/7 (prediction markets) | custom cron

models:
  primary:
    provider: anthropic
    model: claude-sonnet-4-20250514
    api_key_env: ANTHROPIC_API_KEY
  secondary:
    provider: openai
    model: gpt-4o
    api_key_env: OPENAI_API_KEY
  consensus:
    enabled: false
    strategy: majority      # majority | unanimous | primary-with-veto
    min_agreement: 0.7

markets:
  stocks:
    enabled: true
    broker: alpaca
    data_source: alpaca
    api_key_env: ALPACA_API_KEY
    api_secret_env: ALPACA_SECRET_KEY
    paper: true
  predictions:
    enabled: true
    platforms:
      - name: polymarket
        api_key_env: POLYMARKET_API_KEY
      - name: kalshi
        api_key_env: KALSHI_API_KEY

watchlist:
  stocks:
    - ticker: AAPL
      strategies: [trend_following, news_sentiment]
      timeframes: [1h, 4h, 1d]
    - ticker: NVDA
      strategies: [momentum, news_sentiment]
      timeframes: [1h, 1d]
  predictions:
    - platform: polymarket
      categories: [politics, tech, crypto]
      min_volume: 50000
      max_contracts: 20

strategies:
  trend_following:
    indicators: [ema_20, ema_50, ema_200, macd, adx]
    entry_conditions:
      ema_crossover: true
      adx_min: 25
    exit_conditions:
      ema_crossunder: true
      trailing_stop_pct: 3.0

  mean_reversion:
    indicators: [rsi_14, bollinger_20_2, stochastic]
    entry_conditions:
      rsi_oversold: 30
      rsi_overbought: 70
    exit_conditions:
      rsi_neutral: 50

  momentum:
    indicators: [rsi_14, obv, atr_14, volume_sma_20]
    entry_conditions:
      breakout_confirmation: true
      volume_multiplier: 1.5
    exit_conditions:
      trailing_stop_atr_mult: 2.0

  news_sentiment:
    sources: [financial_news_api]
    min_sentiment_score: 0.6
    cooldown_minutes: 30

  prediction_mispricing:
    min_edge: 0.10
    min_confidence: 0.7
    max_time_to_resolution: 30d

risk:
  max_position_pct: 5.0
  max_sector_pct: 20.0
  max_daily_loss_pct: 3.0
  max_drawdown_pct: 15.0
  default_stop_loss_pct: 5.0
  default_take_profit_pct: 10.0
  min_risk_reward: 2.0
  max_open_positions: 10
  cool_down_after_loss_min: 15

notifications:
  telegram:
    enabled: true
    bot_token_env: TELEGRAM_BOT_TOKEN
    chat_id_env: TELEGRAM_CHAT_ID
    notify_on: [signal, trade, daily_summary, error, drawdown_warning]

logging:
  level: info                # debug | info | warn | error
  format: text               # text | json

database:
  url_env: DATABASE_URL

reports:
  output_dir: ~/.tradeengine/reports
  auto_open_browser: true
```

### 5.3 Consensus Strategies

When multiple models are enabled:

- **majority** — Act if most models agree on the action. Confidence = average of agreeing models.
- **unanimous** — Only act if all models agree. Produces the highest conviction trades only.
- **primary-with-veto** — Primary model decides, but if any secondary model strongly disagrees (e.g., primary says BUY at 0.85, secondary says SELL at 0.80), the trade is blocked and flagged for manual review.

---

## 6. CLI Specification

The CLI binary is `te`. All commands:

```
CORE
  te run [--mode=manual|auto|paper] [--model=<name>] [--dry-run]
  te stop
  te status
  te migrate                # Run pending database migrations

CONFIG
  te config show
  te config set <key> <value>
  te config validate

MODE
  te mode <manual|auto|paper>

WATCHLIST
  te watchlist add <ticker> --strategies=<s1,s2> [--timeframes=<t1,t2>]
  te watchlist remove <ticker>
  te watchlist show

BACKTESTING
  te backtest run --strategy=<name> --ticker=<ticker> --model=<model> --from=<date> --to=<date>
  te backtest compare --id=<id1> --id=<id2>
  te backtest list

TRADES
  te trades list
  te trades show <trade_id>
  te trades pending
  te trades approve <trade_id>
  te trades reject <trade_id>

MODELS
  te models list
  te models test <prompt>
  te models compare <ticker>

REPORTS
  te report daily
  te report weekly
  te report backtest <id>
  te report backtest compare <id1> <id2>
  te report performance [--from=<date>] [--to=<date>]

KNOWLEDGE GRAPH
  te graph seed            # Populate initial entities and relationships from bundled data
  te graph review          # Review auto-generated entities/relationships
  te graph entities        # List entities
  te graph relationships <ticker>  # Show relationships for a ticker

EMBEDDINGS
  te embeddings reindex    # Re-generate all embeddings with the current model

LOGS
  te logs tail
  te logs search <query>

DASHBOARD
  te dashboard             # Launch TUI

WEB
  te web [--port=8080]     # Start web data-flow visualization server
```

---

## 7. TUI Specification

The TUI is launched via `te dashboard` and built with Bubble Tea.

### 7.1 Main Dashboard View

The default view shows:

- **Header bar** — Current mode (manual/auto/paper), paper trading status.
- **Portfolio summary** — Total value, cash, invested, today's P&L.
- **Open positions table** — Ticker, entry price, current price, unrealized P&L, stop-loss, take-profit, strategy.
- **Pending approvals** — Trades awaiting manual approval with action, ticker, price, confidence, and condensed reasoning. User can press `a` to approve, `r` to reject, `d` for full details.
- **Signal feed** — Scrollable log of recent signals (HOLD, BUY, SELL, SKIP) with ticker, confidence, and one-line reasoning.
- **Risk status bar** — Daily loss usage, drawdown level, open position count, largest position percentage.
- **Keyboard shortcut bar** — `[m]ode [w]atchlist [t]rades [l]ogs [c]onfig [q]uit`

### 7.2 Additional Views

Accessible via keyboard shortcuts from the main view:

- **Trade history** — Filterable list of past trades with P&L.
- **Watchlist management** — Add/remove tickers, toggle strategies.
- **Log stream** — Real-time engine logs.
- **Config toggles** — Change mode, toggle consensus, adjust risk parameters.
- **Position detail** — Expanded view of a single position with full analysis reasoning and related signals.

### 7.3 TUI Data Refresh

The TUI polls the database on a configurable tick interval (default: 3 seconds) for portfolio updates, new signals, and pending approvals. Positions with live prices may use a faster tick (1 second) if connected to a websocket data feed.

---

## 8. Analysis Engine

### 8.1 Prompt Structure

Each analysis call to the LLM is a structured prompt containing:

1. **System prompt** — Defines the model's role, output format (JSON), strategy rules, and risk constraints. This is static per strategy.
2. **Market data** — Current and recent OHLCV data, current price, volume.
3. **Computed indicators** — All indicator values relevant to the strategy (e.g., RSI=42, MACD histogram=0.35, 50-day EMA=138.20).
4. **Portfolio context** — Current cash, open positions, recent trades on this ticker.
5. **Knowledge graph context** — Related entities and relationships (e.g., "NVDA supplies to MSFT, GOOGL. Manufactured by TSMC. Competitor: AMD").
6. **Recent related news** — Headlines and sentiment for this ticker and related entities from the last 48 hours.
7. **Similar past analyses** (via pgvector) — Optionally include 2-3 past analyses with similar market setups and their outcomes.

### 8.2 Expected Model Output

The model must return valid JSON matching this structure:

```json
{
  "action": "BUY",
  "confidence": 0.82,
  "size_pct": 2.0,
  "stop_loss": 128.50,
  "take_profit": 145.00,
  "reasoning": "Breakout above $141 resistance on 1.8x average volume. RSI at 62 with room to run. MACD histogram expanding. Recent TSMC earnings positive for supply chain."
}
```

- `action`: one of `BUY`, `SELL`, `HOLD`
- `confidence`: 0.0 to 1.0
- `size_pct`: recommended position size as percentage of portfolio
- `stop_loss`: suggested stop-loss price (null for HOLD)
- `take_profit`: suggested take-profit price (null for HOLD)
- `reasoning`: natural language explanation

### 8.3 Prediction Market Prompt Differences

For prediction markets, the prompt structure differs:

- Instead of OHLCV and technical indicators, provide: contract price (= implied probability), volume, time to resolution, bid/ask spread.
- Instead of asking "what does the technical setup suggest?", ask: "Is this contract mispriced? What is the true probability of this event based on available evidence?"
- Include related prediction market contracts and their current prices for cross-market reasoning.
- The expected output includes `estimated_probability` instead of `stop_loss`/`take_profit`:

```json
{
  "action": "BUY",
  "confidence": 0.78,
  "size_pct": 1.5,
  "estimated_probability": 0.72,
  "market_probability": 0.58,
  "edge": 0.14,
  "reasoning": "Market implies 58% but recent polling data and legislative calendar suggest 72%..."
}
```

### 8.4 Multi-Model Flow

When consensus is enabled:

1. The same prompt is sent to all configured models in parallel (via goroutines).
2. Each model's JSON response is parsed independently.
3. The consensus resolver compares actions and confidences according to the configured strategy (majority, unanimous, or primary-with-veto).
4. The resolved signal is passed to the risk manager.
5. All individual model responses are stored in the `analyses` table for later comparison.

---

## 9. Risk Management

The risk manager enforces constraints defined in the `risk` section of the config. Every signal must pass ALL checks before execution.

### 9.1 Risk Checks (evaluated in order)

| Check | Rule | Behavior on Failure |
|---|---|---|
| Confidence threshold | `confidence >= strategy.min_confidence` (or global default) | Signal marked as SKIP |
| Risk-reward ratio | `(take_profit - entry) / (entry - stop_loss) >= min_risk_reward` | Signal blocked |
| Position size limit | `size_pct <= max_position_pct` | Reduce size to max, or block if too small to be meaningful |
| Portfolio concentration | Total exposure to one sector <= `max_sector_pct` | Signal blocked |
| Max open positions | Open position count < `max_open_positions` | Signal blocked |
| Daily loss limit | Today's realized losses < `max_daily_loss_pct` of portfolio | All trading paused for the day |
| Max drawdown circuit breaker | Current drawdown from peak < `max_drawdown_pct` | All trading paused until manual review |
| Cooldown | Time since last losing trade > `cool_down_after_loss_min` | Signal delayed |

### 9.2 Logging

Every risk check result is logged — both passes and failures. The signal record in the database includes the `block_reason` if the signal was blocked, so the user can review why signals were not acted on.

---

## 10. Execution

### 10.1 Mode Router

After a signal passes risk checks, it is routed based on the current engine mode:

- **auto** — Order is placed immediately with the appropriate broker.
- **manual** — Signal is queued as `pending` in the database. TUI updates to show the pending trade. Telegram sends a notification with trade details and approve/reject buttons.

### 10.1.1 Signal Expiry and Staleness

Pending signals in manual mode are subject to automatic expiry, enforced by a background goroutine in the engine (not by TUI or Telegram):

- **Time-based expiry** — Signals expire after a configurable window (default: 30 minutes). Expired signals are marked `expired` in the database.
- **Price-based staleness** — If the current price moves more than a configurable threshold (default: 2%) from the signal price, the signal is auto-cancelled as `stale`.
- The expiry checker runs on each engine tick, independent of whether the TUI or Telegram bot is connected.
- Both TUI and Telegram display expiry/stale status for cancelled signals.
- **paper** — No real orders are placed. All tracking (P&L, portfolio state) works identically to live trading. Two paper execution backends are available:
  - **Alpaca paper API** (default when `markets.stocks.paper: true`) — Uses Alpaca's paper trading environment for realistic order lifecycle (partial fills, slippage, market hours enforcement). Orders appear in the Alpaca dashboard.
  - **Local simulator** (used during backtesting or when no broker is configured) — Simulates fills instantly at the current market price. Used by the backtesting engine to avoid broker API dependencies.

### 10.2 Order Placement

For stock trades via Alpaca:

1. Place the entry order (market or limit based on strategy config).
2. Once filled, place a stop-loss order and a take-profit order as OCO (one-cancels-other) if supported, or as separate orders.
3. Track all orders in the `orders` table with broker order IDs for reconciliation.

For prediction market trades:

1. Place a limit order at the desired price on the platform's order book.
2. Track fill status.
3. No stop-loss/take-profit orders (prediction markets typically don't support these natively). The system must monitor positions and place sell orders when exit conditions are met.

### 10.3 Position Monitoring

Open positions are monitored on every scheduler tick. The system checks:

- Has the stop-loss price been hit? (If the broker didn't auto-execute it.)
- Has the take-profit price been hit?
- Has the strategy generated an exit signal?
- For prediction markets: has the contract resolved?

---

## 11. Backtesting

### 11.1 Backtest Execution

The backtesting engine replays historical data through the same analysis pipeline used in live trading.

1. Load historical candle data for the specified ticker and date range. If data isn't cached locally, fetch from the market data API and store in the `candles` table for future use.
2. Iterate through candles chronologically. At each step:
   a. Compute indicators using only data available up to the current candle (no look-ahead bias).
   b. Build the prompt using the same template as live trading.
   c. Query the model. Cache the response by prompt hash in the `prompt_cache` table.
   d. If the model recommends a trade, simulate execution at the next candle's open price (to avoid look-ahead bias on the current candle's close).
   e. Track the virtual portfolio (positions, cash, P&L).
3. After all candles are processed, compute performance metrics.
4. Store the run and all simulated trades in `backtest_runs` and `backtest_trades`.
5. Print a summary to the terminal.

### 11.2 Caching

Model API calls are the expensive part of backtesting. The prompt cache works as follows:

- Before querying the model, hash the full prompt content.
- Check `prompt_cache` for a matching hash + model provider + model name.
- If cached, use the cached response (zero API cost, instant).
- If not cached, call the API and cache the response.
- Re-running the same backtest with the same model is instant.
- Changing strategy parameters invalidates the cache (different indicators = different prompt).
- Switching models always requires fresh API calls.
- `--no-cache` flag forces fresh calls.

### 11.3 Performance Metrics

Every backtest run computes and stores:

- Total return (%)
- Annualized return (%)
- Sharpe ratio
- Sortino ratio
- Calmar ratio
- Max drawdown (%)
- Win rate (%)
- Profit factor
- Expectancy ($ per trade)
- Total trade count
- Average hold time
- Best and worst trade (%)

### 11.4 Comparison

`te backtest compare --id=<bt1> --id=<bt2>` compares two runs side-by-side in the terminal. `te report backtest compare <bt1> <bt2>` generates a full HTML report with overlaid equity curves, divergence analysis, and metric comparison tables.

---

## 12. Knowledge Graph

### 12.1 Structure

The knowledge graph is stored in two PostgreSQL tables: `entities` and `relationships`.

**Entities** represent companies, sectors, people, countries, commodities, events, prediction contracts, and indexes. Each entity has a type, name, optional ticker, description, and a pgvector embedding for semantic search.

**Relationships** are typed, directed edges between entities. Supported types: `supplies_to`, `supplied_by`, `competes_with`, `subsidiary_of`, `parent_of`, `partner_of`, `sector_member`, `manufactures_for`, `manufactured_by`, `regulates`, `regulated_by`, `affects_contract`, `affected_by_event`. Each relationship has a strength (0.0 to 1.0), a description, and optional temporal bounds (`valid_from`, `valid_until`).

### 12.2 Usage in Analysis

Before building the analysis prompt for a ticker, the system:

1. Looks up the entity for the ticker.
2. Queries relationships (1-2 hops) to find connected entities.
3. Checks `entity_mentions` for recent news affecting related entities.
4. Injects this as structured context in the prompt.

### 12.3 Self-Enrichment

The knowledge graph grows over time via two mechanisms:

1. **Manual** — The user adds entities and relationships via `te graph` commands.
2. **Automatic** — When Claude processes news articles, an extraction step in the prompt asks it to identify any company relationships mentioned. Extracted relationships are inserted with `auto_generated = true` and a confidence score. The user can review and confirm/reject these via `te graph review`.

---

## 13. Embeddings (pgvector)

### 13.1 What Gets Embedded

| Table | Embedded Content | Dimension | Purpose |
|---|---|---|---|
| `analyses` | reasoning text + indicator values summary | Depends on model | Find past analyses with similar market setups across tickers and time |
| `news_items` | headline + summary | Depends on model | Deduplicate similar stories, find relevant news for a given context |
| `entities` | name + description | Depends on model | Semantic entity search ("companies affected by chip export bans") |

### 13.2 What Does NOT Get Embedded

Raw market data (candles, prediction snapshots), trades, orders, and portfolio snapshots. These are structured data queried efficiently with standard SQL indexes.

### 13.3 Embedding Generation

Embeddings are generated **asynchronously** by a background goroutine, not in the hot trading loop. After an analysis is logged, a news item is stored, or an entity is created, the embedding is generated and the row is updated. This prevents embedding API latency from affecting trade execution speed.

### 13.4 Embedding Model Options

The schema uses a configurable vector dimension. Options:

- **OpenAI `text-embedding-3-small`** — 1536 dimensions. Cheap, fast, good quality.
- **Voyage AI `voyage-3-lite`** — 512 dimensions. Smaller vectors, lower storage.
- **Ollama `nomic-embed-text`** — 768 dimensions. Free, runs locally on the VPS.

The choice of model is a config option. The vector column dimension in the schema must match.

### 13.5 Embedding Dimension Strategy

The embedding dimension is set once at initial deployment via a migration that creates the vector columns. Changing the embedding model after data has been stored requires:

1. A new migration to alter the vector column dimension.
2. A backfill job (`te embeddings reindex`) that re-embeds all existing rows with the new model.
3. Old embeddings are dropped during the backfill — there is no mixed-dimension state.

To minimize this risk, choose the embedding model before Phase 11 deployment and treat it as a durable infrastructure decision. The recommended default is OpenAI `text-embedding-3-small` (1536 dimensions) for quality, or Ollama `nomic-embed-text` (768 dimensions) for cost-free local operation.

---

## 14. Static HTML Reports

### 14.1 Generation

Reports are generated via `te report <type>` and saved as self-contained HTML files in `~/.tradeengine/reports/`. They use go-echarts for interactive, zoomable charts. No web server is required — the file is opened directly in the user's browser.

### 14.2 Report Types

**Daily Report** (`te report daily`)
- Today's trades with outcomes
- Signals generated (acted on + blocked)
- Portfolio value change
- Model cost (tokens used)

**Weekly Report** (`te report weekly`)
- Week's equity curve
- Win/loss breakdown
- Strategy performance comparison
- Top and bottom trades

**Performance Report** (`te report performance`)
- Equity curve (interactive, zoomable)
- Drawdown chart
- Monthly returns heatmap
- All key metrics (Sharpe, Sortino, Calmar, win rate, expectancy, profit factor)
- Breakdown by strategy, by model, by ticker
- Top 5 best and worst trades with reasoning excerpts

**Backtest Report** (`te report backtest <id>`)
- Equity curve of the backtest
- Drawdown chart
- All metrics
- Per-trade scatter plot

**Backtest Comparison** (`te report backtest compare <id1> <id2>`)
- Overlaid equity curves
- Side-by-side metrics table
- Divergence analysis

---

## 15. Notifications (Telegram)

### 15.1 Notification Types

Configurable via `notifications.telegram.notify_on`:

- **signal** — New trade signal generated (after risk check pass).
- **trade** — Trade executed (entry or exit).
- **daily_summary** — End-of-day summary of all activity and P&L.
- **error** — System errors (API failures, data issues).
- **drawdown_warning** — Portfolio drawdown approaching or exceeding the configured limit.

### 15.2 Manual Approval via Telegram

In manual mode, pending trade notifications include inline buttons:

- **Approve** — Execute the trade.
- **Reject** — Cancel the signal.
- **Full Analysis** — Show expanded reasoning from the model(s).

The Telegram bot runs as a goroutine within the main binary, listening for callback queries from button presses.

---

## 16. Database Schema

The full PostgreSQL schema is defined in the companion document `tradeengine-database-schema.md`. Summary of tables:

**Market Data:** `candles`, `prediction_snapshots`, `news_items` (with embedding)
**Knowledge Graph:** `entities` (with embedding), `relationships`, `entity_mentions`
**Analysis:** `analyses` (with embedding)
**Trading:** `signals`, `trades`, `orders`
**Portfolio:** `portfolio_snapshots`, `daily_summaries`
**Backtesting:** `backtest_runs`, `backtest_trades`, `prompt_cache`
**System:** `watchlist`, `config_history`, `config_overrides`
**Views:** `open_positions`, `recent_signals`, `model_performance`, `strategy_performance`

### 16.1 Data Retention

| Data | Retention |
|---|---|
| Daily + hourly candles | Forever |
| 1m/5m/15m candles | 90 days |
| Prediction snapshots | Forever (irreplaceable) |
| News items | Forever |
| Analyses | Forever |
| Trades / Orders | Forever |
| Portfolio snapshots | Forever |
| Prompt cache | 6 months |
| Entities / Relationships | Forever |

Retention is enforced via cleanup functions (`cleanup_intraday_candles`, `cleanup_prompt_cache`) implemented as a scheduled task in the engine's scheduler. The cleanup runs once daily (configurable) as a low-priority background goroutine. Cleanup logic lives in `internal/store/retention.go`.

---

## 17. Project Structure

```
tradeengine/
├── cmd/
│   └── te/
│       └── main.go                     # CLI entry point (cobra)
├── internal/
│   ├── config/
│   │   ├── config.go                   # YAML parsing, validation, defaults, DB override merging
│   │   └── defaults.go                 # Default values
│   ├── engine/
│   │   ├── engine.go                   # Core loop orchestration
│   │   └── scheduler.go               # Cron/ticker scheduling
│   ├── events/
│   │   ├── bus.go                     # Publish-subscribe event bus (channel-based)
│   │   └── types.go                   # Event type definitions (DataFetched, TradeExecuted, etc.)
│   ├── data/
│   │   ├── provider.go                 # MarketDataProvider interface
│   │   ├── alpaca.go                   # Alpaca data adapter
│   │   ├── polymarket.go              # Polymarket data adapter
│   │   ├── kalshi.go                  # Kalshi data adapter
│   │   └── news.go                    # News/sentiment adapter (Finnhub, Benzinga)
│   ├── indicators/
│   │   ├── indicators.go              # Indicator computation interface
│   │   ├── technical.go               # RSI, MACD, EMA, Bollinger, ADX, ATR, OBV, VWAP, Stochastic
│   │   └── prediction.go             # Probability curves, time decay, volume trends
│   ├── analysis/
│   │   ├── analyzer.go                # Prompt building + model querying orchestration
│   │   ├── models.go                  # ModelProvider interface
│   │   ├── anthropic.go               # Claude adapter
│   │   ├── openai.go                  # GPT adapter
│   │   ├── ollama.go                  # Local model adapter
│   │   ├── consensus.go               # Multi-model consensus resolution
│   │   └── prompts/
│   │       ├── stock.go               # Stock analysis prompt templates
│   │       └── prediction.go          # Prediction market prompt templates
│   ├── risk/
│   │   └── manager.go                 # All risk checks
│   ├── execution/
│   │   ├── executor.go                # Executor interface
│   │   ├── alpaca_broker.go           # Stock order execution via Alpaca (paper + live)
│   │   ├── polymarket_exec.go        # Polymarket execution
│   │   ├── kalshi_exec.go            # Kalshi execution
│   │   └── paper.go                   # Local paper trading simulator (backtesting, no-broker mode)
│   ├── portfolio/
│   │   └── portfolio.go               # Position tracking, P&L calculation, snapshots
│   ├── knowledge/
│   │   ├── graph.go                   # Entity and relationship CRUD
│   │   ├── enricher.go                # Auto-extraction of relationships from news
│   │   └── context.go                 # Build knowledge context for prompts
│   ├── embeddings/
│   │   ├── embedder.go                # Embedding generation interface
│   │   ├── openai.go                  # OpenAI embeddings
│   │   ├── ollama.go                  # Local embeddings via Ollama
│   │   └── worker.go                  # Async background embedding worker
│   ├── backtest/
│   │   ├── runner.go                  # Backtest replay engine
│   │   ├── cache.go                   # Prompt response caching
│   │   └── metrics.go                 # Performance metric calculation
│   ├── notify/
│   │   └── telegram.go                # Telegram bot, alerts, approval flow
│   ├── tui/
│   │   ├── app.go                     # Bubble Tea application
│   │   ├── views/
│   │   │   ├── dashboard.go           # Main dashboard view
│   │   │   ├── positions.go           # Position detail view
│   │   │   ├── signals.go             # Signal feed view
│   │   │   ├── trades.go              # Trade history view
│   │   │   ├── watchlist.go           # Watchlist management view
│   │   │   └── config.go              # Config toggle view
│   │   └── components/
│   │       ├── table.go               # Reusable table component
│   │       ├── sparkline.go           # ASCII sparkline for inline charts
│   │       └── statusbar.go           # Status/shortcut bar
│   ├── web/
│   │   ├── server.go                  # HTTP + WebSocket server for data-flow visualization
│   │   └── handlers.go                # WebSocket event streaming, REST endpoints
│   ├── reports/
│   │   ├── generator.go               # HTML report builder
│   │   ├── charts.go                  # go-echarts chart creation
│   │   └── templates/
│   │       ├── performance.html       # Performance report template
│   │       ├── backtest.html          # Backtest report template
│   │       └── comparison.html        # Backtest comparison template
│   └── store/
│       ├── db.go                      # Database connection, migration runner
│       ├── candles.go                 # Candle data CRUD
│       ├── news.go                    # News item CRUD
│       ├── watchlist.go               # Watchlist CRUD
│       ├── analyses.go                # Analysis log CRUD
│       ├── signals.go                 # Signal CRUD
│       ├── trades.go                  # Trade CRUD
│       ├── orders.go                  # Order CRUD (broker order tracking)
│       ├── portfolio.go               # Portfolio snapshot CRUD
│       ├── predictions.go             # Prediction snapshot CRUD
│       ├── backtests.go               # Backtest run CRUD
│       ├── entities.go                # Entity CRUD
│       ├── relationships.go           # Relationship + entity_mentions CRUD
│       ├── overrides.go               # DB-backed config override CRUD
│       └── retention.go               # Data retention cleanup functions
├── web/
│   └── frontend/                      # React frontend for data-flow visualization (Phase 14)
│       ├── package.json
│       ├── src/
│       └── ...
├── migrations/
│   ├── 001_extensions.sql
│   ├── 002_enums.sql
│   ├── 003_market_data.sql
│   ├── 004_knowledge_graph.sql
│   ├── 005_analysis.sql
│   ├── 006_signals_and_trades.sql
│   ├── 007_portfolio.sql
│   ├── 008_backtesting.sql
│   ├── 009_system.sql
│   ├── 010_helper_views.sql
│   ├── 011_retention_policy.sql
│   └── 012_config_overrides.sql
├── config.example.yaml
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

---

## 18. Key Interfaces

The system relies on Go interfaces for swappable components:

```go
// Market data providers (Alpaca, Polymarket, Kalshi)
type MarketDataProvider interface {
    FetchCandles(ctx context.Context, ticker string, tf Timeframe, from, to time.Time) ([]Candle, error)
    FetchQuote(ctx context.Context, ticker string) (*Quote, error)
    FetchHistory(ctx context.Context, ticker string, tf Timeframe, limit int) ([]Candle, error)
}

// Prediction market data
type PredictionDataProvider interface {
    FetchContracts(ctx context.Context, categories []string, minVolume float64) ([]Contract, error)
    FetchContractSnapshot(ctx context.Context, contractID string) (*ContractSnapshot, error)
}

// LLM providers (Claude, GPT, Ollama)
type ModelProvider interface {
    Analyze(ctx context.Context, prompt string) (*AnalysisResponse, error)
    Name() string
    Provider() string
}

// Trade execution (Alpaca, Polymarket, Kalshi, Paper)
type Executor interface {
    PlaceOrder(ctx context.Context, order *Order) (*OrderResult, error)
    CancelOrder(ctx context.Context, orderID string) error
    GetPositions(ctx context.Context) ([]Position, error)
    GetOrderStatus(ctx context.Context, orderID string) (*OrderStatus, error)
}

// Embedding generation
type Embedder interface {
    Embed(ctx context.Context, text string) ([]float32, error)
    Dimensions() int
}
```

---

## 19. Deployment

### 19.1 Target Environment

A VPS (Hetzner, DigitalOcean, or similar) running Ubuntu. The Go binary runs as a systemd service for auto-restart and boot persistence. PostgreSQL runs either locally on the same VPS or as a managed instance.

### 19.2 Requirements

- Go 1.22+
- PostgreSQL 15+ with pgvector extension
- Stable internet (API access to Alpaca, Anthropic, Telegram, etc.)
- Sufficient memory for local Ollama models if using local LLM/embeddings (8GB+ recommended)

### 19.3 Local Development

For development and paper trading, the entire system runs locally. The engine, TUI, and Postgres all run on the developer's machine. The only requirement is that the machine stays on during market hours if running the live loop.

---

## 20. Build Phases

| Phase | Scope |
|---|---|
| **1. Foundation** | Project scaffold, config system (YAML + DB overrides), database + migrations, CLI skeleton, structured logging |
| **2. Data Pipeline** | Alpaca market data, news/sentiment pipeline, indicator computation, data caching |
| **3. Analysis Engine** | Claude API integration, stock prompt template (with news), JSON parsing, multi-model interface |
| **4. Trading Core** | Signal generation, risk manager, Alpaca paper trading, order lifecycle, position tracking, P&L, signal expiry, data retention |
| **5. TUI** | Event bus, Bubble Tea dashboard, live positions, signal feed, trade approval, config toggles |
| **6. Notifications** | Telegram bot, trade alerts, approve/reject buttons, daily summaries, error/warning alerts |
| **7. Backtesting** | Historical data loader, replay engine, prompt caching, metrics, CLI integration |
| **8. Reports** | go-echarts integration, performance/backtest/comparison HTML templates |
| **9. Prediction Markets** | Polymarket/Kalshi data adapters, contract auto-discovery, prediction prompts, execution adapters |
| **10. Knowledge Graph** | Entity/relationship CRUD, seed data, prompt context injection, auto-enrichment from news |
| **11. Embeddings** | pgvector integration, dimension strategy, async embedding worker, semantic search in prompt builder |
| **12. Multi-Model Consensus** | OpenAI/Ollama adapters, consensus resolver, model management CLI |
| **13. Go Live** | Switch from paper to live, conservative sizing, monitoring, iteration |
| **14. Web Data Flow Visualization** | Go WebSocket server, React frontend, real-time data-flow graph, pipeline node inspection |

Phases 1-6 produce a functional paper trading bot with manual approval. This is the MVP.

Phases 7-12 add depth: backtesting, reports, prediction markets, knowledge graph, embeddings, and multi-model support.

Phase 13 is the transition to real money and is ongoing.

Phase 14 is the web-based observability layer — a futuristic visualization of data flowing through the system in real time.

---

## 21. Web Data Flow Visualization

### 21.1 Purpose

The web visualization is a real-time observability interface that renders the engine's data pipeline as an animated flow graph. It demonstrates how data enters the system from external sources, transforms through internal processing stages, and produces decisions that flow out as exchange actions. This is not a control plane — trades are managed via the TUI or Telegram. The web layer is read-only.

### 21.2 Architecture

- **Go WebSocket server** — `te web` starts an HTTP server (default port 8080) that subscribes to the engine's event bus and streams structured events to connected browser clients over WebSocket.
- **React frontend** — A single-page application built with React, React Flow (or D3.js) for the graph layout, and Framer Motion for animations. Bundled as static assets embedded in the Go binary (via `embed`).
- **No separate process** — The web server runs as a goroutine within the main engine binary, similar to the Telegram bot.

### 21.3 Visualization Concept

The interface renders a directed graph with three layers:

1. **Source nodes** (left) — Alpaca, Polymarket, Kalshi, Finnhub/Benzinga. Each node pulses when data arrives. Metadata on hover: last fetch time, data point count, latency.
2. **Processing pipeline** (center) — Nodes for: Data Enrichment → Indicator Computation → Prompt Assembly → LLM Analysis → Consensus Resolution → Risk Checks. Live data particles travel along edges between nodes, color-coded by type (market data, news, LLM responses, signals).
3. **Decision output** (right) — Executed trades, blocked signals, queued approvals flowing out to exchange endpoints. Green particles for executed buys, red for sells, gray for blocked.

Clicking any node drills into its current state: the risk manager node shows active check results, a ticker node shows its latest analysis, the LLM node shows the most recent prompt/response.

### 21.4 Visual Aesthetic

Dark background with glowing edges. Particle systems along data paths. Bloom/glow effects on active nodes. Monospace typography. Cyan, magenta, and amber accent palette. The intent is a futuristic mission-control feel.

### 21.5 CLI

```
WEB
  te web [--port=8080]       # Start the web visualization server
```

---

## 22. Companion Documents

These documents should be created and referenced alongside this PRD:

| Document | Contents |
|---|---|
| `trading-crash-course.md` | Trading concepts, strategies, indicators, and metrics primer |
| `tradeengine-architecture-v2.mermaid` | High-level architecture diagram (Mermaid) |
| `tradeengine-system-flow-v2.md` | Detailed system flows, TUI mockup, CLI spec, project structure |
| `tradeengine-database-schema.md` | Full PostgreSQL migration SQL with pgvector, knowledge graph, views, retention |
