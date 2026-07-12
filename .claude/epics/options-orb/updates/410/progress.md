---
issue: 410
started: 2026-07-11T17:15:28Z
last_sync: 2026-07-11T17:44:25Z
completion: 100%
---

# Issue #410 — Contract identity foundation

## Status: implemented (pending epic PR merge)

## Done
- [x] Migration 039 (forward-only): option columns (underlying/expiry/strike/right/occ_symbol) + multiplier on orb_trades; asset_class already existed (018); idx_orb_trades_occ_symbol
- [x] execution.OptionContract + OrderRequest.Option + Position.Option; SecType allows "OPT"
- [x] orb.BracketParams.Option (*execution.OptionContract)
- [x] ORBTrade struct fields + CreateORBTrade writes asset_class/contract/multiplier (defaults equity/1) + GetORBTradeByID canonical reader
- [x] Multiplier-aware P&L: ExitPnLM/exitPnLM; exitPnL delegates with multiplier=1 (behavior-preserving)
- [x] Tests: option round-trip + equity-default + not-found (store, real test DB); multiplier P&L + legacy-unchanged (orb)
- [x] go build ./... OK; store+orb+execution suites green (zero regression)
- [x] feature-dev:code-reviewer — no high-confidence issues

## Notes
- Six existing list-readers intentionally unchanged (rely on column defaults). Future consumers of option fields MUST read via GetORBTradeByID, not list-readers (per code-review landmine note; documented on the function).
