---
issue: 414
started: 2026-07-12T05:55:50Z
last_sync: 2026-07-12T05:55:50Z
completion: 100%
---

# Issue #414 — Execution router

## Status: implemented (pending epic PR merge)

## Done
- [x] OptionsRouter.Route: gate (#412) -> size by premium -> place limit-at-mid via #411 OPT path
- [x] Long-only invariant: order Side ALWAYS buy (direction picks call vs put via gate)
- [x] Sizing: contracts=floor(premiumBudget/(mid*multiplier)); max-loss cap; <1 -> not placed; div-by-zero guarded
- [x] Handles() options-mode set (nil-router safe); scheduler hook in per-side loop, inert by default
- [x] routeOptionsEntry persists asset_class='option' + full identity + UNDERLYING stop/target (for #415); tracks; publishes
- [x] SetOptionsRouter wiring hook (constructed from config in #418)
- [x] 8 router unit tests (happy/sizing/cap/budget/gate-abort/sell-buys-put/broker-error/handles + order-id surfaced)
- [x] zero-regression: full orb suite green with nil router

## Code review — 2 real bugs found & fixed
- CRITICAL (95): options branch continued without setting ids[i] -> orderIDs[ticker]=["",""] -> re-entry guard never fired -> runaway duplicate orders every 30s tick. Fix: routeOptionsEntry returns orderID; loop sets ids[i].
- IMPORTANT (80): store failure still wrote trackedTrades with empty dbID -> naked unmanaged option (no broker bracket). Fix: on persist error, skip tracking, still return orderID (no duplicate), log ERROR loudly for #416 reconciler.

## Notes
- Active limit reprice-if-unfilled deferred to #415's management loop; DAY TIF prevents overnight dangling.
- A DB-backed orb-package tick integration test would need an orb TestMain (no other orb test uses DB); router logic is fully unit-tested, scheduler wiring mirrors proven placeAndRecord.
