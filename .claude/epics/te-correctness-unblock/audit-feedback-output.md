# Feedback-loop audit — captured output (Issue #314)

**Date:** 2026-05-09
**Branch:** `feat/te-feedback-audit-314`
**Tool:** `cmd/audit-feedback`

## Result: WORKING — performance context flows from DB into the rendered analyst prompt

## DB state at capture time

| Table               | Rows | Last `created_at`              | Last `date` |
|---------------------|------|--------------------------------|-------------|
| `feedback_insights` | 738  | 2026-05-08 18:00:26.634122+00  | 2026-05-08  |
| `feedback_flags`    | 37   | 2026-05-08 18:00:26.644739+00  | 2026-05-08  |

Row distribution by scope/key (top 10):

```
    scope    | scope_key | rows | last_date
-------------+-----------+------+------------
 orb_session | EUR/USD   |  168 | 2026-05-08
 orb_session | GBP/USD   |  162 | 2026-05-08
 orb_session | XAU/USD   |   96 | 2026-05-08
 model       | /         |   93 | 2026-05-08
 instrument  | IBKR      |   81 | 2026-05-08
 instrument  | NVDA      |   69 | 2026-05-08
 system      | arbiter   |   69 | 2026-05-08
```

NVDA most-recent metrics (closed long, +6.33%):

```
    date    |   metric    | value  | time_window | trade_count
------------+-------------+--------+-------------+-------------
 2026-05-08 | avg_pnl_pct | 6.3286 | 30d         |           1
 2026-05-08 | trade_count |      1 | 30d         |           1
 2026-05-08 | win_rate    |      1 | 30d         |           1
```

## Config gate

`internal/config/defaults.go:354` defaults `feedback.enabled = false`, but both
user-controlled configs enable it explicitly:

- `~/.tradeengine/config.yaml`: `feedback.enabled: true`, `feedback.prompt_enrichment: true`
- `./config.yaml`: `feedback.enabled: true`, `feedback.prompt_enrichment: true`

`internal/engine/engine.go:771` — gate is `e.cfg.Feedback.Enabled && e.cfg.Feedback.PromptEnrichment`. Both true in production. Pass-through into `AnalysisRequest.PerformanceContext` confirmed at `engine.go:785`.

## Renderer

`internal/analysis/prompts/stock.go:271-277` writes `### PERFORMANCE CONTEXT`
into the user message body and emits the supplied string. If empty, falls back
to "No performance data available." Confirmed in captured output below.

## Captured output — NVDA

```
=== AUDIT: feedback.GetPerformanceContext ===
ticker:    NVDA
non-empty: true
length:    90 bytes
--- raw performance context ---
- NVDA: 1 trades in last 30 days, 100% win rate, avg +6.3%
- /: 100% win rate on 2 trades
--- end raw performance context ---
```

Excerpt from the rendered user message:

```
### PERFORMANCE CONTEXT
- NVDA: 1 trades in last 30 days, 100% win rate, avg +6.3%
- /: 100% win rate on 2 trades


Respond with ONLY the JSON object.
```

## Captured output — IBKR

```
=== AUDIT: feedback.GetPerformanceContext ===
ticker:    IBKR
non-empty: true
length:    90 bytes
--- raw performance context ---
- IBKR: 1 trades in last 30 days, 100% win rate, avg +5.1%
- /: 100% win rate on 2 trades
--- end raw performance context ---
```

Excerpt from the rendered user message:

```
### PERFORMANCE CONTEXT
- IBKR: 1 trades in last 30 days, 100% win rate, avg +5.1%
- /: 100% win rate on 2 trades
```

## Findings

1. **Loop is closed.** For both tickers with closed trades, `feedback.GetPerformanceContext` returns a non-empty 90-byte block, and that block lands verbatim under `### PERFORMANCE CONTEXT` in the rendered analyst user message.
2. **Reviewer ran on schedule.** Both `feedback_insights` and `feedback_flags` have `created_at = 2026-05-08 18:00:26+00` — exactly the configured `review_time_utc: "18:00"`. The writer side is healthy.
3. **System prompt rules are gated by trade volume.** The system prompt explicitly says "apply only when the ticker line shows at least 5 trades in last 30 days." With only 1 closed trade per ticker today, the analyst sees the data but the strict win-rate ceilings/floors don't bind yet. This is by design — not a bug.

## Open concern (out of scope for this PR)

The `model` scope produces `scope_key = "/"` (e.g. `- /: 100% win rate on 2 trades`) — the `provider/model` pair appears empty when summarising closed trades. The string is harmless to the prompt but is uninformative. Worth a follow-up issue to populate `provider/name` on the trade rows that feed the model-level reviewer.

## How to re-run

```sh
DATABASE_URL=... go run ./cmd/audit-feedback -ticker NVDA
DATABASE_URL=... go run ./cmd/audit-feedback -ticker IBKR
```
