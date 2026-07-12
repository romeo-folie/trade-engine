---
issue: 418
started: 2026-07-12T07:02:18Z
last_sync: 2026-07-12T07:02:18Z
completion: 100%
---

# Issue #418 — Config, TUI & wiring (the on-switch)

## Status: implemented (pending epic PR merge); code review found ZERO issues

## Done
- [x] OptionsORBConfig (enabled, underlyings, etf_underlyings, premium/max-loss, gate thresholds, time_exit, iv window) + top-level Config.OptionsORB; config.example.yaml section; drift guard green
- [x] Concrete providers: marketDataPricer (FetchQuote last/mid), chainQuoter (GetOptionChain OCC mid)
- [x] SetupOptionsORB: builds options.Engine + OptionsRouter + OptionExitManager from config; nil earnings (ETF-only v1); no-op unless enabled + Alpaca broker
- [x] Wired into cmd/te/run.go equities/newyork scheduler (type-assert broker -> *AlpacaBroker; nil/PaperExecutor skip safely)
- [x] TUI: GetORBTrades reader carries asset_class/occ_symbol/right/strike; ORBTradeRow.Label() shows "SPY C551" for options, plain ticker for equities (zero visual change)
- [x] docs/options-orb.md (prereqs: Level 2 + OPRA feed; how it works; IV cold-start; universe/earnings; config; paper-first)
- [x] tests: config drift, GetORBTrades option identity (real DB); full build + config/orb/store/tui suites green
- [x] code review: zero issues; added the one coverage-gap test it flagged (GetORBTrades option roundtrip)

## v1 enablement
- options_orb default OFF. To enable paper: options_orb.enabled=true, markets.stocks.option_feed=opra, underlyings=[SPY,QQQ].
- Single names (NVDA/TSLA) fail closed at the earnings gate until an earnings provider is wired.

## Remaining for a live run (not #418)
- Concrete Alpaca historical-quote provider + scheduler job for the IV bounded backfill/daily snapshot (deferred from #413) — needed so the IV-Rank gate has day-one data.
- #417 backtest (validation).
