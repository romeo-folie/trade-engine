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
