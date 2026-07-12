---
issue: 415
started: 2026-07-12T06:21:42Z
last_sync: 2026-07-12T06:21:42Z
completion: 100%
---

# Issue #415 — App-managed exits

## Status: implemented (pending epic PR merge)

## Done
- [x] decideOptionExit (pure): stop/target on UNDERLYING (correct for buy->call and sell->put), IV-crush time exit, EOD flatten; priority stop/target>time>eod; session-ending ALWAYS flattens
- [x] OptionExitManager.plan (fetch underlying+option price, decide) / closeOption (market SELL-to-close, price fallback: fill->mid->entry)
- [x] enforceOptionExits scheduler sweep, wired in tick loop after enforceTimeExits; nil-manager no-op (zero regression); 15s throttle
- [x] EOD flatten at 15:50 ET (optionsSessionEnding, nyLoc); intraday-only, no overnight
- [x] realized P&L via ExitPnLM("buy",...) — long option regardless of ORB direction; exit_type stop/tp/time_exit/eod
- [x] GetOpenFilledOptionTrades store reader (option cols + status=filled AND asset_class=option); DB round-trip test
- [x] tests: decideOptionExit (11 cases), plan, closeOption fallback, estimateExitPrice, optionsSessionEnding; store reader (real DB)

## Code review — 1 CRITICAL bug found & fixed
- (88) double-close: if the market SELL succeeded but CloseORBTrade failed, the row stayed 'filled' and the next sweep re-sold -> naked short / infinite retry. Fix: heldOptionSymbols(GetPositions) flat-guard before selling (mirrors time_exit.go); already-flat -> record-only heal (exit_source='reconcile'), never a second sell. In-flight window: positions-read failure proceeds with protective exit (documented).

## Notes
- Concrete OptionQuoter/UnderlyingPricer impls (over marketData + chain) + manager construction from config -> #418.
- exit_source 'reconcile' rows carry an estimated price; #416 broker-truth reconcile heals the exact fill.
