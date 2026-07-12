---
issue: 417
started: 2026-07-12T10:16:03Z
last_sync: 2026-07-12T10:16:03Z
completion: 0%
---
# Issue #417 — Backtest path
## Approach
Green-lit after 7 Codex rounds + 2 manual rounds + local review all clean (cdc2a4a).
New internal/backtest/optionsreplay package that REUSES the live functions so parity is structural:
options.Engine.Select (gates) + orb.DecideOptionExit (exit) + orb.ExitPnLM (P&L) + orb.SizeOptionContracts.
## Plan
- [ ] Export orb.DecideOptionExit + orb.SizeOptionContracts (reuse in backtest)
- [ ] optionsreplay.Engine.Replay: gate -> size -> walk underlying/option path applying live exit logic -> SimTrade
- [ ] Aggregate metrics (win rate, multiplier P&L, aborts-by-gate, exits-by-reason)
- [ ] Parity test: Replay's gate decision == direct options.Engine.Select on identical inputs
- [ ] Deferred (documented): concrete Alpaca historical chain reconstruction + BS-derived IV series (the I/O; same deferral as #413)
