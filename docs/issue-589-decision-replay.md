# Issue #589: Reproducible analysis inputs and deterministic decision replay

Status: exploration and contract, written 2026-11-09 against `main` @ `fbc2cbe6`.
Every statement in the gap inventory cites the code path it was read from. It
describes what the code does, not what other docs say it does.

## 1. Live request path (as built)

```
processStock (engine.go)
  -> runAnalysisWithProfile (engine.go ~L950)
       candles (store, upserted)  -> snaps (cap: MaxCandlesPerPrompt / HistoryWindowByStrategy)
       store.GetRecentNews(now-30d) -> promptNewsSnippets(now, cap)      [headline only]
       knowledge.BuildContext(now positions) + CANDIDATE ORIGIN prefix
       FindSimilarAnalyses (pgvector, live table)
       feedback.GetPerformanceContext (live, time.Now day boundary)
       anti-chase TimingSnapshot (daily history, mark, news)
       portfolioContextForPrompt (live tracker / open trades)
       unifiedTimeframeEvidence (supplemental TFs, same request)
  -> AnalyzeSpecialist | AnalyzeFull
       single | depth-1 | ensemble fan-out -> TryFastPath | Arbitrate | majorityVote
  -> ValidateFinalLensSelection, contextErr BUY->WAIT downgrade
  -> InsertEnsembleAnalysis | InsertAnalysisWithScannerRunAudit  (analyses row)
  -> generateSignal -> risk/entry gates -> signals -> execution
```

There is **no** separate entry-confirmation model request on the live path.
`unifiedTimeframeEvidence` (`timeframe_workflow.go`) loads every configured
supplemental timeframe into `AnalysisRequest.TimeframeContexts` before the
ensemble starts. The workflow audit records `Mode: "unified"`. The
`TimeframeRoleEntryConfirmation` role and the `PrimaryThesisContext` prompt
contract still exist in `analysis/prompts`, but no engine caller sets them.
The confirmation paragraph in `internal/analysis/README.md` was stale and is
corrected in this change.

## 2. Gap inventory

Legend: **R** retained (immutable enough to replay), **M** retained but mutable
or prunable, **D** derivable only with hindsight, **X** not retained.

### 2.1 Market inputs

| Input | Status | Evidence |
|---|---|---|
| Primary-TF bars supplied to the prompt | M | Only as text inside `raw_prompt`. `candles` rows are upserted `ON CONFLICT (ticker,timeframe,ts) DO UPDATE` (`store/candles.go`), so a revised or still-forming bar overwrites the value the model saw. There is no `observed_at`/`fetched_at` column. |
| Intraday bars (1m/5m/15m) | M | `CleanupIntradayCandles` deletes them after 90 days (`store/retention.go`). |
| Supplemental-TF bars and indicators | M | Rendered into `raw_prompt` only. Not structured in `timeframe_workflow` (that holds status/counts, not bars). |
| Indicator snapshot (primary) | R | `analyses.indicators` = `json.Marshal(req.Indicators)` per provider call. Supplemental-TF indicators are not stored structurally. |
| Anti-chase timing snapshot | R | `analyses.timing_snapshot`. |
| Bars used for the indicator warm-up (2y) | D | Only the latest-row state is in the DB. Reconstruction depends on candles never having been revised. |
| Observation time of the freshest bar | X | `candleFetchObservation` is used for the timing mark, not persisted. |

### 2.2 News

| Input | Status | Evidence |
|---|---|---|
| Headlines supplied | M | Inside `raw_prompt` only. The prompt omits `summary`/`url`. |
| Age shown to the model | X | `prompts/stock.go` renders `time.Since(n.PublishedAt)` **at render time inside each provider**, so it is a hidden wall-clock read. Analysts render at slightly different instants. Re-rendering later changes the prompt. |
| Sentiment label | M | `sentiment_score` is overwritten on conflict whenever the new score is non-null (`store/news.go`: `COALESCE(EXCLUDED.sentiment_score, ...)`). |
| Ticker associations | M | Merged on conflict. |
| Ingestion time | R/M | `news_items.created_at` exists but is not passed to the request or the prompt. Rows are deleted after 90 days (`CleanupNewsItems`). |
| Prompt selection window | X | `promptNewsSnippets(items, time.Now(), cap)` uses a 48h cutoff relative to the wall clock. The decision cutoff is not recorded. |

### 2.3 Context blocks

| Input | Status | Evidence |
|---|---|---|
| Portfolio (cash, equity, positions, entry counts) | M | Text inside `raw_prompt` only, read from the live tracker or open trades at request time. |
| Knowledge-graph context | M | Built from the live graph and current `PositionMap()`. Only the text in `raw_prompt` survives. |
| Similar past analyses | M | pgvector search over the live `analyses` table (rows can be pruned). The match set, threshold and IDs are not recorded. |
| Performance context | M | `feedback.GetPerformanceContext` uses a live `time.Now()` day boundary. Text only. |
| Discovery context / execution profile | R | Watchlist-owned, rendered into the prompt. Lens audit columns record selected/rejected lenses. The profile version is implicit. |
| Scanner lineage | R | `analyses.scanner_run_id`. |
| Risk policy rendered in prompt | M | Derived from live `cfg.Risk`. There is no effective-config hash. |

### 2.4 Model calls

| Item | Status | Evidence |
|---|---|---|
| Per-analyst prompt/response | R | `ensemble_results` = `json.Marshal([]AnalystResult)`, which includes `Meta.RawPrompt`/`RawResponse`. `RawPrompt` is `system + "\n\n---\n\n" + user`, not the exact HTTP body. |
| Request parameters (temperature, max_tokens, API version) | X | Set in each adapter from config. Not persisted. Gemini/OpenAI/Ollama default an unset temperature to 0.7. Anthropic sends the configured value as-is (0 when unset). |
| Resolved model identity | X | `Meta.Model = c.cfg.Model` (requested). The provider-returned `model` field is not parsed. |
| Analyst failures | X | `AnalystResult.Err` is an `error` interface. `json.Marshal` emits `{}` (verified), so the failure reason is lost from `ensemble_results`. |
| Whole-analysis failures | X | `runAnalysisWithProfile` returns before persistence on `err != nil`. Only a scanner outcome string survives (and only when `scanner_run_id` is set). Partial analyst results, including paid raw responses, are discarded. |
| Pre-LLM exits (`indicators_missing`, `model_unavailable`, SELL-only short-circuit) | X | Scanner outcome / block event only. There is no analysis row. |
| Specialist fallback hop | M | The final provider is recorded. The failed primary attempt is only inside the wrapped error text, which is then lost as above. |
| Gemini 429/503 retries | X | Logged only. The attempt count is not persisted. |
| Arbiter prompt and answer | R | `arbiter_response` includes `Meta`. |
| Arbiter analyst permutation | D | `rand.Perm` (`ensemble.go`). It can only be recovered by parsing labels out of the raw arbiter prompt. |
| Latency/tokens | R | Per analyst. |

### 2.5 Resolution determinism

| Behavior | Problem |
|---|---|
| `successfulResults` sorts by `LatencyMs` | Fast-path `DecisiveReason`, `consensusRejections` first-wins reasons (fast path, majority and arbiter paths), and majority-vote `SizePct/StopLoss/TakeProfit` (taken from the *fastest* winning analyst) all depend on network timing. Same recorded answers + different latencies = different final output. Arbiter label order is separately random (next rows). |
| `runAnalystSet` appends in goroutine completion order | The stored `ensemble_results` order is nondeterministic. |
| `rand.Perm` in `Arbitrate` | The arbiter prompt is unreproducible and the seed is not recorded. |
| `majorityVote` builds `leaders` from map iteration | Resolved by a fixed switch, so it is deterministic today. Keep it covered by tests. |
| Rounding | Fast path rounds confidence to 3 dp, size to 2 dp, stops to 4 dp. This is stable but undocumented as policy. |
| Specialist | Results are sorted by canonical role order. This is already deterministic. |

### 2.6 Cache

`analysis_cache` (`store/analysis_cache.go`) and `analysis/cache.Build` exist,
but the live engine never reads or writes the cache: only the expiry sweeper
and a metrics snapshot run. The backtest runner uses its own prompt cache keyed
by prompt hash/provider/model. Neither is an identity for a decision: the
fingerprint deliberately buckets indicators, so cache hits are lossy by design.
A replay record must never be served from it.

### 2.7 Retention and versioning

* `CleanupPromptCache` deletes `analyses` rows older than 6 months that are not
  referenced by a signal. Every HOLD/WAIT and most rejected BUYs are therefore
  pruned, which removes exactly the non-actionable decisions a paired evaluation
  needs.
* There is no code revision in the binary beyond `-X main.version` from the
  Makefile, and that is not attached to analyses.
* There are no prompt-template, question or policy versions. `DecisionPolicyAudit.Version`
  (specialist) and `TimeframeWorkflowAudit.Version` are the only precedents.

## 3. Contract

### 3.1 Decision snapshot (v1)

There is one immutable row per decision attempt, written *before* any model
call, plus one row per stage call. Schema sketch:

```sql
CREATE TABLE decision_snapshots (
  id                UUID PRIMARY KEY,
  schema_version    INT  NOT NULL,          -- contract version, starts at 1
  ticker            TEXT NOT NULL,
  decision_cutoff   TIMESTAMPTZ NOT NULL,   -- the single clock for this decision
  code_revision     TEXT NOT NULL,          -- vcs.revision from build info, or 'unknown'
  workflow          TEXT NOT NULL,          -- single | ensemble | specialist
  policy_version    TEXT NOT NULL,          -- resolution policy (fast-path/majority/arbiter rules)
  prompt_version    TEXT NOT NULL,
  input_hash        TEXT NOT NULL,          -- sha256 of canonical inputs JSON
  decision_hash     TEXT NOT NULL,          -- sha256(schema_version|input_hash|policy_version|prompt_version|config_hash)
  inputs            JSONB NOT NULL,         -- canonical payload (3.2)
  missing           JSONB NOT NULL,         -- explicit list of absent/failed inputs
  effective_config  JSONB NOT NULL,         -- sanitized allowlist (3.3)
  config_hash       TEXT NOT NULL,
  scanner_run_id    UUID,
  analysis_id       UUID,                   -- linked after persistence; NULL on failure
  outcome           TEXT NOT NULL CHECK (outcome IN
                      ('pending','completed','failed','short_circuited','abandoned')),
  final_output      JSONB,                  -- resolved response + lens audit
  output_hash       TEXT,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE decision_stage_calls (
  snapshot_id       UUID NOT NULL REFERENCES decision_snapshots(id),
  stage             TEXT NOT NULL,          -- analyst | specialist:<role> | arbiter | risk_manager
  ordinal           INT  NOT NULL,          -- stable position (3.4)
  attempt           INT  NOT NULL,          -- 0 primary, 1.. retries/fallbacks
  provider          TEXT NOT NULL,
  requested_model   TEXT NOT NULL,
  resolved_model    TEXT,                   -- provider-reported, when available
  request_params    JSONB NOT NULL,         -- temperature, max_tokens, api version, timeout
  system_prompt     TEXT,
  user_message      TEXT,
  raw_response      TEXT,
  parsed_response   JSONB,
  error             TEXT,
  input_tokens INT, output_tokens INT, latency_ms BIGINT,
  PRIMARY KEY (snapshot_id, stage, ordinal, attempt)
);
```

Both tables are append-only. The application role gets no `UPDATE` except the
single `analysis_id`/`outcome`/`final_output`/`output_hash` completion write,
guarded by `WHERE outcome = 'pending'`. On startup, the engine marks any
`pending` row older than the longest configured analysis deadline as
`abandoned`, with the stage calls already written kept as evidence.
`abandoned` snapshots are reported as incomplete by replay and are never
completed retroactively.

**Identities.** `input_hash` identifies *what the models saw*. `decision_hash`
identifies *the decision under a given policy*: changing only the policy,
prompt template, or effective config yields the same `input_hash` and a
different `decision_hash`. `output_hash` identifies the resolved result.
Recorded replay asserts `output_hash` equality under the same
`decision_hash`. They are excluded from `CleanupPromptCache`.
Retention is separate and explicit, defaulting to keep.

### 3.2 Canonical inputs payload

These are typed Go structs with explicit JSON tags, marshalled with sorted map
keys (`encoding/json` already sorts map keys). `encoding/json` preserves slice
order, so every slice has a declared canonical order, which is applied once
*before* both hashing and prompt rendering so that the prompt and hash agree:

* bars: timestamp descending (as the prompt shows them);
* news: `published_at` descending, then `news_item_id`;
* positions: ticker, then side;
* similar analyses: score descending, then analysis id;
* lens menus: kept in profile order, because order is meaningful there (the
  profile owns it);
* `missing[]`: block name. Floats are kept as-is (no
rounding at capture). Times are RFC 3339 UTC with nanoseconds.

* `primary`: timeframe, bars exactly as supplied (ts, OHLCV, VWAP), observed-at
  of the freshest bar, and the indicator snapshot.
* `timeframe_contexts[]`: the same, per supplemental timeframe and role.
* `news[]`: `news_item_id`, external id, headline, source, `published_at`,
  `ingested_at` (`created_at`), sentiment label *as supplied*, and selection
  window bounds. Ages are computed from `decision_cutoff`, never `time.Now()`.
* `portfolio`: the `PortfolioContext` as supplied plus its source (`tracker` | `open_trades`).
* `knowledge_context`, `performance_context`: text as supplied plus builder
  version. `similar_analyses`: IDs, scores, threshold and limit.
* `discovery_context`, `execution_profile`, `pullback_context`, `risk_policy`,
  `timing_snapshot`, `momentum_play`, `strategy`/`strategies`.

`missing[]` records every optional block that errored or was disabled, as
`{block, reason}`, for example `{"similar_analyses","embedder_disabled"}` or
`{"timeframe:1h","need 20 candles; got 12"}`. Replay treats a missing block as
missing. It never re-fetches.

### 3.3 Effective configuration

This is an allowlist projection, not the operator config. It holds model
registry entries (provider, model, temperature, max_tokens, timeout_sec,
fallback key), consensus thresholds, depth settings, workflow flags, prompt caps
(`MaxCandlesPerPrompt`, `MaxNewsArticlesPerPrompt`, `HistoryWindowByStrategy`),
risk-policy fields rendered into prompts, and entry-gate toggles. API keys, base
URLs with credentials, broker accounts and notification targets are never
copied. A unit test asserts that the projection type has no field whose name
matches `key|secret|token|password|url`.

### 3.4 Determinism rules (resolution policy v1)

1. **Analyst identity and order.** Results are ordered by configured analyst
   index (registry order), then `provider/model`. Latency is never an ordering
   key. Specialist keeps the canonical role order.
2. **Arbiter ordering.** The permutation is derived from
   `sha256(input_hash || policy_version)` as a seed. It is still anonymising,
   but it is reproducible and persisted as `arbiter_permutation` on the stage
   call.
3. **Tie breaks.** The `majorityVote` cascade is unchanged (HOLD > opposed->HOLD > BUY >
   SELL > SKIP > TRADE > WAIT). The reference analyst for size/stop/target is
   the first winning analyst *in identity order*.
4. **Rounding.** Fast-path averages keep the current precision (confidence 3 dp,
   size 2 dp, price 4 dp). Hashes are computed over the rounded final output.
5. **Quorum/timeouts/retries.** These stay as configured. They are recorded as
   `request_params` and as `attempt` rows, and they are part of
   `effective_config`.
6. **Clock.** `decision_cutoff` is captured once at the start of
   `runAnalysisWithProfile` and threaded to news selection, prompt news ages,
   and timing. Prompt builders must not call `time.Now()`.

### 3.5 Replay modes

* **Recorded-answer replay** (`replay.Recorded(snapshotID)`): loads the snapshot
  and stage calls, feeds `parsed_response`/`error` back through the resolution
  policy and `ValidateFinalLensSelection`, and compares `output_hash`. It makes
  no network calls and no store reads outside the two tables, and it has no
  executor, risk manager or event bus. It is implemented as a pure function over
  the snapshot so that order placement is structurally impossible (it has no
  dependency on `engine` or `execution`).
* **Fresh inference** (`replay.Fresh(snapshotID, providers)`): re-renders prompts
  from `inputs` using `decision_cutoff` as the clock, calls the providers, and
  writes results under a new `replay_runs` row, never into live tables.
  Differences are attributed per stage: per-analyst action/confidence, then
  resolution.

Both refuse a snapshot whose `schema_version`, `policy_version` or
`prompt_version` they do not implement, rather than adapting it silently.

## 4. Incremental implementation

| Step | Change | Gate |
|---|---|---|
| 1 | This document. README correction. | Docs. |
| 2 | Deterministic resolution: identity ordering, seeded and recorded arbiter permutation, `AnalystResult` error serialized as a string, injected cutoff clock for news selection and prompt ages. | Unit tests for latency-independence, fixed permutation, and the error string in `ensemble_results`. |
| 3 | `decision_snapshots` + `decision_stage_calls` migration. Capture in `runAnalysisWithProfile` (including failed and short-circuited attempts). `code_revision` from `debug.ReadBuildInfo`. Exclude from retention. | Integration tests: completed, failed, and partial-ensemble attempts all produce snapshot rows. |
| 4 | `internal/replay` recorded-answer replay plus a CLI subcommand. | Same hash on recorded replay. Changed input or policy gives a distinct identity. |
| 5 | Fresh-inference harness and repeatability report. | Measured separately. Not a CI gate. |

Step 3's snapshot tables are also the **minimal shared contract for #590**: the Jev
shadow evaluator reads `decision_snapshots.inputs` plus analyst stage calls,
writes its own `decision_stage_calls` rows (`stage = 'jev:<question>'`), and
resolves under its own `policy_version`. #590 adapter work does not need steps
4–5.

## 5. Fixture plan

| Fixture | Asserts |
|---|---|
| Changed candle (same ts, revised close) | `input_hash` differs. Recorded replay of the old snapshot is unaffected by the new DB row. |
| Changed news (sentiment rescored, ticker merged) | Snapshot keeps the as-supplied label. `input_hash` differs only for the new decision. |
| Input ordering (news order, map key order, positions order) | Canonicalisation gives an identical `input_hash` *and* an identical rendered prompt. |
| Policy-only change | Same `input_hash`, different `decision_hash`. |
| Crash mid-decision | `pending` row becomes `abandoned` on restart. Replay reports it as incomplete. |
| Latency permutation (same answers, different latencies/arrival order) | Identical final output and `output_hash`. |
| Missing analyst (1 of 3 errors) | Error string persisted, quorum still met, replay identical. With 2 of 3 failed: `outcome=failed`, row still present. |
| Arbiter permutation | Same `input_hash` gives the same permutation. A different policy version gives a different permutation. |
| Retry / specialist fallback | Attempt rows 0 and 1, replay uses the final attempt and records both. |
| Cache collision (two decisions with the same bucketed fingerprint) | Distinct `input_hash`. Replay never consults `analysis_cache`. |
| Policy version bump | Distinct `decision_hash`. Replay under the old version refuses or uses the old implementation. |
| Clock | Prompt rendered at cutoff T and re-rendered at T+5h are byte-identical. |
| Secrets | The effective-config projection contains no credential-shaped fields. |
