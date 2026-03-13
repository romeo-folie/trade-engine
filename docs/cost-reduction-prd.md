# TradeEngine Cost Reduction PRD

> **Status:** Agreed — ready for implementation
> **Last updated:** 2026-03-13 (reflects design decisions from planning session)
> **Version:** v2 (supersedes original draft)

---

## 1) Context

TradeEngine token usage is too high for continuous operation. Current testing indicates burn rates that can exceed practical monthly budgets for solo founders and retail users.

Current config runs a full 3-analyst ensemble + arbiter on every tick for every ticker. At 30-minute intervals with 50 tickers, this generates significant API spend regardless of market activity.

---

## 2) Goal

Reduce model and notification operating cost by **60–90%** while maintaining acceptable signal quality and safety.

---

## 3) Non-Goals

- No removal of risk checks.
- No forced single-model strategy for all users.
- No quality guarantees equal to full premium ensemble in all conditions.

---

## 4) Product Principles

1. **Budget first**: default paths should be affordable.
2. **Escalate quality only when needed**.
3. **Explainability over verbosity**: concise structured outputs.
4. **User control**: explicit cost profiles and budget limits.

---

## 5) Scope

### IN scope

| # | Strategy |
|---|----------|
| 5.1 | Frequency reduction + material-change gating |
| 5.2 | Two-stage classifier routing |
| 5.3 | Adaptive ensemble depth |
| 5.4 | Prompt/token optimization |
| 5.5 | Context caching (full) |
| 5.6 | News entity deduplication (store-level only — no throttling) |
| 5.7 | Telegram batching |
| 5.8 | Runtime budget guardrails |
| — | Named model registry |
| — | Cost profiles (budget / balanced / pro) |

### OUT of scope

- News enrichment throttling (article-level limits). The system must remain fully aware of entity relationships. Deduplication prevents storage bloat without reducing enrichment coverage.
- A/B testing infrastructure (deferred).

---

## 6) Model Registry

All models used anywhere in the system are declared in a central `models.registry`. Components reference **role names**, not raw model strings. The engine resolves roles to providers at startup; unknown roles are a fatal error.

### Registry definition

```yaml
models:
  registry:
    analyst_cheap:
      provider: openai
      model: gpt-4o
      api_key_env: OPENAI_API_KEY
      max_tokens: 2048
      temperature: 0.1
      timeout_sec: 60

    analyst_mid:
      provider: google
      model: gemini-3.1-pro-preview
      api_key_env: GEMINI_API_KEY
      max_tokens: 2048
      temperature: 0.1
      timeout_sec: 60
      fallback: gemini_flash       # used on 429/503

    gemini_flash:
      provider: google
      model: gemini-2.5-flash
      api_key_env: GEMINI_API_KEY
      max_tokens: 2048
      temperature: 0.1
      timeout_sec: 60

    analyst_frontier:
      provider: anthropic
      model: claude-sonnet-4-6
      api_key_env: ANTHROPIC_API_KEY
      max_tokens: 2048
      temperature: 0.1
      timeout_sec: 60

    arbiter:
      provider: anthropic
      model: claude-sonnet-4-6
      api_key_env: ANTHROPIC_API_KEY
      max_tokens: 4096
      temperature: 0.1
      timeout_sec: 90

    classifier:
      provider: openai
      model: gpt-4o-mini
      api_key_env: OPENAI_API_KEY
      max_tokens: 256
      temperature: 0.0
      timeout_sec: 15

    news_enricher:
      provider: openai
      model: gpt-4o-mini
      api_key_env: OPENAI_API_KEY
      max_tokens: 2048
      temperature: 0.1
      timeout_sec: 60

    embedder:
      provider: ollama
      model: nomic-embed-text
      ollama_base_url: http://localhost:11434
      dimension: 768

  roles:
    analysts: [analyst_cheap, analyst_mid, analyst_frontier]
    arbiter: arbiter
    classifier: classifier
    news_enricher: news_enricher
    embedder: embedder
```

### Migration notes

The following legacy config fields are **removed** and resolved through `roles` instead:
- `models.analysts[]` → `roles.analysts`
- `models.arbiter` → `roles.arbiter`
- `models.primary` → removed (superseded by adaptive depth tier selection)
- `news.extraction_provider` + `news.extraction_model` → `roles.news_enricher`
- `embeddings.provider` + `embeddings.model` → `roles.embedder`

### Fallback logic

When a model returns 429 or 503, the model client resolver checks the registry entry for a `fallback` key and retries once with that model. Logged as `FALLBACK_USED{from, to, reason}`. One hop only — no cascading chains.

---

## 7) Cost Reduction Strategies (Detailed)

### 7.1 Frequency reduction + material-change gating (largest lever)

Default tick interval moves from 30m to 60m for swing mode.

Re-analysis only triggers when at least one material change is detected (pure code, no LLM):

| Signal | Threshold (default) |
|--------|-------------------|
| RSI delta | ≥ 3.0 pts |
| EMA gap delta | ≥ 0.5% |
| Price move since last analysis | ≥ 0.75% |
| News digest hash | changed |
| HOLD cooldown elapsed | 120 min post-HOLD decision |

All thresholds are configurable. If none of the above are met → automatic skip with `reason_code: NO_MATERIAL_CHANGE`. Zero LLM calls.

### 7.2 Two-stage classifier routing

A classifier gate runs before any analyst call.

**Layer 1 — Rule-based (zero tokens):**
Evaluates the material-change conditions above deterministically. Skip if all below threshold.

**Layer 2 — LLM fallback (only when Layer 1 is ambiguous):**
When one or more indicators are marginally changed, a minimal prompt is sent to `roles.classifier` (gpt-4o-mini):

```
You are a trading signal filter.
Ticker: {ticker} | Timeframe: {timeframe}
RSI: {prev} → {curr} ({delta})
EMA gap: {prev}% → {curr}% ({delta}%)
MACD: {sign}, magnitude {bucket}
News: {novelty_summary}
Last decision: {decision} ({elapsed}h ago)

Reply SKIP or ANALYZE with a reason in 10 words max.
```

Output: `SKIP <reason>` or `ANALYZE <reason>`. ~100 input / ~20 output tokens.

Skip path emits: `reason_code`, delta values, metrics increment (`analysis_skips_total{reason}`).

Classifier operates in two modes controlled by config:
- `shadow`: classifies but does not block — logs `WOULD_SKIP` / `WOULD_ANALYZE` for threshold tuning
- `active`: live gating

Default: `shadow` (safe rollout).

### 7.3 Adaptive ensemble depth

Context is scored for impact complexity before invoking analysts. The score determines how many analysts run.

**Impact score inputs (weighted):**

| Signal | Weight |
|--------|--------|
| ATR% (volatility) | 0.30 |
| News novelty | 0.25 |
| Risk proximity (distance to stop/drawdown limits) | 0.25 |
| Trend strength (ADX) | 0.20 |

**Depth tiers:**

| Score | Depth | Analysts | Arbiter |
|-------|-------|----------|---------|
| < 0.3 | 1 | gpt-4o only | Never |
| 0.3–0.65 | 2 | gpt-4o + Gemini 3.1 Pro | Only on disagreement |
| ≥ 0.65 | 3 | All three | Standard rules |

Thresholds and weights are configurable. Arbiter rules (fast-path confidence, spread) are unchanged.

### 7.4 Prompt/token optimization

- Hard `max_tokens` caps by role: classifier=256, news_enricher=2048, analyst=2048, arbiter=4096.
- News enrichment input: top-N article digest (summarized), not raw dumps.
- History window capped per strategy type.
- Structured JSON schema output enforced for all analyst responses.

### 7.5 Context caching

Full cache implementation. See original PRD §5.5 for complete schema — retained as-is.

**Summary:**
- Fingerprint: ticker + timeframe + strategy + bucketed indicators + price regime + news digest hash + risk context → SHA-256
- Storage: `analysis_cache` table (PostgreSQL)
- TTL by profile: budget=180m, balanced=120m, pro=60m
- Invalidation triggers: price move ≥ threshold, indicator regime flip, high-impact news, risk event, trade lifecycle event
- Cache flow: `Classify → CacheCheck → LLMCall`
- Fail-open: cache errors never block the trading loop

### 7.6 News entity deduplication

No change to enrichment logic or article limits. Store layer uses `UPSERT` to prevent duplicate writes:
- Entities: upsert on `(name, type)`
- Relationships: upsert on `(subject_id, object_id, relationship_type)`

No throttling. Full relationship graph maintained.

### 7.7 Telegram batching

Per-tick summary batch replaces per-ticker serial messages.

```yaml
notifications:
  telegram:
    batch_enabled: true
    batch_window_sec: 60
    max_items_per_batch: 25
    include_holds: false
    min_confidence_for_alert: 0.65
```

Immediate sends retained for: errors, critical failures only.

Batch content: analyzed count, executed/blocked/pending, top actionable signals.

### 7.8 Runtime budget guardrails

- Daily + weekly token budget caps.
- Auto-degrade profile when threshold crossed (pro → balanced → budget).
- `cost_per_signal` and `cost_per_executed_trade` exposed in TUI and reports.

---

## 8) Cost Profiles

```yaml
cost:
  profile: balanced             # budget | balanced | pro
  daily_token_budget: 200000
  weekly_token_budget: 1200000
  auto_degrade_on_budget_hit: true
```

| Profile | Tick | Depth default | Arbiter | Cache TTL | Batching |
|---------|------|--------------|---------|-----------|---------|
| budget | 90–120m | 1 | disabled | 180m | strict |
| balanced | 60m | adaptive 1–2 | on disagreement | 120m | enabled |
| pro | 60m | adaptive 1–3 | standard | 60m | optional |

---

## 9) Implementation Plan

Delivered as **3 sequential epics**. One branch per epic, one PR per epic (opened only when all tasks in the epic are complete).

### Epic 1 — Foundation & Quick Wins

Low-risk, no new LLM paths. Shippable independently.

| Task | Description |
|------|-------------|
| E1-T1 | Model registry — new config schema, migrate all legacy model fields, startup validation, fallback resolver |
| E1-T2 | Tick interval + HOLD cooldown + material-change gate — configurable tick (default 60m), 120m HOLD cooldown, indicator delta checks, news hash; pure code |
| E1-T3 | Prompt/token caps — `max_tokens` by role, news digest summarizer, history window caps |
| E1-T4 | Telegram batching — per-tick summary, immediate-only for errors, config keys |
| E1-T5 | News entity deduplication — UPSERT on canonical keys in entity/relationship store methods |

### Epic 2 — Intelligence Layer

Core cost reduction machinery. Depends on Epic 1 (model registry must be in place).

| Task | Description |
|------|-------------|
| E2-T1 | Classifier — Layer 1 rule-based (`analysis/classifier/rule_based.go`), skip path, reason codes, metrics |
| E2-T2 | Classifier — Layer 2 LLM fallback (`analysis/classifier/llm_classifier.go`), shadow/active mode config |
| E2-T3 | Adaptive ensemble depth — impact scorer, tier selection, analyst invocation by depth |
| E2-T4 | Cache — DB migration + store methods (`GetCachedAnalysis`, `UpsertCachedAnalysis`, `TouchCachedAnalysis`, `DeleteExpired`) + fingerprint builder |
| E2-T5 | Cache — invalidation engine (`ShouldInvalidate`) + engine integration (`Classify → CacheCheck → LLMCall`), fail-open |
| E2-T6 | Cache observability — Prometheus metrics, TUI/report counters |

### Epic 3 — Productization

User-facing cost controls. Depends on Epic 2.

| Task | Description |
|------|-------------|
| E3-T1 | Cost profiles — `budget/balanced/pro` config, profile-aware defaults wired into all subsystems |
| E3-T2 | Budget guardrails — daily/weekly token caps, auto-degrade, `cost_per_signal` + `cost_per_executed_trade` in TUI/report |

---

## 10) Success Metrics

- 7-day average token spend reduction (%).
- Cost per executed trade.
- Signal quality deltas vs baseline (paper PnL, win rate, drawdown).
- Message volume reduction (Telegram sends/tick).
- Cache hit rate.
- Classifier skip rate (shadow mode first, then active).

---

## 11) Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Classifier over-prunes, misses signals | Shadow mode first; tunable thresholds; skip-rate monitoring |
| Cache returns stale decisions | TTL + forced refresh on volatility/news shock; fail-open |
| Gemini 3.1 Pro Preview unavailable | Registry fallback to gemini-2.5-flash on 429/503 |
| Budget mode degrades quality silently | Active profile displayed in TUI; auto-degrade sends Telegram notice |

---

## 12) Open Questions

1. Default profile for first-run users? (Proposed: `balanced`)
2. Budget guardrails: fail-closed (skip trading) or degrade-only? (Proposed: degrade-only, never block)
3. Which tickers/conditions should always force depth-3 regardless of score?
4. Arbiter escalation confidence threshold for depth-2? (Proposed: reuse existing `fast_path_max_spread`)
