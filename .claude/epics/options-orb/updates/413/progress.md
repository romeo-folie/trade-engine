---
issue: 413
started: 2026-07-12T05:25:45Z
last_sync: 2026-07-12T05:25:45Z
completion: 90%
---

# Issue #413 — IV-Rank pipeline

## Status: core implemented (pending epic PR merge); one deferred I/O piece

## Done
- [x] Black-Scholes price + ImpliedVol (bisection); call/put no-arb bounds; tested vs known values, round-trip, non-invertible
- [x] IV-Rank math (ivRankOf) + IVRankService implementing options.IVRankProvider (#412 consumer)
- [x] Cold-start resolution: MinSamples guard -> ok=false below the bounded seed (fail closed); flat window -> unavailable
- [x] migration 040 option_iv_history + store UpsertOptionIV/GetOptionIVHistory (source-priority guard: derived never clobbers live)
- [x] Forward path RecordLiveATMIV (reads live-chain IV via #411, no BS)
- [x] Bounded backfill BackfillIVHistory over HistoricalQuoteProvider iface + BS; recovers known IV, skips non-invertible
- [x] Tests: BS (incl put ITM bound), rank cold-start/happy, atm selection, forward path, backfill; store round-trip + source guard (real DB)
- [x] code-review: fixed put upper-bound bug + upsert source-priority (both flagged)

## Deferred (thin I/O; interface tested)
- [ ] Concrete Alpaca HistoricalQuoteProvider: fetch historical option bars + underlying closes, per-day ATM strike selection (needs live-API validation; the hard "which strikes existed historically" piece). Orchestration + BS are done and tested against a fake.
- [ ] Scheduler wiring for the daily RecordLiveATMIV snapshot + one-time backfill -> belongs with #418 (config/scheduling).
