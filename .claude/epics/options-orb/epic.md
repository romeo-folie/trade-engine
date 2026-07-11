---
name: options-orb
status: backlog
created: 2026-07-11T17:03:37Z
updated: 2026-07-11T17:21:10Z
progress: 0%
prd: .claude/prds/options-orb.md
github: https://github.com/romeo-folie/trade-engine/issues/409
---

# Epic: options-orb

## Overview

Add a **post-signal options execution router** to the ORB engine. When a breakout confirms on an options-mode underlying, intercept the existing `BracketParams` signal and execute it as a long single-leg equity option (call on long, put on short) via Alpaca, instead of shares. The strategy is unchanged — this is purely a new last-mile execution vehicle plus the durable layer options require (contract identity, gates, app-managed exits, reconciliation).

The work concentrates in two proven-to-be-hard areas the spikes measured — **contract identity in the schema** and **the reconciler layer** — and reuses everything upstream: ORB signal production, the underlying price feed, the Alpaca HTTP plumbing, the forex `StopOrderSimulator` OCO pattern, and the `orbreplay` backtest engine.

## Architecture Decisions

1. **Router, not scheduler.** Options-ORB taps the single centralized signal (`BracketParams`, produced at `internal/orb/scheduler.go:1205-1360`). Options-mode underlyings route to the options path; all others fall through unchanged. No parallel scheduler, no duplicated session/detection logic. Default off, opt-in per underlying.
2. **Extend the `SecType` seam with a contract-identity payload.** `SecType="OPT"` routes the order (IBKR's `secType` branching is the template), but an option can't be identified by a string alone — `OrderRequest`/`Position`/`BracketParams` gain an optional `OptionContract{underlying, expiry, strike, right, occ_symbol, multiplier}`.
3. **Schema follows the `018_forex_orb` precedent.** Add `asset_class='option'` + contract columns to `orb_trades`/`orb_sessions`, exactly as forex added `asset_class`+`conid`. `qty` and P&L honor the ×100 multiplier on the options path.
4. **App-managed exits are mandatory** (Alpaca rejects bracket/OCO on options). Exit triggers read the **underlying** (stop/target crossings) off the feed ORB already streams; the action is a market-close on the **option**. Reuse the dual-sided forex OCO / `StopOrderSimulator` machinery (PR #283).
5. **Gates fail closed, with a bounded-derive cold-start fix.** IV-Rank/spread gates abort on missing input (inverting the ORB fail-open default). A bounded ~60–90-day up-front BS IV derivation seeds the IV-Rank series so the fail-closed gate has a real value on day one.
6. **New OCC-keyed reconciler + scope the nine out.** All options reconciliation lives in one new component; the nine existing ticker-keyed matchers get `asset_class != 'option'` guards. Mirrors the existing forex-only scoping; zero regression risk to proven code.
7. **Alpaca equity options only; OPRA real-time feed required.** Real-time is mandatory even in paper (decisions are made live at the open). IBKR/index options are explicitly out of v1.

## Technical Approach

### Backend Services (Go)

- **Store / schema** (`internal/store`, `migrations/`): new migration adding `asset_class` + `underlying,expiry,strike,right,occ_symbol,multiplier` to `orb_trades`/`orb_sessions`; `ORBTrade` struct fields; multiplier-aware P&L. Foundational.
- **Alpaca options adapter** (`internal/execution/alpaca_broker.go` + new options client): chain snapshots (`/v1beta1/options/snapshots`), contract resolution (`/v2/options/contracts`), option order path on `/v2/orders` with OCC symbol + contract-qty semantics, option-aware `GetPositions` (`asset_class:"us_option"`). Reuse existing `do`/auth plumbing.
- **Strike-selection engine** (new `internal/options` package): deterministic, fail-closed gate pipeline — IV-Rank → DTE(0<..≤5) → delta band → spread% — plus the single-name earnings-blackout gate. Unit-tested against numeric fixtures.
- **IV-Rank pipeline** (`internal/options`): bounded up-front BS-derivation of a ~60–90d IV series per underlying, a daily live-chain IV snapshot job to grow it, IV-Rank computation, and a parallel extension toward a trailing year.
- **Execution router** (`internal/orb/scheduler.go` interception + `internal/options`): consume `BracketParams`, run gates, size by premium (`floor(budget/(mid×100))`), place limit-at-mid with a fill/reprice policy, persist the trade with contract identity.
- **App-managed exits** (`internal/options` + reuse `internal/execution/*StopOrderSimulator*` pattern): underlying-trigger close watcher, IV-crush time exit, EOD flatten, OCO sibling handling.
- **Options reconciler** (`internal/orb` new file): OCC↔underlying map, broker-vs-DB match on OCC symbol, orphan alerts; plus `asset_class != 'option'` guards on the nine existing matchers.
- **Backtest** (`internal/backtest/orbreplay` options variant): replay the same gate functions against Alpaca historical quotes with BS-derived IV/delta; parity-per-gate.

### Frontend Components (TUI)

- Options trades surfaced in the existing TUI positions/trades views with contract identity (underlying, strike, expiry, C/P) and multiplier-correct P&L. Gate-evaluation logging (pass + abort with values) for observability. No web-UI work in v1.

### Infrastructure

- Alpaca **Level 2 options approval** + **Algo Trader Plus (OPRA real-time)** subscription — provisioned by the live-paper phase; new secret/config wired.
- Config surface: per-underlying options-mode opt-in, gate thresholds (IV-Rank ceiling, delta band, spread ceiling, DTE cap), earnings-blackout window, premium budget, time-exit window. All default-off / safe defaults.

## Implementation Strategy

Build the durable foundation first (contract identity), fan out the independent subsystems (gates, IV pipeline, Alpaca client, reconciler) in parallel against interfaces/fixtures, then wire the router and exits, and validate paper-first before any live enablement. Every gate has a backtest twin (parity-per-guard, matching the ORB roadmap discipline). Ship default-off; enable SPY+QQQ in paper first, then NVDA+TSLA.

## Task Breakdown Preview

Estimated 9 tasks (critical path anchored on Task 1):

- [ ] **1. Contract identity foundation** — migration (`asset_class`+contract cols+multiplier), `ORBTrade` struct, `OptionContract` on `OrderRequest`/`Position`/`BracketParams`, multiplier-aware P&L. *Blocks 2(integration),5,6,7,9.* `depends_on: []`
- [ ] **2. Alpaca options adapter** — options client (chain/contracts/orders/positions), OCC symbol construction, `SecType="OPT"` order path. *Client part parallel; broker integration after 1.* `depends_on: [1]` (client scaffolding can start immediately)
- [ ] **3. Strike-selection gate engine** — 4 gates + earnings-blackout, fail-closed, unit-tested. *Parallel; built against a chain-data interface.* `depends_on: []`
- [ ] **4. IV-Rank pipeline** — bounded up-front BS derivation + daily snapshot job + IV-Rank compute + parallel extension. Feeds Task 3. *Parallel, start early.* `depends_on: []`
- [ ] **5. Execution router** — tap `BracketParams`, run gates, premium sizing, limit-at-mid + fill/reprice, persist with identity. `depends_on: [1,2,3]`
- [ ] **6. App-managed exits** — underlying-trigger close, IV-crush time exit, EOD flatten, OCO semantics. `depends_on: [1,5]`
- [ ] **7. Options reconciler + scope-out** — OCC-keyed reconciler + OCC→underlying map + orphan alerts + `asset_class!='option'` guards on the nine. `depends_on: [1]`
- [ ] **8. Backtest path** — orbreplay options variant, gates on historical quotes with BS-derived IV/delta, parity tests. `depends_on: [3,4]`
- [ ] **9. Config, TUI & observability** — options-mode config + thresholds, TUI surfacing, gate-evaluation logging. `depends_on: [1]` (config can start early)

## Dependencies

**External:** Alpaca Level 2 approval + Algo Trader Plus (OPRA) subscription; Alpaca options APIs (snapshots, contracts, orders, positions, historical quotes post-Feb-2024).

**Internal (reuse):** ORB signal `BracketParams` (`internal/orb/executor.go:22-30`) + placement site (`scheduler.go:1205-1360`); migration-018 `asset_class` precedent; forex `StopOrderSimulator` OCO (PR #283); Alpaca HTTP/auth plumbing + IBKR `secType` template; `orbreplay` backtest engine; the nine reconcilers to guard (`internal/engine/reconcile.go`, `internal/orb/orphan_detector.go`, `internal/orb/fill_reconcile.go`, `cmd/orb-reconcile-statement`).

**Critical path:** 1 → 5 → 6. Tasks 3, 4 run parallel from the start; 2, 7, 9 unlock after 1; 8 after 3+4.

**Parallel waves:**
- **Wave 1:** 1 (identity), 2-client, 3 (gates), 4 (IV pipeline).
- **Wave 2 (after 1):** 2-integration, 7 (reconciler), 9 (config/TUI).
- **Wave 3 (after 1,2,3):** 5 (router); 8 (backtest) once 3+4 land.
- **Wave 4 (after 5):** 6 (exits).

## Success Criteria (Technical)

- Existing ORB test suite passes unchanged; non-options underlyings execute byte-identically (zero regression).
- 100% of paper option trades carry complete contract identity and reconcile to Alpaca truth with zero unexplained orphans over a 2-week forward test.
- Every executed trade passed all gates; every abort logged the failing gate + value; zero market orders on the options path (all limit-at-mid).
- No option position held past market close; no single-trade loss exceeds the premium budget.
- Backtest gate pipeline produces the same accept/abort decision as the live pipeline on identical inputs (parity test green).
- Real-money enablement remains blocked until the paper forward test meets correctness, gate-efficacy, and risk-containment criteria.

## Estimated Effort

- **Overall:** ~3–4 focused weeks across parallel streams; critical path (1 → 5 → 6) is the pacing constraint.
- **Heaviest tasks:** 1 (identity ripple), 4 (BS/IV pipeline), 7 (reconciler correctness).
- **Lightest:** 9 (config/TUI), 3 (self-contained pure logic once the chain interface is fixed).
- **Prerequisite lead time:** Alpaca Level 2 approval + OPRA subscription should be requested early (external approval latency), even though it's only strictly needed by Wave 3/4 live-paper work.

## Tasks Created
- [ ] 001.md - Contract identity foundation (parallel: true)
- [ ] 002.md - Alpaca options adapter (parallel: true)
- [ ] 003.md - Strike-selection gate engine (parallel: true)
- [ ] 004.md - IV-Rank pipeline (parallel: true)
- [ ] 005.md - Execution router (parallel: false)
- [ ] 006.md - App-managed exits (parallel: false)
- [ ] 007.md - Options reconciler + scope-out (parallel: true)
- [ ] 008.md - Backtest path (parallel: true)
- [ ] 009.md - Config, TUI & observability (parallel: true)

Total tasks: 9
Parallel tasks: 7
Sequential tasks: 2
Estimated total effort: 134 hours
