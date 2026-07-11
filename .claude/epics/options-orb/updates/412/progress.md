---
issue: 412
started: 2026-07-11T18:34:25Z
last_sync: 2026-07-11T18:34:25Z
completion: 100%
---

# Issue #412 — Strike-selection gate engine

## Status: implemented (pending epic PR merge)

## Done
- [x] New package internal/options; fail-closed gate pipeline (Engine.Select -> Decision)
- [x] Gate 0 earnings blackout (single names; ETFs exempt; nil provider => abort; known-within-window => abort; none/outside => pass)
- [x] Gate 1 IV rank < ceiling (default 70); unavailable => abort (fail closed)
- [x] Gate 2 expiry 0<DTE<=MaxDTE (default 5), excludes 0DTE; provider filter + in-code recheck agree
- [x] Gate 3 delta band |Δ| in [.50,.60], closest to mid; null delta skipped; none => abort
- [x] Gate 4 spread (ask-bid)/mid < ceiling% (default 5); crossed quote => abort
- [x] direction->right (buy=call, sell=put); Decision carries observability values + deciding gate
- [x] ChainProvider/IVRankProvider/EarningsProvider interfaces (impls: #411 chain; #413 iv/earnings)
- [x] DefaultGateConfig + withDefaults
- [x] 15 unit tests incl. every fail-closed path; code-review found crossed-quote accept (fixed)

## Notes
- Spread gate checks the best-delta contract then its spread (can over-reject; intentional v1). Not a wrongful-accept.
- Config thresholds surfaced in #418; earnings/iv-rank concrete providers in #413.
