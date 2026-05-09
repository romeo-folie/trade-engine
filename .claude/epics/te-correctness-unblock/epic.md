---
name: te-correctness-unblock
status: backlog
created: 2026-05-09T20:01:47Z
updated: 2026-05-09T20:49:15Z
progress: 0%
prd: .claude/prds/te-throughput-discipline.md
github: https://github.com/romeo-folie/trade-engine/issues/310
---

# Epic: te-correctness-unblock

## Overview

Make the trade engine honest about what is open, free up the position budget that has been silently exhausted by stale dev-mode rows, and verify the feedback loop actually closes. This epic lands first because every metric we want to measure in Epic B (BUY-signal volume, win rate, would-have-passed counterfactuals) depends on the gate seeing the right number of open positions and the dashboard showing the right number to the operator.

## Architecture Decisions

- **Trade-row preservation over deletion.** Stale JMIA rows get a new `consolidated` enum value plus a `consolidated_into UUID` pointer. We do not delete history. Reversible: `UPDATE … SET status='open' WHERE consolidated_into=$1`.
- **Distinct (ticker, direction) is the canonical "open position" notion.** All consumers — risk gate, portfolio tracker, canvas — must agree on this denominator. The trade-row count remains available as a secondary informational metric.
- **Reconciler logs, does not auto-correct.** The startup reconciler emits WARN on broker/DB drift. Auto-mutation would risk masking bugs. Mirrors the visibility-first stance taken in PR #297 for ORB.
- **Single PRD, two epics.** This epic is correctness-only. All gate calibration, prompt changes, trailing stops, specialist analysts, and observability counters live in `te-generation-exits`.

## Technical Approach

### Backend

- New migration: adds `consolidated` to the `trade_status` enum, `consolidated_into UUID NULL` column on `trades`, and an index on `(consolidated_into)`.
- One-shot data migration: collapse 8 JMIA rows. Verification step (broker quantity check) runs separately as a Go script or psql query before the migration is committed.
- `internal/engine/engine.go:1272` — count distinct `(ticker, direction)` pairs from `openTrades` instead of `len(openTrades)`. Add `MaxPerTicker int` to `risk.CheckInput`; new check `checkMaxPerTicker` in `internal/risk/manager.go`.
- New: `engine.reconcileBrokerVsDB` runs at engine boot. Compares broker positions to DB open trades and logs WARN entries.
- Audit `internal/feedback.GetPerformanceContext`. Sample one ticker (e.g. NVDA), capture the prompt that gets sent, confirm `performanceContext` is non-empty.

### Frontend

- Portfolio card in canvas: extend the existing `OpenPositions` field to also expose `OpenTradeRows`. Render as `1 (8 rows)` when they diverge, plain `1` when they agree.

### Infrastructure

- No infra changes. Migration runs through the existing `store.Migrate` path.

## Implementation Strategy

- **Phase 1 (sequential):** Verify JMIA broker quantity → run migration → collapse JMIA. This sequence cannot parallelise.
- **Phase 2 (parallel):** Distinct-ticker counting + `max_per_ticker`, startup reconciler, feedback-loop audit, UI position-counter — all touch different files and can run in parallel agents.
- Each task ships as a standalone PR per repository convention. Per-task PR allows backout of any single change.

## Task Breakdown Preview

1. JMIA verification + collapse migration (sequential, blocking).
2. Distinct-ticker counting + per-ticker cap (parallel after #1).
3. Startup broker-vs-DB reconciler (parallel).
4. Feedback-loop audit (parallel).
5. Canvas position-counter visibility (parallel after #2).

## Dependencies

- Migration runner (`store.Migrate`).
- Broker abstraction (`brokerView`) for both the verification step and the reconciler.
- Existing reconciliation pattern from PR #297.

## Success Criteria (Technical)

- After collapse: broker quantity for JMIA == quantity on the canonical trade row; 7 rows have `status='consolidated'` and a non-null `consolidated_into`.
- Risk gate `OpenPositions` value matches portfolio card on all observed tickers, verified continuously for one full trading day post-deploy.
- Startup reconciler emits at most one WARN per ticker per boot; emits zero WARNs once the JMIA collapse has run on production.
- Feedback audit produces a written record (committed to repo or a one-time issue comment) of whether `performanceContext` reaches the prompt.

## Estimated Effort

- 5 tasks, mostly XS/S; one S/M task (collapse migration).
- ~12–18 engineering hours total, single-developer pace.

## Tasks Created

- [ ] #311 - JMIA verification + collapse migration (parallel: false; blocker for #312, #315)
- [ ] #312 - Distinct-ticker position counting + max_per_ticker cap (parallel: true; depends_on #311)
- [ ] #313 - Startup broker-vs-DB reconciler (parallel: true)
- [ ] #314 - Feedback-loop audit (performanceContext flow) (parallel: true)
- [ ] #315 - Canvas position-counter visibility (parallel: true; depends_on #312)

Total tasks: 5
Parallel tasks: 4 (after #311 lands)
Sequential tasks: 1 (#311 must precede #312 and #315)
Estimated total effort: 12–18 hours
