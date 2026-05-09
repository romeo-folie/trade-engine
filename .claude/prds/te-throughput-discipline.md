---
name: te-throughput-discipline
description: Generate more BUY signals and exit them adaptively, without lowering quality — informed by Alpha Arena, Grok 4.20 and Claude $50K outcomes.
status: backlog
created: 2026-05-09T20:01:47Z
---

# PRD: te-throughput-discipline

## Executive Summary

The trade engine currently generates too few executable trades and exits the few it places too slowly. Over the last six weeks: 151 signals, 11 executed (7.3%), 132 blocked, 2 closed trades (both winners — NVDA +6.3%, IBKR +5.1%). The position budget is silently exhausted by 8 stale JMIA rows from dev-mode restarts. Public AI portfolio benchmarks (Alpha Arena, Rallies, Grok 4.20, Claude $50K) show the same shape of failure mode and point at the same two missing primitives: trailing stops and per-trade risk caps. This PRD covers the unblock + the throughput uplift, organised so the unblock half can ship and start measuring while the throughput half is still being built.

## Problem Statement

Three problems compound.

**1. Position budget is silently exhausted.** The risk gate counts `len(openTrades)` (database rows). The portfolio card and broker count distinct tickers. The two diverged when 8 JMIA rows accumulated from local dev restarts on 2026-03-30. The gate has read 8 of 10 used since 2026-04-13; effectively 2 free slots. The gate has been wrong for nearly four weeks.

**2. Generation pipeline is too quiet.** Since the last execution on 2026-04-13, the multi-LLM arbiter has produced **0 BUY**, 415 WAIT, 51 HOLD, 10 SELL across all analyses. Watchlist tickers skip via NO_MATERIAL_CHANGE most of the time. Scanner-added tickers reach the LLM but resolve to WAIT because the move already happened by the time they're added. The R:R floor at 1.50 quantises LLM TP suggestions onto the wrong side of the line — multiple historical signals at R:R 1.42 were rejected when the two trades that filled cleared at R:R ≥ 1.65 (both winners).

**3. Exits are too slow.** Trades have no take-profit and ride until the analyst arbiter calls SELL. Alpha Arena's Claude entry (−12% Season 1) failed for exactly this reason: long-only, no dynamic stop-loss, missed reversals. Without trailing stops, a winning trade can give back its gains before the arbiter loop catches up.

## User Stories

### S1 — As the operator, I want the position budget to reflect reality
- The portfolio card and the risk gate must agree on what "open positions" means.
- Counting must be by distinct ticker/direction pair, not by trade row.
- A single-ticker burst (e.g. dev restarts) must not be able to consume the whole budget.
- **Acceptance:** after this lands, the JMIA stack is collapsed; the gate reports 1 JMIA position; the canvas portfolio card reports 1 JMIA position; running the same restart sequence again does not produce 8 rows.

### S2 — As the operator, I want the bot to generate more candidate trades without lowering quality
- The bot should produce BUY signals at least weekly under normal market conditions.
- Calibration must be data-backed, not arbitrary: every gate change must be paired with counterfactual logging proving the historical hit rate would have improved.
- **Acceptance:** week-over-week BUY-signal volume increases by ≥3× over the current 6-week baseline (currently averaging 0.4 BUY/week). Win rate of executed trades remains ≥50%.

### S3 — As the operator, I want trades to exit on price action, not just on the next analyst loop
- Long positions must have a server-side trailing stop active at the broker.
- Stops must adapt as the position moves in our favour; never widen.
- **Acceptance:** after a trade is filled, a stop order is live with the broker; a 1× ATR adverse move from the high-water mark triggers an exit independent of the analyst loop.

### S4 — As the operator, I want every trade to have a hard cap on dollar risk
- No single trade may risk more than 1% of portfolio equity, regardless of confidence/R:R/size_pct passing the existing gates.
- **Acceptance:** the new check rejects any signal whose `qty × |entry − stop|` exceeds 1% of portfolio equity at signal time. Block reason is logged and visible in the canvas.

### S5 — As the operator, I want to see what would have happened under looser gates
- For every blocked signal, also evaluate against R:R 1.30 / conf 0.55 thresholds and log "would have passed" so we can calibrate empirically over time.
- **Acceptance:** counterfactual log lines appear for every blocked signal; daily summary counters expose the rate.

## Functional Requirements

### FR-1 — Position counting correctness
- `internal/engine/engine.go:1272` must count distinct (ticker, direction) pairs.
- New config `risk.max_per_ticker` (default 2) prevents single-ticker bursts.
- Migration adds `consolidated` value to the `trade_status` enum and a `consolidated_into UUID` column.
- One-shot collapse: 8 JMIA rows → 1 canonical row, 7 marked `consolidated`. Wrapped in a single transaction. Verified against broker quantity before running.
- Startup reconciler: on engine boot, fetch broker positions and compare to DB open trades. Log WARN on drift > tolerance. Pattern from `reconcileDoneSessionTrades` (see PR #297).

### FR-2 — Feedback loop verification
- Audit `internal/feedback` → `performanceContext` flow. Sample one ticker's prompt; confirm `performanceContext` is non-empty and reaches the analyst.
- Fix any breakage discovered.

### FR-3 — Canvas position-counter visibility
- Portfolio card surfaces both metrics: distinct tickers (used by the gate) and total trade rows (informational). Mismatch is visible immediately.

### FR-4 — Gate calibration
- Drop `risk.min_risk_reward` 1.50 → 1.40 in default config and `~/.tradeengine/config.yaml`.
- Tier confidence threshold by R:R: conf ≥ 0.60 if R:R ≥ 2.0; ≥ 0.65 if 1.4 ≤ R:R < 2.0; reject below 1.4.
- Loosen `rsi_delta_threshold` 3.0 → 2.0 for scanner-added tickers with conviction ≥ 0.75 only. Default RSI delta unchanged for manual watchlist.

### FR-5 — Prompt tightening
- In both `buildSystem` and `buildMomentumSystem` (`internal/analysis/prompts/stock.go`), instruct the model to downgrade BUY → WAIT when its computed R:R < 1.5. The model should self-filter rather than rely on the gate alone.

### FR-6 — SELL ownership short-circuit
- Reorder `risk.CheckAll` so `checkPositionOwnership` runs before `checkConfidence` for SELL signals. Better — short-circuit the LLM call entirely when no long exists for SELL strategies. Saves tokens.

### FR-7 — First-touch verification (regression check)
- Confirm that the `prev == nil` branch in `material_change.go:98` is firing when scanner adds a new ticker. Add a regression test if not already present.

### FR-8 — Multi-timeframe arbiter for borderline WAITs
- When the arbiter resolves to WAIT with confidence in 0.55–0.70, optionally run a second pass on the alternate timeframe (1H ↔ 4H) and re-arbitrate. Cost is 2× tokens on borderline cases only.

### FR-9 — Per-trade max-$-at-risk cap
- New risk check: `qty × |entry − stop|` must be ≤ 1% of portfolio equity.
- Independent of `size_pct` (which is a position-size cap, not a risk cap).
- Block reason: `max_$_at_risk %.2f%% > limit 1.00%%`.

### FR-10 — Server-side trailing stop manager
- On each portfolio update tick, recompute trailing stop for each open long: `max(initial_stop, high_water_mark × (1 − atr_multiple))`.
- Submit/replace the stop order with the broker.
- Decouples exit timing from arbiter latency.
- Initial implementation: stocks/long-only. Forex/ORB has its own exit machinery already.

### FR-11 — Specialist analysts
- Replace the duplicate-prompt ensemble with role-specialised prompts: fundamentalist, technical, sentiment, risk-manager.
- Each role assigned to a provider (configurable). Arbiter resolves disagreement, not averaging.
- Reuses existing arbiter plumbing; new code is purely in `internal/analysis/prompts` and `internal/analysis/arbiter`.

### FR-12 — Pyramiding into winners
- If a long is up ≥ X% (config) and the analyst returns BUY again, add to position with a tightened stop instead of rejecting as duplicate.
- Subject to `max_per_ticker` (FR-1).
- **Hard dependency:** FR-1 (per-ticker cap + distinct counting) must be live first.

### FR-13 — Counterfactual logging
- For every blocked signal, also evaluate against R:R 1.30 / conf 0.55. Log "would_have_passed=true" and the reason that flipped.
- Surfaces in canvas dashboard.

### FR-14 — Daily summary counters
- Per-day counters: signals generated, blocked-by-reason (top-N), executed, executed → winning. Surface in canvas dashboard.

## Non-Functional Requirements

- **Backwards compatibility**: existing trades, signals, orders, config keys remain valid.
- **Migrations**: forward-only, idempotent. The collapse migration is one-shot but the schema additions (`consolidated` enum value, `consolidated_into` column) must apply cleanly to fresh installs.
- **Branch + PR per task**: per repository CLAUDE.md, no direct-to-main commits; PR + Monitor required for each task.
- **Observability**: every gate change is paired with counterfactual logging so we can measure the calibration delta after two weeks.
- **Test coverage**: new risk checks (FR-9), trailing stop logic (FR-10), and counting logic (FR-1) must have unit tests covering the edge cases identified.

## Success Criteria

- **Position budget is correct**: portfolio card and risk gate report the same number of open positions for every ticker (validated against broker after each task lands).
- **Generation rate**: weekly BUY-signal volume ≥ 3× the current baseline within 4 weeks of Epic B landing, measured via the new daily counters.
- **Win rate preserved**: executed-trade win rate ≥ 50% (was 100% on a tiny sample of 2 — anything above 50% is acceptable for the higher-volume regime).
- **Adaptive exit live**: every long position opened post-launch has a server-side trailing stop active.
- **Per-trade risk capped**: 0 trades opened with > 1% portfolio risk after FR-9 lands.
- **Counterfactual data flowing**: dashboard shows would-have-passed rate within 1 week of FR-13 landing.

## Constraints & Assumptions

- **Constraint**: PostgreSQL is the only database; broker is Alpaca (paper) primary, IBKR as configured. Forex/ORB exit logic is out of scope (already has its own machinery).
- **Constraint**: trailing-stop submission depends on the broker accepting STOP order replacement. Confirmed for Alpaca; IBKR path may need additional work — out of scope for first cut.
- **Assumption**: the broker reports a single JMIA position with quantity matching one of the 8 DB rows (most likely the Mar 31 row since it's the only one with a linked broker order). Verified before migration runs.
- **Assumption**: existing `feedback_insights` / `feedback_flags` tables are populated; the audit task confirms or denies.

## Out of Scope

- Forex/ORB exit logic changes (separate system).
- Live broker (non-paper) integration changes.
- Replacing the multi-LLM arbiter wholesale — specialist prompts reuse existing plumbing.
- Backtesting harness changes (separate package).
- Sizing model changes beyond the per-trade $-at-risk cap.
- Meme/penny stock exclusion logic.
- Crypto trading.

## Dependencies

- Existing infrastructure: `internal/risk`, `internal/engine`, `internal/analysis`, `internal/portfolio`, `internal/web`, `internal/store`, `internal/scanner`.
- Migrations directory consumed by `store.Migrate`.
- Broker abstraction (`brokerView`) — used by both the startup reconciler and the trailing stop manager.
- Existing patterns: `reconcileDoneSessionTrades` (PR #297), SELL ownership guard (PR #306), scanner momentum prompt (PR #309).

## Implementation Note: Two-Epic Split

This PRD is implemented as two epics, both pointing back here:

- **`te-correctness-unblock`** — FR-1, FR-2, FR-3. Lands first; unblocks measurement.
- **`te-generation-exits`** — FR-4 through FR-14. Lands incrementally after Epic A is in production.

Pyramiding (FR-12) in Epic B has a hard dependency on the per-ticker cap (FR-1) from Epic A.
