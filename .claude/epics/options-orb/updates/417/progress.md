---
issue: 417
started: 2026-07-12T10:16:03Z
last_sync: 2026-07-12T11:20:00Z
completion: 100%
---
# Issue #417 — Backtest path
## Approach
New internal/backtest/optionsreplay package that REUSES the live functions so parity is structural:
options.Engine.Select (gates) + orb.DecideOptionExit (exit) + orb.ExitPnLM (P&L) + orb.SizeOptionContracts.
Then the RUNNABLE path on top: historical Alpaca option-data I/O + BS-derived greeks + signal assembly + CLI.
## Done
- [x] Export orb.DecideOptionExit + orb.SizeOptionContracts (reused, not reimplemented)
- [x] optionsreplay.Engine.Replay: gate -> size -> walk underlying/option path applying live exit -> SimTrade
- [x] Aggregate metrics (win rate, multiplier P&L, aborts-by-gate, exits-by-reason)
- [x] Parity test: Replay's gate decision == direct options.Engine.Select on identical inputs
- [x] Runnable path (user chose "build now", not defer):
  - [x] HistoricalOptionData seam + as-of context (no lookahead)
  - [x] internal/data.AlpacaOptionsHist: /v2/options/contracts + /v1beta1/options/{quotes,bars} + equity underlying bars
  - [x] BSDelta + HistChainProvider (as-of chain, BS-derived IV/delta since Alpaca serves greeks live-only)
  - [x] BuildIVSeries + in-memory IVHistoryReader (IV-rank seed)
  - [x] AssembleSignals (live ORB range/bracket → breakouts) + Runner (peek-Select → fill option path → Replay)
  - [x] `te backtest orb-options` CLI + docs/options-orb-backtest.md
## Review rounds (all addressed)
- Local #1: path_exhausted metric distortion + unpriced-quote exclusion → Unresolved/Unpriced buckets
- Codex: user-acct P2 (path_exhausted) + bot P1 (stale-across-sessions flatten) + bot P2 (partial-cancel persist)
- Local #2: IV-seed UTC-anchoring bug (would fail-closed every signal on the real CLI path) + swallowed OPRA-403 errors → fixed + UTC regression test
## Honest limits (documented in docs/options-orb-backtest.md)
Alpaca option history needs OPRA entitlement + reaches back ~Feb 2024; option-mid uses the 1m bar close (last-trade proxy); missing quotes → Unpriced, data-exhausted paths → Unresolved, both excluded from win rate/P&L.
