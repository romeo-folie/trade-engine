# Execution Status — te-generation-exits

Wave plan:
- **Wave 1 (parallel, dispatched 2026-05-10):** #317, #318, #319, #320, #321, #322, #324
- **Wave 2 (queued, depends on #317):** #325
- **Wave 3 (queued, depends on #317 AND not concurrent with #318):** #323

## Wave 1 — COMPLETE (7 PRs merged 2026-05-10)

- **#317** Gate calibration — PR #339 merged
- **#318** Prompt tightening + first-touch — PR #334 merged
- **#319** SELL ownership short-circuit — PR #337 merged
- **#320** Per-trade max-$ at risk cap — PR #335 merged
- **#321** Multi-timeframe arbiter — PR #336 merged
- **#322** Server-side trailing stop manager — PR #340 merged
- **#324** Pyramiding into winners — PR #338 merged

## Wave 2 + 3 — Active (parallel dispatch 2026-05-10)

- **#325** Observability (counterfactual logging + daily counters) — branch `feat/te-observability-325`
- **#323** Specialist analysts — branch `feat/te-specialist-analysts-323`

## Conflict watch

- **#317, #319, #320, #324** all touch `internal/risk/manager.go` — first to merge wins; subsequent PRs rebase.
- **#319, #324** both touch the engine signal handler in `internal/engine/engine.go` — likely independent decision branches but watch for merge conflicts.
- **#321, #319, #324** all may touch `internal/engine/engine.go` paths — minor overlap.
