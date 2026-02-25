# TradeEngine — Implementation Plan

Actionable phases derived from the PRD. Each phase is structured as an independently testable Epic (EIC) with a clear goal, definition of done, concrete tasks, acceptance criteria, and explicit dependencies on prior phases. Phases are sequential — each builds on the previous. Do not skip ahead.

---

## Phase 1: Foundation

**Goal:** Project scaffold, configuration system (YAML + DB overrides), database with migrations, working CLI skeleton, and structured logging.

**Dependencies:** None — this is the starting phase.

**Definition of Done:** `te config show` prints the parsed YAML config. `te config validate` passes with a valid config. `te status` reports "Engine not running" and confirms database connectivity. `te migrate` creates all tables. The binary compiles and installs as `te`. All commands from the PRD appear in `te --help` (stubs are fine).

### Tasks

**1.1 — Initialize the Go project**
- `go mod init github.com/romeo/tradeengine`
- Create the directory structure from the PRD (section 17). All directories created, all files can be empty stubs except `main.go`.
- Set up `cmd/te/main.go` as the entry point with Cobra.
- Add a `Makefile` with targets: `build`, `install`, `test`, `lint`, `migrate`.
- Acceptance: `make build` produces a `te` binary. `te --help` prints available commands.

**1.2 — Configuration system**
- Create `internal/config/config.go` with Go structs matching the full YAML schema from the PRD (section 5.2), including the `logging` section.
- Create `internal/config/defaults.go` with sensible defaults for every field.
- Parse `~/.tradeengine/config.yaml` on startup. If the file doesn't exist, create it from defaults.
- Support environment variable resolution for fields ending in `_env` (e.g., `api_key_env: ANTHROPIC_API_KEY` reads from `$ANTHROPIC_API_KEY`).
- Implement `te config show` — pretty-prints the resolved config (with secrets masked).
- Implement `te config set <key> <value>` — updates the YAML file. Use dot notation for nested keys (e.g., `te config set engine.mode auto`).
- Implement `te config validate` — checks for required fields, valid enum values, API keys present in environment.
- Create `config.example.yaml` at the project root with all fields documented.
- Acceptance: `te config validate` passes with a valid config. `te config set engine.mode auto` updates the file and `te config show` reflects the change.

**1.3 — DB-backed config overrides**
- Create migration `012_config_overrides.sql` with a `config_overrides` table: `key TEXT PRIMARY KEY, value TEXT, updated_at TIMESTAMPTZ`.
- Create `internal/store/overrides.go` — CRUD for the `config_overrides` table (`GetOverride`, `SetOverride`, `ListOverrides`, `DeleteOverride`).
- Extend the config loader to merge overrides: defaults → YAML → DB overrides → CLI flags.
- Add `te config set --runtime <key> <value>` variant that writes to the DB instead of the YAML file.
- Acceptance: Setting `engine.mode` via `--runtime` flag persists to DB. Engine restart reads the override. `te config show` displays the merged result with override source annotated.

**1.4 — Database setup**
- Create `internal/store/db.go` with a PostgreSQL connection pool (use `pgx`).
- Connection string read from `DATABASE_URL` env var (as configured in YAML).
- Create all migration files in `migrations/` matching the schema document (`001_extensions.sql` through `012_config_overrides.sql`). Write the SQL directly — do not depend on the companion schema document existing as a file.
- Implement a migration runner in `internal/store/db.go` that tracks applied migrations in a `schema_migrations` table and applies pending ones in order.
- Add a `te migrate` CLI command that runs pending migrations.
- Add migration execution to engine startup (auto-migrate on `te run`).
- Acceptance: `te migrate` creates all tables, indexes, views, and functions. Running it again is a no-op. Connecting to the database and running `\dt` shows all expected tables.

**1.5 — CLI skeleton**
- Register all top-level Cobra commands from the PRD (section 6), including `te migrate`, `te graph seed`, `te web`, and `te embeddings reindex`. Unimplemented commands print "not implemented yet."
- Commands to implement fully in this phase: `te config show`, `te config set`, `te config validate`, `te status`, `te migrate`, `te run`, `te stop`.
- `te status` should connect to the database and report: engine running/stopped, current mode, database connectivity, number of watchlist items.
- `te run` should start a placeholder loop that logs "Engine running in {mode} mode" every 10 seconds and gracefully shuts down on SIGINT/SIGTERM. On startup, write the engine PID to `~/.tradeengine/engine.pid`.
- `te stop` should read the PID file, send SIGTERM to the running engine process, and confirm shutdown. If no engine is running, report "Engine not running."
- Acceptance: Every command from the PRD is registered (shows in `te --help`). `te run` starts, logs, and stops cleanly on Ctrl+C. `te stop` from another terminal shuts down the running engine.

**1.6 — Structured logging**
- Set up structured logging using `slog` from the standard library.
- Log level and format configurable via `config.yaml` (`logging.level`, `logging.format`).
- Implement `te logs tail` — streams logs from stdout of the running engine (basic implementation: `tail -f` on a log file).
- Implement `te logs search <query>` — searches log output for a keyword/pattern. Basic implementation using grep over the log file.
- Acceptance: Running `te run` produces structured log output with timestamps and levels. `te logs tail` streams live output. `te logs search` finds matching entries.

### Phase Test Plan

Run the following sequence to verify Phase 1:
1. `make build` — binary compiles.
2. `te --help` — all commands listed.
3. `te config validate` — passes with a valid `config.yaml`.
4. `te config set engine.mode auto` then `te config show` — mode is `auto`.
5. `te config set --runtime risk.max_position_pct 3.0` — persists to DB.
6. `te migrate` — all tables created (verify with `\dt` in psql).
7. `te status` — reports engine stopped, DB connected.
8. `te run` — starts, logs structured output, writes PID file, stops on Ctrl+C.
9. `te run` in background, then `te stop` from another terminal — engine shuts down, PID file removed.
10. `te logs tail` — streams output while engine runs.
11. `te logs search "Engine running"` — finds matching log entries.

---

## Phase 2: Data Pipeline

**Goal:** Fetch real market data from Alpaca and financial news from a news API. Compute technical indicators and store everything in the database.

**Dependencies:** Phase 1 (database, config, CLI).

**Definition of Done:** Running `te run` fetches OHLCV candles for every stock in the watchlist on each tick. Indicators are computed and logged. News headlines are fetched and stored. Historical data is backfilled when a ticker is added. You can query both the `candles` and `news_items` tables and see real data.

### Tasks

**2.1 — Market data provider interface**
- Create `internal/data/provider.go` with the `MarketDataProvider` interface from the PRD (section 18).
- Define the `Candle`, `Quote`, and `Timeframe` types.
- Acceptance: Interface compiles. Types are usable across the codebase.

**2.2 — Alpaca data adapter**
- Create `internal/data/alpaca.go` implementing `MarketDataProvider`.
- Use Alpaca's market data API (REST with `net/http`, or the official Go SDK if clean).
- Implement `FetchCandles` — fetches OHLCV bars for a ticker, timeframe, and date range.
- Implement `FetchQuote` — fetches the latest quote (bid, ask, last price).
- Implement `FetchHistory` — fetches the last N candles for a ticker and timeframe.
- Handle rate limiting (Alpaca allows 200 requests/minute on the free tier).
- Acceptance: Calling `FetchCandles("AAPL", "1d", 30daysAgo, today)` returns 30 daily candles with valid OHLCV data.

**2.3 — Candle storage**
- Create `internal/store/candles.go` with CRUD operations for the `candles` table.
- `UpsertCandles(ticker, timeframe, []Candle)` — bulk insert with ON CONFLICT DO NOTHING (idempotent).
- `GetCandles(ticker, timeframe, from, to)` — retrieve candles for a date range.
- `GetLatestCandle(ticker, timeframe)` — retrieve the most recent candle.
- Acceptance: Candles fetched from Alpaca are stored in the database. Reinserting the same candles doesn't create duplicates.

**2.4 — Watchlist management**
- Create `internal/store/watchlist.go` — CRUD for the `watchlist` table.
- Implement `te watchlist add <ticker> --strategies=trend_following,momentum --timeframes=1h,1d`.
- Implement `te watchlist remove <ticker>`.
- Implement `te watchlist show` — prints a table of all watchlist items with their strategies and timeframes.
- On `te watchlist add`, backfill historical candles for the ticker (last 2 years of daily candles, last 30 days of hourly candles).
- Acceptance: Adding AAPL to the watchlist triggers a backfill. `te watchlist show` lists it. The `candles` table has ~500 daily candles and ~30 days of hourly candles for AAPL.

**2.5 — Indicator computation engine**
- Create `internal/indicators/indicators.go` — define the `IndicatorSet` struct containing all computed values.
- Create `internal/indicators/technical.go` — implement the following indicators from raw candle data:
  - **EMA** (Exponential Moving Average) — for periods 20, 50, 200
  - **SMA** (Simple Moving Average) — for periods 20, 50, 200
  - **RSI** (Relative Strength Index) — period 14
  - **MACD** — (12, 26, 9) with signal line and histogram
  - **Bollinger Bands** — (20, 2) with upper, middle, lower
  - **ADX** (Average Directional Index) — period 14
  - **ATR** (Average True Range) — period 14
  - **OBV** (On-Balance Volume)
  - **Stochastic Oscillator** — (14, 3, 3) with %K and %D
  - **VWAP** (Volume-Weighted Average Price)
  - **Volume SMA** — period 20
- Each indicator function takes a slice of candles and returns the computed value(s) for the most recent candle, plus a short history if needed for the prompt.
- Acceptance: Unit tests pass for each indicator with known inputs and expected outputs. Computing indicators on 200 candles of AAPL produces reasonable values (e.g., RSI between 0-100, EMA close to recent price).

**2.6 — News and sentiment pipeline**
- Create `internal/data/news.go` — define a `NewsProvider` interface with `FetchNews(ticker string, since time.Time) ([]NewsItem, error)`.
- Implement an adapter for Finnhub or Benzinga (choose based on API quality and free tier).
- `NewsItem` struct: `headline`, `summary`, `source`, `url`, `published_at`, `tickers []string`, `sentiment_score float64` (if provided by the API, otherwise leave as 0 for now).
- Create `internal/store/news.go` — CRUD for the `news_items` table.
- `UpsertNewsItems([]NewsItem)` — idempotent insert keyed on URL or external ID.
- `GetRecentNews(ticker, since)` — retrieve recent news for a ticker.
- Fetch news on each engine tick (or on a separate, slower cadence — e.g., every 15 minutes) for all watchlist tickers.
- Acceptance: After running for one tick, `news_items` table contains headlines for watchlist tickers. `GetRecentNews("AAPL", 48h)` returns results.

**2.7 — Integrate data pipeline into engine loop**
- Modify the engine loop (`internal/engine/engine.go`) to:
  1. On each tick, iterate through the watchlist.
  2. For each item, fetch the latest candles from Alpaca (or from the DB if recently cached).
  3. Compute indicators based on the strategies configured for that item.
  4. Fetch recent news for the ticker.
  5. Log the computed indicators and news count.
- Implement the scheduler (`internal/engine/scheduler.go`) with configurable intervals. Default: every 5 minutes during market hours for stocks. Market hours detection: 9:30 AM - 4:00 PM ET, Monday-Friday, excluding US holidays.
- Use goroutines for parallel processing of watchlist items.
- Acceptance: Running `te run` with AAPL and NVDA in the watchlist logs computed indicator values and news counts for both tickers every 5 minutes during market hours.

### Phase Test Plan

1. `te watchlist add AAPL --strategies=trend_following --timeframes=1h,1d` — triggers backfill.
2. Query `SELECT COUNT(*) FROM candles WHERE ticker='AAPL'` — shows ~500+ rows.
3. `te run` — engine logs indicator values (RSI, MACD, EMA, etc.) every tick.
4. Query `SELECT COUNT(*) FROM news_items WHERE 'AAPL' = ANY(tickers)` — shows news rows.
5. Stop engine, restart — no duplicate candles or news items inserted.
6. Run indicator unit tests — all pass with known inputs.

---

## Phase 3: Analysis Engine

**Goal:** Send enriched market data and news to Claude and receive structured JSON trade recommendations.

**Dependencies:** Phase 2 (market data, indicators, news).

**Definition of Done:** On each tick, after computing indicators, the engine builds a prompt including market data, indicators, portfolio context, and recent news. It sends the prompt to Claude, receives a JSON response with action/confidence/stop_loss/take_profit/reasoning, and logs the full analysis to the database. `te models list` and `te models test` work.

### Tasks

**3.1 — Model provider interface**
- Create `internal/analysis/models.go` with the `ModelProvider` interface from the PRD (section 18).
- Define the `AnalysisResponse` struct matching the expected JSON output (section 8.2).
- Acceptance: Interface compiles. Response struct can be marshaled/unmarshaled.

**3.2 — Claude adapter**
- Create `internal/analysis/anthropic.go` implementing `ModelProvider`.
- Use the Anthropic Messages API directly via `net/http` (or the official Go SDK).
- Send the assembled prompt as a user message with a system prompt.
- Parse the response and extract the JSON object from Claude's output. Handle cases where Claude wraps JSON in markdown code blocks.
- Track token usage and latency.
- Acceptance: Sending a test prompt returns a valid `AnalysisResponse`. Token count and latency are recorded.

**3.3 — Stock analysis prompt template**
- Create `internal/analysis/prompts/stock.go`.
- Build the prompt as defined in PRD section 8.1:
  1. System prompt defining role, output format (strict JSON), strategy rules, and constraints.
  2. Market data section with current and recent OHLCV.
  3. Computed indicators section with all values.
  4. Portfolio context (current cash, open positions, recent trades on this ticker). Use placeholder/empty data for now since portfolio tracking isn't built yet.
  5. Recent news — headlines and sentiment for this ticker from the last 48 hours (from Phase 2 news pipeline).
  6. Knowledge graph context — placeholder section ("No relationship data available yet"), filled in Phase 10.
  7. Similar past analyses — placeholder section ("No historical comparisons available yet"), filled in Phase 11.
- The prompt must instruct the model to return ONLY valid JSON matching the `AnalysisResponse` schema.
- Acceptance: The generated prompt for AAPL includes real indicator data and real news headlines. Claude returns a valid JSON response that parses into `AnalysisResponse` without errors at least 95% of the time.

**3.4 — Analysis storage**
- Create `internal/store/analyses.go` — CRUD for the `analyses` table.
- `InsertAnalysis(analysis)` — stores the full analysis with all fields including `raw_response`, `indicators` JSONB snapshot, and `prompt_hash`.
- `GetAnalysesByTicker(ticker, limit)` — retrieve recent analyses for a ticker.
- `GetAnalysisByID(id)` — retrieve a single analysis with full details.
- Compute `prompt_hash` as SHA-256 of the full prompt text (for caching in backtesting later).
- Acceptance: After the engine runs one analysis cycle, querying the `analyses` table shows a record with all fields populated, including valid JSON in `raw_response` and `indicators`.

**3.5 — Integrate analysis into engine loop**
- After indicator computation, the engine now:
  1. Builds the prompt using the stock template.
  2. Sends it to the configured primary model.
  3. Parses the response.
  4. Stores the analysis in the database.
  5. Logs the action, confidence, and reasoning summary.
- Acceptance: Running `te run` with a watchlist produces analysis log entries every tick. The database populates with analyses. The terminal shows logs like "AAPL: BUY (confidence: 0.78) — Breakout above resistance..."

**3.6 — Multi-model provider registration**
- Create a model registry in `internal/analysis/analyzer.go` that reads the `models` config and instantiates the appropriate provider(s).
- For now, only the Claude adapter is implemented. The registry should accept any `ModelProvider` implementation.
- If consensus is disabled, only the primary model is queried. (Consensus logic is Phase 12.)
- Stub `openai.go` and `ollama.go` exist that return "not implemented" errors.
- Acceptance: Adding a secondary model to the config doesn't break anything. The primary model is always used.

**3.7 — Model management CLI commands**
- Implement `te models list` — shows all configured models with their provider, model name, and status (active/inactive/not implemented).
- Implement `te models test <prompt>` — sends a freeform prompt to the primary model and prints the raw response. Useful for verifying API connectivity.
- Acceptance: `te models list` shows the configured Claude model as active. `te models test "What is 2+2?"` returns a response.

### Phase Test Plan

1. `te models list` — shows Claude as the active primary model.
2. `te models test "Respond with JSON: {\"test\": true}"` — returns a response.
3. `te run` with AAPL in watchlist — engine logs analyses with action/confidence.
4. Query `SELECT action, confidence, reasoning FROM analyses WHERE ticker='AAPL' ORDER BY created_at DESC LIMIT 5` — shows valid analyses.
5. Verify `raw_response` and `indicators` JSONB fields are populated.
6. Verify the prompt includes news headlines (check `raw_prompt` or logs).

---

## Phase 4: Trading Core

**Goal:** Full trade lifecycle: signal generation, risk management, order execution (paper), position tracking, P&L calculation, signal expiry, and data retention.

**Dependencies:** Phase 3 (analysis engine).

**Definition of Done:** The engine generates signals from analyses, validates them against risk rules, executes paper trades on Alpaca, tracks positions with P&L, auto-expires stale signals, and cleans up old data. `te trades list` shows real paper trades. `te status` shows portfolio value. All risk checks are unit-tested.

### Tasks

**4.1 — Signal generation**
- Create signal logic in the engine loop. After an analysis, if the action is BUY or SELL (not HOLD), create a signal record.
- Create `internal/store/signals.go` — CRUD for the `signals` table.
- A signal captures: ticker, action, confidence, strategy, signal price, stop-loss, take-profit, size_pct, analysis_ids.
- Acceptance: Analyses that recommend BUY or SELL produce signal records. HOLD analyses do not create signals.

**4.2 — Risk manager**
- Create `internal/risk/manager.go`.
- Implement every risk check from PRD section 9.1, in order:
  1. Confidence threshold
  2. Risk-reward ratio
  3. Position size limit
  4. Portfolio concentration (sector check — simplified to per-ticker for now)
  5. Max open positions
  6. Daily loss limit
  7. Max drawdown circuit breaker
  8. Cooldown after loss
- Each check returns pass/fail with a reason string.
- If any check fails, the signal is marked as `blocked` with the `block_reason` stored.
- All check results (pass and fail) are logged.
- Acceptance: Unit tests for each risk check. A signal with confidence below threshold is blocked. A signal that would exceed max open positions is blocked. Blocked signals have `block_reason` populated.

**4.3 — Executor interface and local paper trading**
- Create `internal/execution/executor.go` with the `Executor` interface from PRD section 18.
- Create `internal/execution/paper.go` implementing `Executor` for the local paper simulator.
- Paper executor simulates fills at the current market price (fetched from Alpaca quote).
- Records simulated orders and fills in the `orders` table.
- This executor is used by the backtesting engine (Phase 7) and when no broker is configured.
- Acceptance: A paper trade for AAPL creates an order record with `filled_price` close to the current market price.

**4.4 — Alpaca paper broker**
- Create `internal/execution/alpaca_broker.go` implementing `Executor`.
- Connect to Alpaca's paper trading API endpoint (used when `markets.stocks.paper: true`).
- Implement `PlaceOrder` — submits market or limit orders.
- Implement `CancelOrder` — cancels an open order.
- Implement `GetPositions` — fetches current positions from Alpaca.
- Implement `GetOrderStatus` — checks order fill status.
- After entry fill, submit stop-loss and take-profit orders.
- Acceptance: Running in paper mode places real orders on Alpaca's paper trading environment. Orders appear in the Alpaca dashboard.

**4.5 — Trade and order storage**
- Create `internal/store/trades.go` — CRUD for `trades` table.
- Create `internal/store/orders.go` — CRUD for `orders` table (link to broker order IDs).
- When an order fills, create or update the corresponding trade record.
- When an exit order fills (stop-loss or take-profit), update the trade with `exit_price`, `pnl`, `pnl_pct`, `closed_at`, and set status to `closed`.
- Acceptance: A full trade lifecycle (entry → position open → exit) results in a complete trade record with P&L calculated.

**4.6 — Portfolio tracking**
- Create `internal/portfolio/portfolio.go`.
- Track total portfolio value, cash, invested amount, unrealized P&L.
- Take portfolio snapshots on a schedule (every hour, configurable) and store in `portfolio_snapshots`.
- Generate daily summaries and store in `daily_summaries`.
- Acceptance: After a day of paper trading, `portfolio_snapshots` has hourly entries. `daily_summaries` has one entry for the day with accurate P&L, trade counts, and win/loss counts.

**4.7 — Position monitoring**
- Add position monitoring to the engine loop. On each tick, for each open position:
  1. Fetch current price.
  2. Check if stop-loss or take-profit has been breached (in case broker orders didn't trigger).
  3. Check if the strategy generates an exit signal.
  4. If exit conditions are met, place a sell order.
- Acceptance: A position that hits its stop-loss price is automatically closed. The trade record shows the loss.

**4.8 — Signal expiry and staleness**
- Implement a background goroutine in the engine that checks pending signals on each tick (PRD section 10.1.1):
  - Time-based: signals older than the configured expiry window (default 30 min) are marked `expired`.
  - Price-based: signals where the current price has moved more than 2% from the signal price are marked `stale`.
- Both TUI (Phase 5) and Telegram (Phase 6) will read these statuses — the engine is the single source of truth for expiry.
- Acceptance: A pending signal that sits for 31 minutes is auto-expired. A pending signal where price moves 3% is auto-cancelled as stale. Both are visible in `te trades pending`.

**4.9 — Mode router**
- Implement the mode routing logic in the engine loop (PRD section 10.1):
  - `auto` — signals that pass risk checks are executed immediately.
  - `paper` — signals are executed via Alpaca paper API (default) or local simulator (if no broker configured).
  - `manual` — signals are queued as `pending`. (Approval flow is TUI Phase 5 and Telegram Phase 6.)
- Implement `te mode <manual|auto|paper>` — switches mode at runtime (writes to DB overrides) and persists across restarts.
- Acceptance: Switching between modes changes behavior. In auto/paper mode, trades execute. In manual mode, trades queue as pending.

**4.10 — Trade CLI commands**
- Implement `te trades list` — shows recent trades in a table (ticker, direction, entry, exit, P&L, status, date).
- Implement `te trades show <trade_id>` — full trade details including the analysis reasoning that triggered it.
- Implement `te trades pending` — shows signals awaiting approval (with expiry countdown).
- Implement `te trades approve <trade_id>` and `te trades reject <trade_id>` — approve/reject pending signals from the CLI.
- Update `te status` to include: mode, portfolio value, cash, open positions count, today's P&L.
- Acceptance: After several paper trades, `te trades list` shows them with accurate data. `te status` shows a correct portfolio summary.

**4.11 — Data retention cleanup**
- Create `internal/store/retention.go` with cleanup functions:
  - `CleanupIntradayCandles()` — deletes 1m/5m/15m candles older than 90 days.
  - `CleanupPromptCache()` — deletes prompt cache entries older than 6 months.
- Schedule cleanup as a daily background goroutine in the engine's scheduler.
- Acceptance: After running cleanup, old intraday candles are removed. Daily and hourly candles are preserved. Cleanup runs automatically once per day.

### Phase Test Plan

1. `te run --mode=paper` with a watchlist — engine generates signals and executes paper trades.
2. `te trades list` — shows executed trades with entry prices.
3. `te trades pending` — in manual mode, shows queued signals.
4. Wait 31 minutes — pending signal auto-expires and shows as `expired`.
5. `te trades approve <id>` — approves a pending trade (execute from CLI).
6. `te status` — shows portfolio value, cash, open positions.
7. Verify risk checks: set `max_open_positions: 1`, open one position, verify next signal is blocked.
8. Run all risk manager unit tests — pass.
9. Verify position monitoring: set a tight stop-loss, watch it trigger.
10. `te mode auto` then `te mode manual` — verify mode switching works and persists.

---

## Phase 5: TUI Dashboard

**Goal:** A real-time terminal dashboard for monitoring and interacting with the engine, powered by an internal event bus.

**Dependencies:** Phase 4 (trading core, signals, positions, portfolio).

**Definition of Done:** `te dashboard` launches a full-screen TUI showing live positions, signal feed, pending approvals, risk status, and keyboard-driven navigation. Data updates in real time via the event bus (not just DB polling). Config toggles in the TUI persist to the DB override layer.

### Tasks

**5.1 — Event bus**
- Create `internal/events/bus.go` — a channel-based publish-subscribe event bus.
- Create `internal/events/types.go` — define event types: `DataFetched`, `IndicatorsComputed`, `AnalysisCompleted`, `SignalGenerated`, `SignalBlocked`, `SignalExpired`, `RiskCheckCompleted`, `TradeExecuted`, `TradeSimulated`, `SignalQueued`, `PositionUpdated`, `PortfolioSnapshotTaken`, `ErrorOccurred`.
- Each event carries a typed payload with relevant data (ticker, prices, action, etc.).
- Subscribers register via `bus.Subscribe(eventType, channel)`. The bus fans out events to all subscribers for that type.
- The bus is created in the engine and passed to all components that emit or consume events.
- Wire the engine loop to emit events at each pipeline stage (matching the PRD section 4.4 data flow diagram).
- Acceptance: Running the engine with a test subscriber logs all event types as they fire. Events flow without blocking the main loop (buffered channels).

**5.2 — Bubble Tea app scaffold**
- Create `internal/tui/app.go` — the main Bubble Tea program.
- Set up the application model, update loop, and view rendering.
- Subscribe to the event bus for real-time updates (supplement with DB polling at 3-second intervals as fallback).
- Implement a tab/view switching system: pressing keys switches between views (dashboard, trades, watchlist, logs, config).
- Add the bottom status bar with keyboard shortcuts.
- Acceptance: `te dashboard` launches a full-screen terminal app. Pressing `q` quits cleanly. Pressing view keys switches between placeholder views.

**5.3 — Main dashboard view**
- Create `internal/tui/views/dashboard.go`.
- Implement all sections from the PRD section 7.1:
  - Header bar with current mode and paper status.
  - Portfolio summary (total value, cash, invested, today's P&L).
  - Open positions table (ticker, entry, current, P&L, stop, target, strategy).
  - Pending approvals section (in manual mode).
  - Signal feed (scrollable, showing recent signals with status).
  - Risk status bar (daily loss, drawdown, position count, largest position).
- Data updates via event bus with DB polling fallback.
- Acceptance: With the engine running in another terminal, `te dashboard` shows live updating positions and signals.

**5.4 — Position detail view**
- Create `internal/tui/views/positions.go`.
- Expanded view of a single open position with: full analysis reasoning that triggered the entry, all indicator values at time of entry, related signals, current P&L, stop-loss/take-profit levels, strategy name.
- Accessible by pressing Enter on a position in the main dashboard's positions table.
- Acceptance: Selecting a position in the dashboard opens a detail view with complete context. Pressing Escape returns to the dashboard.

**5.5 — Trade approval in TUI**
- In the pending approvals section, implement:
  - `a` key to approve a pending trade → updates signal status to `approved`, triggers execution.
  - `r` key to reject → updates signal status to `rejected`.
  - `d` key to expand full details (model reasoning, all indicator values, risk check results).
- Arrow keys or `j`/`k` to navigate between pending trades.
- Display expiry countdown for each pending signal.
- Acceptance: In manual mode, signals appear as pending. Pressing `a` executes the trade. Pressing `r` cancels it. Expired signals show as expired.

**5.6 — Trade history view**
- Create `internal/tui/views/trades.go`.
- Scrollable table of past trades.
- Press Enter on a trade to see full details (entry/exit, P&L, analysis reasoning).
- Acceptance: Navigation works. Trade details are correct and match database records.

**5.7 — Signal feed view**
- Create `internal/tui/views/signals.go`.
- Expanded view of the signal feed with more detail per signal.
- Shows blocked signals with their block reasons.
- Acceptance: All signals (passed, blocked, executed, expired, stale) appear with correct statuses.

**5.8 — Watchlist management view**
- Create `internal/tui/views/watchlist.go`.
- Shows all watchlist items with strategies and timeframes.
- Ability to toggle items active/inactive.
- Acceptance: Toggling a watchlist item inactive stops the engine from analyzing it on the next tick.

**5.9 — Config toggle view**
- Create `internal/tui/views/config.go`.
- Toggle mode (manual/auto/paper).
- Toggle consensus on/off.
- Adjust risk parameters (with confirmation).
- Changes write to the `config_overrides` DB table (Phase 1 task 1.3) and take effect immediately.
- Acceptance: Switching mode in the TUI is reflected in `te status` and changes engine behavior. Changes persist across engine restarts.

**5.10 — Reusable TUI components**
- Create `internal/tui/components/table.go` — reusable table with column alignment, scrolling, and row selection.
- Create `internal/tui/components/sparkline.go` — ASCII sparkline for inline mini-charts (e.g., last 20 price points).
- Create `internal/tui/components/statusbar.go` — bottom bar with keyboard shortcuts.
- Acceptance: Components are used across views. Tables scroll correctly with large datasets.

### Phase Test Plan

1. Start engine in one terminal, `te dashboard` in another.
2. Verify dashboard shows live positions, signals, and portfolio value.
3. Select a position in the dashboard — verify drill-down shows full analysis reasoning and indicators.
4. Set engine to manual mode via TUI config view — verify `te status` shows manual.
5. New signal appears in pending approvals — press `a` to approve, verify trade executes.
6. New signal appears — wait for expiry, verify it shows as expired in TUI.
7. Press `r` to reject a signal — verify it disappears from pending.
8. Navigate to trade history — verify completed trades with P&L.
9. Navigate to watchlist — toggle a ticker inactive, verify engine stops analyzing it.
10. Adjust a risk parameter in TUI — restart engine, verify the change persists.

---

## Phase 6: Notifications

**Goal:** Telegram bot for trade alerts, manual approvals on the go, and system status notifications.

**Dependencies:** Phase 5 (event bus — Telegram subscribes to engine events).

**Definition of Done:** The engine sends trade notifications to Telegram. In manual mode, you can approve or reject trades from Telegram inline buttons. Daily summaries arrive automatically. Error and drawdown warnings are delivered.

### Tasks

**6.1 — Telegram bot setup**
- Create `internal/notify/telegram.go`.
- Initialize the bot using the Telegram Bot API (use a lightweight Go library like `go-telegram-bot-api` or raw HTTP).
- The bot runs as a goroutine within the main engine binary.
- Subscribe to relevant event bus events: `SignalQueued`, `TradeExecuted`, `ErrorOccurred`, `PositionUpdated`.
- On startup, send a "TradeEngine online" message to confirm connectivity.
- Acceptance: Starting the engine sends a startup message to the configured Telegram chat.

**6.2 — Trade alert notifications**
- Send a notification when a trade is executed (entry or exit).
- Format: ticker, action, price, strategy, P&L (for exits).
- Triggered by `TradeExecuted` event.
- Acceptance: Paper trades trigger Telegram messages with correct details.

**6.3 — Signal notifications**
- Send a notification when a new signal is generated (after passing risk checks).
- Format: action, ticker, price, confidence, stop-loss, take-profit, condensed reasoning.
- Triggered by `SignalQueued` or `SignalGenerated` events.
- Acceptance: Signals produce Telegram notifications.

**6.4 — Manual approval via Telegram**
- In manual mode, pending trade notifications include inline keyboard buttons: Approve, Reject, Full Analysis.
- Approve callback → update signal to `approved`, trigger execution, reply with confirmation.
- Reject callback → update signal to `rejected`, reply with confirmation.
- Full Analysis callback → reply with expanded reasoning from all models.
- Handle signal expiry: if the signal has expired or is stale by the time the user presses Approve, reply with "Signal expired" or "Signal stale — price moved."
- Acceptance: Pending trades can be approved and rejected from Telegram. Approved trades execute. Expired/stale signals are handled gracefully.

**6.5 — Daily summary**
- At market close (or at a configured time for 24/7 mode), send a daily summary:
  - Portfolio value and daily change.
  - Trades opened and closed today.
  - Wins/losses.
  - Signals generated and blocked.
- Acceptance: A daily summary arrives at the configured time with accurate data.

**6.6 — Error and warning notifications**
- Send alerts for: API failures, data fetch errors, drawdown warnings (approaching limit), circuit breaker activation.
- Triggered by `ErrorOccurred` events and drawdown threshold checks.
- Acceptance: Simulating a drawdown breach triggers a Telegram warning.

**6.7 — Configurable notification types**
- Respect the `notifications.telegram.notify_on` config array. Only send notification types the user has enabled.
- Acceptance: Removing `signal` from the array stops signal notifications while trade notifications continue.

### Phase Test Plan

1. Start engine — verify "TradeEngine online" message arrives on Telegram.
2. Run in paper mode — verify trade execution messages arrive.
3. Switch to manual mode — verify pending signal notification with inline buttons arrives.
4. Press Approve in Telegram — verify trade executes and confirmation message arrives.
5. Let a signal expire — press Approve after expiry — verify "Signal expired" response.
6. Press Full Analysis — verify expanded reasoning is sent.
7. Wait for market close — verify daily summary message arrives.
8. Simulate API error — verify error notification arrives.
9. Remove `signal` from `notify_on` — verify signal notifications stop but trade notifications continue.

---

## Phase 7: Backtesting

**Goal:** Replay historical data through the analysis pipeline and measure strategy performance.

**Dependencies:** Phase 3 (analysis engine), Phase 4 (risk manager, local paper executor, trade storage).

**Definition of Done:** `te backtest run` replays a date range through the full pipeline, simulates trades, computes all performance metrics, and stores results. `te backtest compare` shows side-by-side metrics. Prompt responses are cached. Re-running the same backtest is instant (all cached).

### Tasks

**7.1 — Historical data loader**
- Create `internal/backtest/runner.go`.
- On backtest start, check the `candles` table for the requested ticker, timeframe, and date range.
- If data is missing, fetch from Alpaca and store (backfill).
- Load all candles into memory for the replay loop.
- Acceptance: Running a backtest for AAPL 2023-01 to 2024-12 loads ~500 daily candles, fetching from Alpaca if not already cached.

**7.2 — Prompt cache**
- Create `internal/backtest/cache.go`.
- Before each model query during backtesting, compute the prompt hash (SHA-256).
- Check `prompt_cache` for a matching (hash, provider, model) entry.
- If found, return the cached response. If not, call the API and cache the response.
- Support `--no-cache` flag to bypass.
- Track cache hit rate during the run.
- Acceptance: Running the same backtest twice: first run makes N API calls, second run makes 0 (all cached). Cache hit rate is reported.

**7.3 — Replay engine**
- Implement the replay loop from PRD section 11.1:
  1. Iterate candles chronologically.
  2. At each candle, compute indicators using only data up to that point (sliding window, no look-ahead bias).
  3. Build prompt.
  4. Query model (via cache).
  5. If BUY/SELL: simulate entry at the **next candle's open** (prevents look-ahead bias).
  6. For open positions: check if stop-loss or take-profit was hit within the candle's high/low range.
  7. Track virtual portfolio (cash, positions, P&L).
- Uses the local paper executor (Phase 4 task 4.3), not the Alpaca paper API.
- Handle edge cases: position already open when a new BUY signal comes, multiple signals on the same candle.
- Acceptance: A backtest produces a sequence of simulated trades with realistic entry/exit prices. No look-ahead bias (entry prices use next candle's open).

**7.4 — Metrics calculation**
- Create `internal/backtest/metrics.go`.
- Compute all metrics from PRD section 11.3:
  - Total return (%)
  - Annualized return (%)
  - Sharpe ratio (risk-free rate of 4% or configurable)
  - Sortino ratio
  - Calmar ratio
  - Max drawdown (%)
  - Win rate (%)
  - Profit factor
  - Expectancy ($ per trade)
  - Total trade count
  - Average hold time
  - Best and worst trade (%)
- Acceptance: Unit tests with known trade sequences produce correct metric values. Edge cases: zero trades, all wins, all losses, single trade.

**7.5 — Backtest storage**
- Create `internal/store/backtests.go` — CRUD for `backtest_runs` and `backtest_trades`.
- Assign sequential labels (BT-00001, BT-00002, etc.).
- Store the full config snapshot at the time of the run.
- Acceptance: Completed backtests appear in the database with all metrics and simulated trades.

**7.6 — Backtest CLI commands**
- Implement `te backtest run --strategy=momentum --ticker=AAPL --model=claude-sonnet --from=2023-01-01 --to=2024-12-31`.
- Print a progress bar during execution (candle X of Y).
- Print the summary table (all metrics) on completion.
- Implement `te backtest list` — shows all past runs with key metrics.
- Implement `te backtest compare --id=BT-00001 --id=BT-00002` — side-by-side metric comparison in the terminal.
- Acceptance: A full backtest completes, prints metrics, and stores results. Comparison shows two runs side-by-side.

### Phase Test Plan

1. `te backtest run --strategy=trend_following --ticker=AAPL --from=2024-01-01 --to=2024-06-30` — runs to completion.
2. Verify progress bar updates during execution.
3. Verify summary metrics are printed (return, Sharpe, drawdown, win rate, etc.).
4. `te backtest list` — shows the run with its label and key metrics.
5. Run the same backtest again — verify 100% cache hit rate (zero API calls).
6. Run with `--no-cache` — verify fresh API calls are made.
7. `te backtest run` with a different strategy — run completes, different metrics.
8. `te backtest compare --id=BT-00001 --id=BT-00002` — shows side-by-side comparison.
9. Run metrics unit tests — all pass (including edge cases).

---

## Phase 8: Reports

**Goal:** Generate static HTML files with interactive charts for performance analysis and backtest review.

**Dependencies:** Phase 4 (trade and portfolio data), Phase 7 (backtest data).

**Definition of Done:** `te report performance` opens a browser with equity curves, drawdown charts, and performance heatmaps. `te report backtest <id>` shows a backtest's results visually. All reports are self-contained HTML files.

### Tasks

**8.1 — Report generator framework**
- Create `internal/reports/generator.go`.
- Set up go-echarts for chart generation.
- Create a base HTML template that wraps go-echarts output with consistent styling (dark theme, clean typography).
- Reports are saved to the configured `reports.output_dir` (default `~/.tradeengine/reports/`).
- If `reports.auto_open_browser` is true, open the file in the default browser after generation.
- Acceptance: A placeholder report generates and opens in the browser as a styled HTML page.

**8.2 — Chart components**
- Create `internal/reports/charts.go` with reusable chart builders:
  - **Equity curve** — line chart of portfolio value over time (interactive, zoomable).
  - **Drawdown chart** — area chart showing drawdown from peak.
  - **Monthly returns heatmap** — months as columns, years as rows, colored by return.
  - **Trade scatter plot** — each trade as a dot, x = date, y = P&L %, color = win/loss.
  - **Metrics summary table** — HTML table with all key metrics.
- Acceptance: Each chart function produces valid go-echarts output that renders correctly in a browser.

**8.3 — Performance report**
- Implement `te report performance [--from=<date>] [--to=<date>]`.
- Content: equity curve, drawdown chart, monthly heatmap, metrics summary, breakdowns by strategy and by ticker, top 5 best/worst trades with reasoning excerpts.
- Acceptance: Report generates with real data from paper trading. All charts are interactive.

**8.4 — Daily and weekly reports**
- Implement `te report daily` and `te report weekly`.
- Daily: today's trades, signals, portfolio change, model cost (tokens used).
- Weekly: week's equity curve, win/loss breakdown, strategy comparison.
- Acceptance: Reports generate with correct data for the respective time periods.

**8.5 — Backtest report**
- Implement `te report backtest <id>`.
- Content: equity curve, drawdown chart, all metrics, per-trade scatter.
- Acceptance: Report accurately reflects the backtest run's data.

**8.6 — Backtest comparison report**
- Implement `te report backtest compare <id1> <id2>`.
- Content: overlaid equity curves, side-by-side metrics table, trade distribution comparison.
- Acceptance: Two backtests are visually compared in a single HTML report.

### Phase Test Plan

1. `te report performance` — generates HTML, opens in browser with equity curve and drawdown chart.
2. `te report daily` — generates daily summary with today's trades and signals.
3. `te report weekly` — generates weekly summary with equity curve.
4. `te report backtest BT-00001` — generates backtest report matching stored metrics.
5. `te report backtest compare BT-00001 BT-00002` — generates comparison with overlaid curves.
6. Verify all charts are interactive (zoom, hover tooltips).
7. Verify reports are self-contained (open HTML file with no internet — still renders).

---

## Phase 9: Prediction Markets

**Goal:** Fetch prediction market data, analyze contracts with LLM prompts tailored for probability estimation, and execute trades on Polymarket and Kalshi.

**Dependencies:** Phase 4 (execution framework, risk manager), Phase 3 (analysis engine).

**Definition of Done:** The engine monitors prediction market contracts alongside stocks. Claude estimates probabilities and identifies mispriced contracts. Trades execute on prediction market platforms. The watchlist supports category-based auto-discovery of contracts.

### Tasks

**9.1 — Prediction data provider interface**
- Create the `PredictionDataProvider` interface in `internal/data/provider.go` (from PRD section 18).
- Define `Contract` and `ContractSnapshot` types.
- Acceptance: Interface compiles with methods for fetching contracts and snapshots.

**9.2 — Polymarket adapter**
- Create `internal/data/polymarket.go` implementing `PredictionDataProvider`.
- Fetch available contracts by category and minimum volume.
- Fetch contract snapshots (price, volume, bid/ask, time to resolution).
- Acceptance: Fetching contracts from the "politics" category returns real data with prices and volumes.

**9.3 — Kalshi adapter**
- Create `internal/data/kalshi.go` implementing `PredictionDataProvider`.
- Same interface as Polymarket.
- Acceptance: Fetching contracts from Kalshi returns real data.

**9.4 — Contract auto-discovery**
- Implement the auto-discovery mechanism for prediction market watchlist items.
- The PRD config specifies categories, `min_volume`, and `max_contracts` per platform.
- On each scheduler tick (or a slower cadence, e.g., every hour):
  1. Query each platform for contracts matching the configured categories.
  2. Filter by `min_volume`.
  3. Rank by volume and select up to `max_contracts`.
  4. Add newly discovered contracts to the active monitoring set.
  5. Remove contracts that no longer meet criteria or have resolved.
- Store the active contract set in the `watchlist` table with `source = 'auto_discovered'`.
- Acceptance: Configuring `categories: [politics, crypto]` and `max_contracts: 20` results in up to 20 contracts being monitored, automatically refreshed.

**9.5 — Prediction snapshot storage**
- Create `internal/store/predictions.go` — CRUD for `prediction_snapshots`.
- Store snapshots on each polling interval (default every 15 minutes, 24/7).
- Acceptance: After running for an hour, the `prediction_snapshots` table has ~4 snapshots per monitored contract.

**9.6 — Prediction indicators**
- Create `internal/indicators/prediction.go`.
- Compute from stored snapshots:
  - Price trend (direction and velocity over last N snapshots).
  - Volume trend.
  - Spread trend.
  - Time decay (days until resolution, rate of probability change as deadline approaches).
- Acceptance: Given a series of snapshots, indicators produce meaningful values.

**9.7 — Prediction analysis prompt**
- Create `internal/analysis/prompts/prediction.go`.
- Different structure from stock prompts (PRD section 8.3):
  - Current contract price (implied probability), volume, bid/ask, time to resolution.
  - Historical price snapshots (probability over time).
  - Related contracts and their prices.
  - Recent news relevant to the event.
  - Ask Claude to estimate the true probability and identify mispricing.
- Expected output includes `estimated_probability`, `market_probability`, and `edge`.
- Acceptance: Claude returns structured JSON with probability estimates. The `edge` field (estimated - market) is calculated.

**9.8 — Prediction market execution**
- Create `internal/execution/polymarket_exec.go` and `internal/execution/kalshi_exec.go` implementing `Executor`.
- Place limit orders at desired prices.
- Monitor fill status.
- Handle contract resolution (automatic P&L calculation when the event resolves).
- Acceptance: A paper-mode prediction trade is recorded. When the contract resolves, the trade closes with correct P&L.

**9.9 — Integrate predictions into engine loop**
- The scheduler runs prediction market analysis on a separate cadence (every 15 minutes, 24/7 — not tied to stock market hours).
- Watchlist supports both manual contract entries and auto-discovered contracts.
- Emit events for prediction market activity (same event types as stocks).
- Acceptance: The engine monitors both stocks (during market hours) and prediction markets (24/7). The TUI and Telegram show prediction market signals and trades.

### Phase Test Plan

1. Configure prediction market watchlist with `categories: [politics]` and `min_volume: 50000`.
2. `te run` — engine discovers and monitors contracts meeting criteria.
3. Query `prediction_snapshots` — shows snapshot data for monitored contracts.
4. Verify prediction indicators compute from stored snapshots.
5. Verify Claude returns probability estimates with `edge` field.
6. Execute a paper prediction trade — verify it's recorded with correct structure.
7. Verify the TUI dashboard shows prediction market signals alongside stock signals.
8. Verify prediction market analysis runs 24/7, independent of stock market hours.

---

## Phase 10: Knowledge Graph

**Goal:** Maintain entity relationships in PostgreSQL and inject them as context into analysis prompts.

**Dependencies:** Phase 3 (analysis prompts), Phase 2 (news pipeline).

**Definition of Done:** Entities and relationships are stored in the database. Seed data is loaded. The analysis prompt includes related entity context. Claude can auto-extract relationships from news. `te graph` commands work.

### Tasks

**10.1 — Entity and relationship CRUD**
- Create `internal/knowledge/graph.go`.
- Create `internal/store/entities.go` — CRUD for `entities` table.
- Create `internal/store/relationships.go` — CRUD for `relationships` table. Also includes CRUD for the `entity_mentions` table (tracking which entities appear in which news items — used in context building to find recent news affecting related entities).
- Support querying relationships by entity (1-hop and 2-hop).
- Acceptance: Can create entities (NVDA as a company), create relationships (NVDA supplies_to MSFT), record entity mentions in news, and query all relationships for NVDA.

**10.2 — Seed initial data**
- Implement `te graph seed` — populates common entities from a bundled JSON/YAML data file:
  - Major companies (FAANG+, major tech, finance, etc.) with tickers and sector assignments.
  - Sector entities (Technology, Healthcare, Finance, Energy, etc.).
  - Key supply chain relationships for commonly traded stocks.
- Idempotent — running `te graph seed` multiple times doesn't create duplicates.
- Acceptance: After seeding, `te graph entities` lists 50+ entities. `te graph relationships NVDA` shows supply chain and competitive relationships.

**10.3 — Context builder**
- Create `internal/knowledge/context.go`.
- Given a ticker, build a structured text block for prompt injection:
  1. Look up the entity.
  2. Query relationships (1-2 hops).
  3. Check for any open positions in related tickers.
  4. Format as structured context.
- Acceptance: Calling `BuildContext("NVDA", portfolio)` returns a formatted string listing NVDA's relationships, sector, and any portfolio overlap.

**10.4 — Inject context into analysis prompts**
- Modify the stock and prediction prompt builders to replace the placeholder knowledge graph section with real context from the context builder.
- Acceptance: Analysis prompts now include a "Related Context" section. Claude's reasoning references related entities when relevant.

**10.5 — Auto-enrichment from news**
- Create `internal/knowledge/enricher.go`.
- When processing news articles, add an extraction prompt: "Extract any company relationships mentioned in this news (supply chain, partnerships, competition, regulatory)."
- Parse extracted relationships and insert into `entities` and `relationships` with `auto_generated = true` and a confidence score.
- Acceptance: Processing a news article about "TSMC winning a contract from NVIDIA" creates or confirms a `manufactures_for` relationship between TSMC and NVDA entities.

**10.6 — Graph CLI commands**
- Implement `te graph seed` — loads bundled seed data.
- Implement `te graph review` — shows auto-generated entities and relationships awaiting review. User can confirm or reject.
- Implement `te graph entities` — lists all entities.
- Implement `te graph relationships <ticker>` — shows relationships for a ticker.
- Acceptance: Auto-generated relationships appear in review. Confirming persists them. Rejecting removes them.

### Phase Test Plan

1. `te graph seed` — loads 50+ entities and relationships.
2. `te graph entities` — lists seeded entities.
3. `te graph relationships NVDA` — shows NVDA's supply chain and competitive relationships.
4. Run `te graph seed` again — verify no duplicates.
5. `te run` with knowledge graph enabled — verify analysis prompts include "Related Context" section.
6. Verify Claude's reasoning references related entities (check `raw_response` in analyses table).
7. Process a news article that mentions a new relationship — verify auto-generated entry appears.
8. `te graph review` — confirm the auto-generated relationship, verify confidence updates.

---

## Phase 11: Embeddings

**Goal:** Generate pgvector embeddings for analyses, news, and entities. Enable semantic search for "find similar setups."

**Dependencies:** Phase 3 (analyses), Phase 2 (news), Phase 10 (entities).

**Definition of Done:** Past analyses with similar market conditions can be found via vector similarity search. The analysis prompt optionally includes past similar analyses and their outcomes. An embedding dimension is chosen and locked.

### Tasks

**11.1 — Embedding dimension decision**
- Choose the embedding model and lock the vector dimension before any embedding data is stored:
  - OpenAI `text-embedding-3-small` (1536 dimensions) — recommended for quality.
  - Ollama `nomic-embed-text` (768 dimensions) — recommended for cost-free local operation.
- Set the dimension in the relevant migration (vector column size).
- Document the choice and the re-indexing procedure (`te embeddings reindex`) in case of future model change.
- Acceptance: Vector columns have a fixed dimension. The chosen model is documented in config.

**11.2 — Embedder interface and providers**
- Create `internal/embeddings/embedder.go` with the `Embedder` interface from PRD section 18.
- Create `internal/embeddings/openai.go` — uses OpenAI's text-embedding-3-small API.
- Create `internal/embeddings/ollama.go` — uses local Ollama with nomic-embed-text.
- Provider selection based on config.
- Acceptance: Both providers generate embeddings of the correct dimension for a test string.

**11.3 — Async embedding worker**
- Create `internal/embeddings/worker.go`.
- Runs as a background goroutine.
- Watches for new analyses, news items, and entities without embeddings (via a `WHERE embedding IS NULL` query on a polling interval).
- Generates embeddings and updates the rows.
- Rate-limited to avoid API cost spikes.
- Acceptance: After an analysis is stored, its embedding field is populated within a few seconds. The embedding worker does not block the main trading loop.

**11.4 — Semantic search queries**
- Add to `internal/store/analyses.go`:
  - `FindSimilarAnalyses(embedding, threshold, limit)` — returns past analyses ranked by cosine similarity with their trade outcomes.
- Add to `internal/store/news.go`:
  - `FindSimilarNews(embedding, threshold, limit)` — for deduplication and relevance search.
- Add to `internal/store/entities.go`:
  - `FindSimilarEntities(embedding, threshold, limit)` — semantic entity search.
- Acceptance: Querying similar analyses for a current NVDA setup returns past NVDA analyses and potentially other semiconductor stocks with similar indicator profiles.

**11.5 — Inject similar analyses into prompts**
- Modify the prompt builder to replace the placeholder "Similar Past Analyses" section with real results from semantic search.
- Format: "A similar setup occurred on [date] for [ticker]. The recommendation was [action] at confidence [X]. The trade resulted in [+Y% / -Z%]."
- Gated by a config flag to enable/disable.
- Acceptance: When enabled, the analysis prompt includes a "Similar Past Setups" section. Claude's reasoning can reference these.

**11.6 — Re-indexing command**
- Implement `te embeddings reindex` — re-generates all embeddings using the currently configured model. Used when switching embedding providers.
- Shows progress (X of Y rows re-embedded).
- Acceptance: After switching from OpenAI to Ollama in config and running `te embeddings reindex`, all embeddings are regenerated with the new model's dimensions.

### Phase Test Plan

1. Verify vector column dimensions match the chosen embedding model.
2. Run the engine — analyses are stored, embeddings appear within seconds.
3. Query `SELECT COUNT(*) FROM analyses WHERE embedding IS NOT NULL` — count increases over time.
4. `FindSimilarAnalyses` for a current NVDA analysis — returns relevant past analyses.
5. Verify the analysis prompt includes "Similar Past Setups" when enabled.
6. Disable the feature in config — verify prompts no longer include the section.
7. `te embeddings reindex` — all rows re-embedded (check progress output).

---

## Phase 12: Multi-Model Consensus

**Goal:** Query multiple LLMs in parallel and resolve their recommendations using configurable consensus strategies.

**Dependencies:** Phase 3 (model provider interface, analysis pipeline).

**Definition of Done:** The engine queries Claude, GPT-4o, and/or a local model simultaneously. Their recommendations are compared and resolved via majority, unanimous, or primary-with-veto. `te models list`, `te models test`, and `te models compare` all work. Model performance is tracked over time.

### Tasks

**12.1 — OpenAI adapter**
- Create `internal/analysis/openai.go` implementing `ModelProvider`.
- Use the OpenAI Chat Completions API.
- Same prompt format, same expected JSON output.
- Acceptance: Sending the same prompt to GPT-4o returns a valid `AnalysisResponse`.

**12.2 — Ollama adapter**
- Create `internal/analysis/ollama.go` implementing `ModelProvider`.
- Connect to a local Ollama instance.
- Acceptance: Sending a prompt to a local Llama model returns a valid `AnalysisResponse`.

**12.3 — Parallel model querying**
- Modify the analyzer to send prompts to all configured models in parallel (goroutines).
- Collect all responses with timeouts.
- If a secondary model fails or times out, proceed with available responses.
- Acceptance: With two models configured, both are queried simultaneously. Total latency ≈ max of the two, not the sum.

**12.4 — Consensus resolver**
- Create `internal/analysis/consensus.go`.
- Implement the three strategies from PRD section 5.3:
  - **majority** — Act if >50% of models agree on the action. Confidence = average of agreeing models.
  - **unanimous** — Only act if all models agree on the same action.
  - **primary-with-veto** — Primary model's recommendation is used, unless any secondary model strongly disagrees (opposite action with confidence > 0.7). In that case, the signal is blocked and flagged.
- Store the consensus method and all individual model responses in the signal record.
- Acceptance: Unit tests for each strategy. Majority correctly resolves 2-of-3 agreement. Unanimous blocks on disagreement. Veto triggers when secondary strongly opposes primary.

**12.5 — Model management CLI commands**
- Update `te models list` — now shows all configured models with status, provider, and recent accuracy (if available).
- Update `te models test <prompt>` — sends to all configured models and shows each response.
- Implement `te models compare <ticker>` — fetches current data, sends to all configured models, prints side-by-side results: action, confidence, stop/target, and condensed reasoning for each.
- Acceptance: Running `te models compare AAPL` produces a formatted comparison table in the terminal.

**12.6 — Model performance tracking**
- Track per-model accuracy over time using the `model_performance` view.
- Compare model recommendations against actual trade outcomes.
- Add model-specific breakdowns to performance reports (Phase 8).
- Acceptance: After sufficient trades, `te report performance` includes a section showing which model was most accurate.

### Phase Test Plan

1. Configure Claude as primary, GPT-4o as secondary, consensus enabled.
2. `te models list` — shows both models as active.
3. `te models test "What is 2+2?"` — both models respond.
4. `te models compare AAPL` — side-by-side comparison table.
5. `te run` with consensus mode `majority` — verify both models are queried in parallel.
6. Verify consensus resolution: when models agree, trade executes; when they disagree, signal is blocked.
7. Test `primary-with-veto`: secondary strongly disagrees → signal blocked and flagged.
8. Run all consensus resolver unit tests — pass.
9. After trades accumulate, verify model performance tracking shows per-model accuracy.

---

## Phase 13: Go Live

**Goal:** Transition from paper trading to real money. This phase is ongoing and iterative.

**Dependencies:** All prior phases (1-12). The system should be stable and well-tested before live deployment.

**Definition of Done:** Never "done" — this is continuous monitoring and improvement.

### Tasks

**13.1 — Pre-flight checklist**
- Run paper trading for at least 4-8 weeks with the final strategy configuration.
- Review paper trading metrics: positive expectancy, acceptable drawdown, consistent Sharpe ratio.
- Verify that all notification channels work reliably.
- Verify that all risk checks activate correctly (test each one deliberately).
- Review a sample of Claude's analyses to confirm reasoning quality.
- Document the exact configuration that will be used for live trading.

**13.2 — Switch to live**
- Set `markets.stocks.paper: false` in config.
- Start with **manual mode** for the first 2 weeks minimum. Approve every trade personally.
- Use the smallest possible position sizes (reduce `max_position_pct` and `risk.default_stop_loss_pct`).
- Acceptance: First real trade executes successfully. P&L is tracked correctly.

**13.3 — Conservative ramp-up**
- Week 1-2: Manual mode, 1% max position size.
- Week 3-4: Manual mode, 2% max position size. Review performance.
- Week 5-8: Switch to auto mode for high-confidence signals only (raise confidence threshold to 0.85+). Keep manual for lower confidence.
- Week 9+: Gradually lower confidence threshold and increase position sizes based on track record.

**13.4 — Ongoing monitoring**
- Daily: Review daily Telegram summary. Check for anomalies.
- Weekly: Generate weekly report. Review strategy-by-strategy performance.
- Monthly: Full performance review. Compare live results to paper trading and backtest results. Tune strategy parameters if needed. Review and prune the knowledge graph.
- Continuously: Monitor max drawdown. If the circuit breaker triggers, stop and investigate before resuming.

**13.5 — Iteration**
- Add new tickers to the watchlist based on opportunities.
- Test new strategies via backtesting before deploying live.
- Compare model performance and swap the primary model if a secondary consistently outperforms.
- Refine prompts based on observed reasoning quality.
- Expand prediction market coverage as confidence grows.

### Phase Test Plan

This phase is not a one-time test — it's an ongoing operational checklist:
1. Paper trading metrics review complete (positive expectancy confirmed).
2. All risk checks deliberately tested and verified.
3. First live trade executes in manual mode with 1% position size.
4. Daily Telegram summaries arrive for 7 consecutive days.
5. Weekly report generated and reviewed.
6. No unexpected drawdowns or missed stop-losses in first 2 weeks.

---

## Phase 14: Web Data Flow Visualization

**Goal:** A real-time web-based observability interface that visualizes data flowing through the TradeEngine pipeline — from source ingestion through processing to trade execution.

**Dependencies:** Phase 5 (event bus), Phase 13 (stable running engine with real activity to visualize).

**Definition of Done:** `te web` starts a local web server. Opening `http://localhost:8080` in a browser shows a live, animated data-flow graph. Clicking any pipeline node drills into its current state. Data particles flow along edges as events fire in the engine.

### Tasks

**14.1 — WebSocket server**
- Create `internal/web/server.go`.
- `te web [--port=8080]` starts an HTTP server as a goroutine within the main engine binary.
- The server subscribes to the engine's event bus and streams structured JSON events to connected browser clients over WebSocket.
- Serve the frontend as static assets embedded in the Go binary via `go:embed`.
- Support multiple concurrent browser connections.
- Acceptance: Starting `te web` while the engine runs, connecting via WebSocket from a test client receives a stream of JSON events matching engine activity.

**14.2 — Event protocol**
- Define the WebSocket event protocol — each message is a JSON object:
  ```json
  {
    "type": "DataFetched",
    "timestamp": "2026-03-01T14:30:00Z",
    "source": "alpaca",
    "target": "indicator_engine",
    "payload": { "ticker": "AAPL", "candle_count": 30 }
  }
  ```
- Map each internal event bus event type to a source→target edge in the graph.
- Include a `snapshot` endpoint that sends the current state of all pipeline nodes on initial connection (so the UI doesn't start empty).
- Acceptance: New WebSocket connections receive a snapshot followed by live events. Events include source, target, and payload.

**14.3 — REST API for drill-down data**
- Create `internal/web/handlers.go`.
- Endpoints for node inspection:
  - `GET /api/node/risk` — current risk check status (daily loss usage, drawdown, position count).
  - `GET /api/node/portfolio` — current portfolio snapshot.
  - `GET /api/node/ticker/:ticker` — latest analysis, indicators, and signals for a ticker.
  - `GET /api/node/models` — model status, recent latency, accuracy.
  - `GET /api/signals/recent` — recent signal feed.
  - `GET /api/trades/recent` — recent trade feed.
- Acceptance: Each endpoint returns correct JSON data matching the engine's current state.

**14.4 — React frontend: project setup**
- Initialize the React frontend in `web/frontend/`.
- Set up the build pipeline (Vite or Next.js static export) to produce a `dist/` directory that gets embedded into the Go binary.
- Install core dependencies: React, React Flow (for the graph layout), Framer Motion (for animations), a WebSocket client library.
- Implement the dark theme foundation: dark background, monospace typography, cyan/magenta/amber accent palette.
- Acceptance: `npm run build` produces static assets. The Go binary serves them at `http://localhost:8080`. The page loads with the dark theme.

**14.5 — Data flow graph**
- Build the main visualization using React Flow:
  - **Source nodes** (left column): Alpaca, Polymarket, Kalshi, Finnhub/Benzinga. Each node pulses/glows when data arrives.
  - **Processing nodes** (center column): Data Enrichment → Indicator Computation → Prompt Assembly → LLM Analysis → Consensus Resolution → Risk Checks.
  - **Output nodes** (right column): Executed Trades, Blocked Signals, Queued Approvals, flowing to exchange endpoints.
  - Edges between nodes represent data flow. Edges glow when an event traverses them.
- Node metadata on hover: last event time, event count, average latency.
- Acceptance: The graph renders with all nodes and edges. When the engine processes a tick, events visually traverse the graph from left to right.

**14.6 — Data particles and animations**
- Implement animated particles that flow along graph edges when events fire:
  - Market data particles: blue.
  - News particles: amber.
  - LLM response particles: purple.
  - Trade execution particles: green (buy) / red (sell).
  - Blocked signal particles: gray.
- Use Framer Motion for smooth particle animation along edge paths.
- Add bloom/glow effects on active nodes (CSS filters or SVG filters).
- Acceptance: Real-time engine activity produces visible, color-coded particles flowing through the graph. The visual aesthetic feels futuristic and mission-control-like.

**14.7 — Node drill-down panels**
- Clicking any node opens a side panel with detailed current state (fetched from the REST API):
  - **Risk node** → active check results, daily loss bar, drawdown gauge.
  - **Ticker node** → latest analysis, indicator values, recent signals.
  - **LLM node** → most recent prompt/response, token usage, latency.
  - **Portfolio node** → current value, positions, P&L.
- Panels update in real time as new events arrive.
- Acceptance: Clicking the risk manager node shows current risk status. Clicking AAPL shows its latest analysis. Data is live.

**14.8 — Connection status and controls**
- Display WebSocket connection status (connected/reconnecting/disconnected).
- Auto-reconnect on connection loss.
- Playback controls: pause/resume particle flow (events still buffer, but animation stops).
- Event rate indicator (events/second).
- Acceptance: Disconnecting the engine pauses particles and shows "disconnected." Restarting the engine auto-reconnects and resumes.

### Phase Test Plan

1. Start the engine with `te run`, then `te web`.
2. Open `http://localhost:8080` — graph renders with all pipeline nodes.
3. Watch real-time particles flow as the engine processes tickers.
4. Verify color coding: blue for market data, amber for news, purple for LLM, green/red for trades.
5. Click the risk manager node — side panel shows current risk status.
6. Click a ticker node — side panel shows latest analysis and indicators.
7. Open a second browser tab — both receive live updates.
8. Kill the engine — web UI shows "disconnected." Restart — auto-reconnects.
9. Verify the aesthetic: dark theme, glowing edges, smooth particle animations.
