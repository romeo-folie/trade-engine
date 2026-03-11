# TradeEngine Cost Reduction PRD

## 1) Context
TradeEngine token usage is too high for mainstream users in continuous operation. Current testing indicates burn rates that can exceed practical monthly budgets for solo founders and retail users.

## 2) Goal
Reduce model and notification operating cost by **60–90%** while maintaining acceptable signal quality and safety.

## 3) Non-Goals
- No removal of risk checks.
- No forced single-model strategy for all users.
- No quality guarantees equal to full premium ensemble in all conditions.

## 4) Product Principles
1. **Budget first**: default paths should be affordable.
2. **Escalate quality only when needed**.
3. **Explainability over verbosity**: concise structured outputs.
4. **User control**: explicit cost profiles and budget limits.

## 5) Cost Reduction Strategies

### 5.1 Reduce call frequency (largest lever)
- Default tick interval: move from 30m to 60m for swing mode.
- Optional 90m/120m profile for strict budget users.
- Per-ticker cooldown after HOLD (e.g., 2–4h) unless breakout condition.
- Re-analysis only on material state change:
  - indicator delta thresholds
  - price move threshold
  - meaningful news novelty

### 5.2 Two-stage model routing
- **Stage A (cheap classifier/filter)**: decide `skip | analyze`.
- **Stage B (full analysis)** only if Stage A passes gates.
- Skip path must produce deterministic reason for observability.

### 5.3 Adaptive ensemble depth
- Low-impact context: 1 analyst only.
- Medium-impact context: 2 analysts.
- High-impact/uncertain context: 3 analysts + arbiter.
- Arbiter should be exception path, not default path.

### 5.4 Prompt/token optimization
- Cap history window sizes per strategy.
- Use summarized/news-digest input, not raw article dumps.
- Enforce short structured JSON schema and concise reasoning.
- Hard max output tokens by model role.

### 5.5 Caching and reuse
- Context hash: ticker + timeframe + indicator snapshot + news digest hash.
- If unchanged, reuse prior analysis for a configurable TTL.
- Track cache hit rates and skip-call savings.

#### 5.5.1 Cache key/fingerprint specification
For each analysis candidate, build a stable fingerprint from:
- market identity: `ticker`, `asset_class`, `timeframe`
- strategy identity: `strategy_name`, `strategy_version`
- model profile identity: `cost.profile`, analyst set, arbiter model, prompt template version
- indicator snapshot (rounded): RSI, EMA gap %, MACD sign/magnitude bucket, ADX bucket, ATR% bucket
- price regime bucket: return over lookback bucketed (e.g., <-2%, -2..-0.5, -0.5..0.5, 0.5..2, >2)
- news digest hash: hash of normalized top-N headlines/entities/sentiment summary
- risk context: open-position count bucket, current exposure bucket

Fingerprint implementation notes:
- canonical JSON serialization (sorted keys)
- rounded/bucketed numeric values to avoid cache misses from tiny noise
- SHA-256 fingerprint key

#### 5.5.2 Cache storage schema (proposed)
Table: `analysis_cache`
- `id` UUID PK
- `fingerprint` TEXT UNIQUE NOT NULL
- `ticker` TEXT NOT NULL
- `timeframe` TEXT NOT NULL
- `strategy` TEXT NOT NULL
- `model_profile` TEXT NOT NULL
- `analysis_json` JSONB NOT NULL
- `decision` TEXT NOT NULL (`BUY|SELL|HOLD`)
- `confidence` DOUBLE PRECISION NOT NULL
- `reason_code` TEXT NULL (`CACHE_HIT`, `FORCED_REFRESH`, etc.)
- `created_at` TIMESTAMPTZ NOT NULL
- `expires_at` TIMESTAMPTZ NOT NULL
- `last_used_at` TIMESTAMPTZ NOT NULL
- `use_count` INT NOT NULL DEFAULT 0

Indexes:
- `UNIQUE(fingerprint)`
- `(ticker, timeframe, expires_at DESC)`
- `(expires_at)` for cleanup jobs

#### 5.5.3 Invalidation rules
Immediate invalidate (force fresh analysis) when any of:
- absolute price move since cached state >= `analysis.cache.invalidate_price_move_pct`
- indicator regime flip (trend/momentum class change)
- new high-impact news item (novelty/relevance above threshold)
- active risk event (drawdown gate near trigger, daily loss close to cap)
- order/trade lifecycle event for ticker (position opened/closed/partially filled)

Time-based invalidate:
- TTL by profile:
  - budget: 180 min
  - balanced: 120 min
  - pro: 60 min
- forced refresh every `N` cycles even if unchanged (anti-staleness)

#### 5.5.4 Cache read/write flow
1. Build fingerprint from current state.
2. Lookup cache by fingerprint and unexpired TTL.
3. If hit and no invalidation trigger: reuse cached analysis, tag decision as `CACHE_HIT`.
4. If miss/invalid: run inference, persist result in cache, emit `CACHE_MISS` metric.
5. Update `last_used_at`, `use_count` on hits.

#### 5.5.5 Observability metrics
- `analysis_cache_hit_ratio`
- `analysis_cache_miss_ratio`
- `analysis_cache_invalidation_count{reason}`
- `token_spend_saved_estimate`
- `avg_latency_saved_ms`

### 5.6 News enrichment throttling
- Extract entities/relationships only for top-N relevant articles.
- Keep existing RPM and article caps; make profile-aware defaults.

### 5.7 Telegram batching (notification cost + UX)
- Replace per-ticker serial messages with per-tick summary batch.
- Immediate sends only for critical failures.
- Batch content:
  - analyzed count
  - executed / blocked / pending
  - top actionable signals
- Optional daily digest for non-urgent metrics.

### 5.8 Runtime budget guardrails
- Daily token budget cap.
- Weekly token budget cap.
- Auto-degrade profile when threshold crossed.
- Expose `cost_per_signal` and `cost_per_executed_trade` in TUI/reports.

## 6) Configuration Additions

```yaml
cost:
  profile: budget               # budget | balanced | pro
  daily_token_budget: 200000
  weekly_token_budget: 1200000
  auto_degrade_on_budget_hit: true

analysis:
  material_change_only: true
  hold_cooldown_min: 120
  context_cache_ttl_min: 180

notifications:
  telegram:
    batch_enabled: true
    batch_window_sec: 60
    max_items_per_batch: 25
    include_holds: false
    min_confidence_for_alert: 0.65
```

## 7) Cost Profiles

### Budget
- 60–120m tick
- 1 cheap analyst default
- arbiter rare/disabled except uncertain high-impact cases
- strict batching and cache

### Balanced
- 60m tick
- adaptive 1–2 analysts
- arbiter on disagreement/high impact

### Pro
- current near-full ensemble behavior
- minimal degradation, highest cost

## 8) Rollout Plan

### Phase 1: Immediate (low risk)
- Telegram batching
- prompt/token caps
- HOLD cooldown + material-change gate

### Phase 2: Core optimization
- two-stage routing
- adaptive ensemble depth
- context-hash cache

### Phase 3: Productization
- cost profiles
- budget guardrails + auto-degrade
- cost telemetry in TUI/report

## 9) Success Metrics
- 7-day average token spend reduction (%).
- Cost per executed trade.
- Signal quality deltas vs baseline (paper PnL, win rate, drawdown).
- Message volume reduction (Telegram sends/tick).
- User-reported notification clarity.

## 10) Risks & Mitigations
- **Risk**: over-pruning lowers signal quality.
  - Mitigation: staged rollout + A/B profile comparison.
- **Risk**: cache stale decisions.
  - Mitigation: TTL + forced refresh triggers on volatility/news shock.
- **Risk**: hidden degradation from budget mode.
  - Mitigation: explicit active profile + auto-degrade notices.

## 11) Open Questions
1. Default profile for first-run users?
2. Should budget guardrails fail-closed (skip trading) or degrade-only?
3. Which markets/tickers should always bypass cheap-filter stage?
4. What confidence/uncertainty thresholds trigger arbiter escalation?

## 12) Implementation Tasks (cache-focused)

### Task C1 — DB migration + storage layer
- Add `analysis_cache` migration with schema/indexes in §5.5.2.
- Add store methods:
  - `GetCachedAnalysis(fingerprint)`
  - `UpsertCachedAnalysis(entry)`
  - `TouchCachedAnalysis(fingerprint)`
  - `DeleteExpiredCachedAnalyses(now)`
- Add migration test coverage + integration tests.

### Task C2 — Fingerprint builder
- Implement `analysis/cache/fingerprint.go` with canonical serialization + SHA-256.
- Include all fields listed in §5.5.1.
- Add unit tests for:
  - deterministic output
  - bucket/rounding stability
  - version/profile changes invalidating keys.

### Task C3 — Invalidation engine
- Implement `ShouldInvalidate(cacheEntry, liveState)` with rules in §5.5.3.
- Add reason codes (`PRICE_MOVE`, `REGIME_FLIP`, `NEWS_NOVELTY`, `RISK_EVENT`, `TTL_EXPIRED`, `FORCED_REFRESH`).
- Add unit tests for each invalidation trigger.

### Task C4 — Engine integration
- Insert cache lookup before expensive analysis call.
- Reuse cached result on valid hit; annotate event/log with `CACHE_HIT`.
- On miss/invalidate, run normal analysis and cache result.
- Ensure existing risk checks still run on reused result.

### Task C5 — Metrics + reporting
- Emit metrics in §5.5.5.
- Add TUI/report counters:
  - hit rate
  - estimated tokens saved
  - cache invalidation reason distribution.

### Task C6 — Profile wiring
- Add config keys:
  - `analysis.cache.enabled`
  - `analysis.cache.ttl_min`
  - `analysis.cache.force_refresh_cycles`
  - `analysis.cache.invalidate_price_move_pct`
- Set defaults by cost profile.

### Task C7 — Safety guardrails
- If cache subsystem errors, fail open to fresh analysis (never block trading loop).
- Add max cache row retention + cleanup job.
- Add migration rollback note and operational runbook.
