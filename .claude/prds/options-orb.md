---
name: options-orb
description: Post-signal execution router that runs the existing ORB strategy on curated mega-cap underlyings but executes via long single-leg equity options (buy calls/puts) on Alpaca
status: backlog
created: 2026-07-11T14:16:54Z
---

# PRD: options-orb

## Executive Summary

Options-ORB adds a new **execution vehicle** to the engine's most mature strategy. Today the Opening Range Breakout (ORB) engine detects breakouts on underlyings and executes them as shares (Alpaca), forex/metals CFDs, or micro futures (IBKR). This feature intercepts the *same* proven ORB signal and, for a curated set of highly liquid mega-cap underlyings, executes it by **buying a single-leg equity option** (a call on a long breakout, a put on a short breakout) on Alpaca instead of shares.

The strategic thesis: the strategy is already validated — we are not inventing a new way to decide *direction*. We are adding a last-mile router that (a) picks the right contract via deterministic mathematical gates, (b) sizes by premium-at-risk, (c) enters with a limit order at mid-price, and (d) manages the exit against the *underlying's* price levels because options carry no native bracket at Alpaca. This buys leveraged, strictly-defined-risk (max loss = premium) exposure to the intraday breakouts the engine already trades, while dodging the two things that make options dangerous — assignment and undefined risk — by staying long-only and intraday-only.

This PRD scopes **v1**: single-leg, long-only, intraday-close, Alpaca equity options, driven by the existing ORB signal.

## Problem Statement

The ORB engine currently expresses every breakout as a linear position. On a mega-cap underlying, a share position requires large capital for meaningful exposure and offers no leverage or convexity. Traders running ORB on liquid names (NVDA, TSLA, AAPL, AMD, etc.) commonly execute via **weekly options** instead: a small intraday move in the underlying produces an outsized move in a near-ATM option, with risk strictly capped at the premium paid.

The engine cannot do this today. It is built end-to-end on a `ticker string = one instrument` assumption:
- Trades/orders/positions are keyed on a bare `ticker` column with no notion of contract, expiry, strike, right, or multiplier.
- Nine broker-vs-DB reconcilers match positions by ticker (or ticker+side); an Alpaca option position reports its OCC symbol (e.g. `AAPL240119C00150000`) and would never reconcile against an underlying-keyed row.
- Sizing, P&L, and dedup counting all assume shares with a ×1 multiplier.
- The Alpaca broker adapter only trades stocks and ignores `SecType` entirely.

So the engine can find the breakout but cannot pull the trigger in the options market. This feature closes that gap for a deliberately narrow, defined-risk slice.

## User Stories

### US-1 — Route an ORB breakout to an option
**As** the trade engine operator, **I want** a confirmed ORB breakout on a configured options-mode underlying to be executed as a long call/put **so that** I gain leveraged, defined-risk exposure to the move instead of a share position.
**Acceptance criteria:**
- When ORB confirms a **long** breakout on an options-mode underlying, the engine buys a **call**; on a **short** breakout, it buys a **put**.
- The traded contract is selected by the gate pipeline (US-2), not hardcoded.
- The trade is recorded with full contract identity (underlying, expiry, strike, right, OCC symbol, multiplier) and is visible in the TUI.
- Underlyings **not** in options mode continue to execute exactly as today (shares/forex/futures) — zero behavioral change on the existing path.

### US-2 — Select the contract via mathematical gates
**As** the operator, **I want** contract selection driven by explicit, testable numeric gates **so that** the engine avoids the specific ways options lose money (IV crush, slippage) and every decision is auditable.
**Acceptance criteria:** the pipeline runs in order and **aborts the trade** (fail-closed) if any gate fails or its input cannot be computed:
1. **IV-Rank gate** — proceed only if the underlying's IV Rank < configurable ceiling (default 70%).
2. **Expiry gate** — filter the chain to `0 < DTE ≤ 5` (nearest weekly).
3. **Delta gate** — select the contract whose delta is in a configurable band (default 0.50–0.60, near-ATM).
4. **Spread gate** — proceed only if the selected contract's bid-ask spread as a % of mid is below a configurable ceiling (default 5%).
- Each gate's inputs, values, and pass/fail are logged for every evaluation (including aborts).
- Gate thresholds are configurable and have safe defaults.

### US-3 — Enter without paying the spread
**As** the operator, **I want** entries placed as **limit orders at the computed mid-price** **so that** I don't bleed edge to wide opening spreads via market orders.
**Acceptance criteria:**
- Entry is a limit order at the mid of the selected contract's bid/ask.
- If the limit does not fill within a configurable window, the order is repriced or cancelled per a defined policy (no silently dangling working orders).
- Position size = `floor(premium_budget / (mid × multiplier))`, capped so max loss (premium × contracts × 100) never exceeds the configured per-trade risk.

### US-4 — Manage the exit against the underlying
**As** the operator, **I want** the option exit driven by the **underlying's** ORB stop/target plus an IV-crush time-stop **so that** exits track the strategy's real levels even though Alpaca offers no bracket on options.
**Acceptance criteria:**
- The engine monitors the underlying (reusing the feed ORB already streams). When the underlying crosses the ORB **stop** or **target**, it market-closes the option.
- An **IV-crush time exit** closes the position if it is not profitable within a configurable number of minutes.
- All positions are flattened before market close (no overnight hold, no assignment exposure).
- Exit legs behave as app-managed OCO (firing one cancels/supersedes the other); no orphaned working orders remain.

### US-5 — Reconcile option positions against broker truth
**As** the operator, **I want** option positions reconciled to Alpaca truth **so that** the engine never silently diverges from the broker (the recurring failure class in this codebase).
**Acceptance criteria:**
- A dedicated options reconciler matches broker option positions to DB rows by **OCC symbol** (with an OCC→underlying reverse map).
- The nine existing ticker-keyed reconcilers are scoped to **exclude** option positions so they do not false-alarm.
- Any broker option position with no explaining DB row raises an alert (detection parity with the existing orphan detectors).

### US-6 — Validate before real money
**As** the operator, **I want** to forward-test in paper and backtest on Alpaca history **so that** I trust the router before it trades live capital.
**Acceptance criteria:**
- The full path runs end-to-end in Alpaca **paper** (place, fill, positions, P&L, exits).
- IV Rank is available on day one via the bounded up-front derivation, then grows via daily live-chain snapshots — so paper trading is neither blocked on a full historical derivation nor dead on cold-start.
- A backtest path replays the gates against Alpaca **historical option quotes** (post-Feb-2024), deriving IV/delta via Black-Scholes, mirroring the live gate logic (parity-per-gate, matching the existing ORB backtest discipline).

## Functional Requirements

### FR-1 Instrument identity (foundational)
- Extend the ORB trade schema with option contract identity, following the `018_forex_orb` precedent (`asset_class` + new columns): `asset_class='option'`, `underlying`, `expiry`, `strike`, `right` (C/P), `occ_symbol`, `multiplier`.
- Carry contract identity through `OrderRequest`, `Position`, and `BracketParams` (currently ticker-only).
- Fix `qty` and P&L math to honor the option multiplier (×100), including the `orb_trades.qty INT` column and every `qty × price` computation on the options path.

### FR-2 Signal interception (router)
- Tap the ORB signal at its single centralized point (`BracketParams`, produced at `internal/orb/scheduler.go:1205-1360`).
- When the underlying is configured options-mode, route `BracketParams` to the options execution path; otherwise fall through to the existing bracket path unchanged.
- Options mode is opt-in per underlying via config; default off.

### FR-3 Strike-selection engine
- Implement the four gates of US-2 as a deterministic, unit-tested pipeline, **fail-closed** on any gate failure or missing input.
- Resolve the tradable OCC contract via Alpaca's contracts endpoint from (underlying, expiry, strike, right).
- Read live chain snapshots (greeks + IV) from the Alpaca real-time (OPRA) feed.
- IV Rank: seed the ranking series with a **bounded up-front Black-Scholes derivation** (~60–90 trading days) so the fail-closed gate has a real value from day one, then keep the series growing via the daily live snapshot (FR-7) and extend the derivation toward a full trailing year in parallel. (Resolves the cold-start-vs-fail-closed conflict; see NFR.)
- **Earnings-blackout gate (single names only):** skip a single-name underlying within a configurable window around its scheduled earnings report. Index ETFs (SPY/QQQ) are exempt (no earnings). This is a distinct, sharper IV-crush guard than the general IV-Rank gate.

### FR-4 Options execution on Alpaca
- Extend the Alpaca broker adapter with an option order path: OCC-symbol order formatting, contract-qty semantics, and option-aware position reads (`asset_class:"us_option"`). Reuse existing HTTP/auth plumbing; branch on `SecType=="OPT"` (IBKR's `secType` branching is the template).
- Entry: limit order at mid with the fill/reprice policy of US-3.

### FR-5 App-managed exits
- Underlying-triggered close: watch the underlying; on stop/target cross, market-close the option.
- IV-crush time exit: close if not profitable within the configured window.
- End-of-session flatten for any still-open option position.
- App-managed OCO semantics; no orphaned working orders.

### FR-6 Reconciliation & observability
- New OCC-keyed options reconciler with OCC→underlying reverse mapping.
- `asset_class != 'option'` guards on the nine existing reconcilers.
- Orphan detection for unexplained broker option positions.
- TUI surfacing of option trades with contract identity and option P&L.

### FR-7 Validation harness
- Paper end-to-end path (FR-2 through FR-6 exercised in Alpaca paper).
- Backtest path replaying gates on Alpaca historical quotes with BS-derived IV/delta, parity-per-gate with the live pipeline.
- A **bounded up-front IV derivation** (~60–90 trading days per underlying) to seed the IV-Rank series, a **daily live-chain IV snapshot job** to grow it forward, and a parallel extension toward a full trailing-year series.

## Non-Functional Requirements

- **Defined risk by construction:** long-only, single-leg, intraday-close. Max loss per trade = premium paid. No margin, no assignment, no naked/short legs in v1.
- **Fail-closed on risk gates:** unlike the ORB entry-quality filters (which fail *open*), the IV-Rank and spread gates must fail *closed* — a missing input means the engine is blind to the exact risk the gate guards, so it must not trade. This inversion must be explicit and tested.
- **Cold-start must not deadlock fail-closed:** because IV Rank is fail-closed, an uncomputable IV series would abort every trade indefinitely. The bounded up-front derivation (FR-3) guarantees the gate has a real value on day one, so paper trading can produce trades immediately without weakening fail-closed.
- **Zero regression on existing paths:** underlyings not in options mode, and all forex/futures/equity flows, behave identically to today. Options mode defaults off.
- **Latency:** strike selection and order placement must complete fast enough to act on the opening move; the real-time OPRA feed is required (the free 15-minute-delayed feed is unusable for this strategy).
- **Testability:** every gate is unit-tested with numeric fixtures; the backtest mirrors live gate logic per-gate (the existing ORB "parity is per-guard" discipline).
- **Observability:** every gate evaluation (pass and abort) is logged with inputs and values; every option trade is reconciled to broker truth.

## Success Criteria

- **Correctness:** In paper, 100% of option trades carry complete contract identity and reconcile to Alpaca truth with zero unexplained orphans over a 2-week forward test.
- **Gate efficacy:** Every executed trade passed all four gates; every abort logged the failing gate and its value. Zero market orders sent on the options path (all entries are limit-at-mid).
- **Risk containment:** No option position is ever held past market close in the forward test. No realized loss on any single trade exceeds the configured premium budget.
- **No regression:** The existing ORB test suite passes unchanged; non-options underlyings show byte-identical execution behavior.
- **Backtest parity:** The backtest gate pipeline produces the same accept/abort decision as the live pipeline on identical chain inputs (parity test).
- **Validation gate to live:** Real-money enablement is blocked until the paper forward test meets the correctness, gate-efficacy, and risk-containment criteria above.

## Constraints & Assumptions

**Broker (Alpaca) — confirmed constraints:**
- **Level 2 approval** is required and sufficient (buy calls/puts). Confirmed supported.
- **No native bracket/OCO on options** — Alpaca rejects complex orders on options. Exits **must** be app-managed. (This forces the FR-5 design.)
- **Real-time OPRA feed (~$99/mo Algo Trader Plus) is mandatory.** The free feed is 15-minute delayed and unusable for an open-momentum strategy. This is a hard prerequisite (spend + secret to provision).
- **Historical options data starts Feb 2024 and contains no historical greeks/IV.** Backtests must derive IV/delta via Black-Scholes from historical quotes + underlying. As of 2026, ~2.4y of history exists, enough for a trailing-1-year IV-Rank lookback.
- **0DTE greeks are intermittently null** — the `DTE ≤ 5` gate already favors weeklies; the delta gate must handle null greeks gracefully (treat as gate-fail → abort).
- Contract symbols are **OCC 21-char** format; tradable symbols resolved via `/v2/options/contracts`.
- **Paper trading supports options end-to-end**, including fills and P&L.

**Assumptions:**
- The existing ORB signal (`BracketParams`) is a sufficient and correct entry trigger for the options path; we reuse it verbatim and do not re-tune direction logic.
- A small, curated, highly liquid underlying universe (index ETFs + a few mega-caps) keeps option spreads tight and fills reliable, sidestepping the gap-scanner universe problems seen on the equities path. Index ETFs are earnings-free, isolating the execution machinery from single-name IV shocks during the first forward test.
- The underlying price feed the ORB engine already streams is sufficient to drive underlying-level exits.

**Confirmed decisions (2026-07-11):**
- **Underlying universe:** stage in as **SPY + QQQ first** (ultra-liquid, earnings-free index ETFs — prove the execution machinery), then fast-follow with **NVDA + TSLA** (with an earnings-blackout rule). Gives the epic a "prove on 2, scale to 4" shape.
- **OPRA feed:** **approved**; provision the ~$99/mo Algo Trader Plus subscription by the live-paper phase (earlier if backtest/history data pulls hit free-tier rate limits). Early build/gate work runs on the free/dev tier.
- **Reconciler strategy:** **one new OCC-keyed reconciler + scope the existing nine out** (`asset_class != 'option'` guards). Isolates all options complexity; zero regression risk to the proven forex/futures/equity reconcilers; mirrors the existing forex-only scoping precedent.
- **IV-Rank strategy:** **bounded up-front derivation (~60–90 trading days) + forward accumulation + parallel extension** toward a full trailing year. Resolves the cold-start-vs-fail-closed conflict — the gate has a real value on day one while converging to full rigor without blocking the start.

## Out of Scope (v1)

- **Multi-leg / spreads** (verticals, iron condors, any `mleg` / Level 3 order). Long single-leg only.
- **Short or naked premium** of any kind (selling calls/puts, cash-secured puts, covered calls). No margin, no assignment handling.
- **Overnight / swing holds, LEAPS, exercise, assignment, pin-risk handling.** Intraday-close only.
- **IBKR options** and **index/futures options** (SPX/NDX, /ES options). Alpaca equity options only; IBKR is a later phase.
- **A self-computed live Greeks engine beyond what's needed for the gates.** v1 leans on Alpaca-supplied greeks live and BS-derived greeks in backtest.
- **An LLM options analyst** choosing direction or strike. Direction comes from ORB; strike from the deterministic gates. (No LLM in the hot path.)
- **Rewriting the legacy signals/trades/orders schema** for options — v1 operates on the ORB path (`orb_trades`) only.

## Dependencies

**External:**
- Alpaca account with **Level 2 options approval** and **Algo Trader Plus (OPRA real-time)** market-data subscription.
- Alpaca options APIs: chain snapshots (`/v1beta1/options/snapshots`), contracts (`/v2/options/contracts`), orders (`/v2/orders`), positions (`/v2/positions`), historical quotes (post-Feb-2024).

**Internal (existing code this builds on):**
- ORB signal production — `BracketParams` (`internal/orb/executor.go:22-30`) and placement site (`internal/orb/scheduler.go:1205-1360`) — the interception seam.
- Schema precedent — `migrations/018_forex_orb` (`asset_class` + `conid` pattern) for adding contract identity.
- App-managed OCO precedent — dual-sided forex ORB `StopOrderSimulator` (PR #283) for exit management.
- Alpaca broker adapter (`internal/execution/alpaca_broker.go`) and IBKR `secType` branching (`internal/execution/ibkr_broker.go`) as the option-order-path template.
- ORB backtest engine (orbreplay) for the parity-per-gate backtest discipline.
- Reconciler suite (`internal/engine/reconcile.go`, `internal/orb/orphan_detector.go`, `internal/orb/fill_reconcile.go`, `cmd/orb-reconcile-statement`) — the nine matchers to scope out + the pattern for the new options reconciler.

**Sequencing:** FR-1 (identity) is foundational and blocks FR-2/FR-4/FR-6. FR-3 (gates) and the OPRA-feed provisioning can proceed in parallel with FR-1. The bounded up-front IV derivation plus the daily snapshot job should start as early as possible so the IV-Rank series is ready (day-one seed) and keeps accruing while the rest is built.
