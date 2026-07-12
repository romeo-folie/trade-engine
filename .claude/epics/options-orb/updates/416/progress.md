---
issue: 416
started: 2026-07-12T06:49:57Z
last_sync: 2026-07-12T06:49:57Z
completion: 100%
---

# Issue #416 — Options reconciler + scope-out

## Status: implemented (pending epic PR merge); code review found ZERO issues

## Done
- [x] NEW OCC-keyed options orphan detector: detectOrphanOptionPositions sweep (detection-only) + findOrphanOptionPositions pure fn; managed set = option OCCs with status pending|filled; de-dup + re-arm alerts; wired in tick loop, options-mode-only
- [x] execution.IsOCCSymbol helper (ParseOCCSymbol succeeds)
- [x] store.GetManagedOptionOCCSymbols (pending+filled OCC set)
- [x] scope-out via SQL: GetOpenFilledORBTrades + GetOpenPendingORBTrades exclude asset_class='option' -> scopes time_exit/bar-replay/fill_reconcile/split_orphan at once
- [x] engine reconcileBrokerVsDB skips IsOCCSymbol positions (no drift/adopt of options into legacy trades)
- [x] tests: findOrphanOptionPositions, IsOCCSymbol, GetManagedOptionOCCSymbols + reader-exclusion (real DB), engine skip-option-position
- [x] zero-regression: orb/engine/execution/store suites all green

## Scope-out map (which of the "nine" needed what)
- ORB orphan_detector / fill_reconcile: ALREADY forex-scoped (forexOnlyPositions) -> no change
- split_orphan: gated on splitPlaced (options never split) -> no change
- time_exit / reconcileBarReplayExits: scoped via GetOpenFilledORBTrades SQL exclusion
- engine reconcileBrokerVsDB / AdoptBrokerOrphans: IsOCCSymbol position guard (DB side reads legacy 'trades', naturally option-free)
- cmd statement/broker-verify: manual ops tools, lowest false-alarm risk -> follow-up (documented)

## Notes
- exit_source='reconcile' rows from #415 carry estimated prices; this detector + broker truth heal them.
- Reviewer noted a benign 60s self-healing window (just-closed option briefly out of managed set); detection-only, mirrors existing pattern.
