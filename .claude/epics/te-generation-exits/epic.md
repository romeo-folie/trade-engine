---
name: te-generation-exits
status: backlog
created: 2026-05-09T20:01:47Z
updated: 2026-05-09T20:49:25Z
progress: 0%
prd: .claude/prds/te-throughput-discipline.md
github: https://github.com/romeo-folie/trade-engine/issues/316
---

# Epic: te-generation-exits

## Overview

Generate more BUY signals per week without lowering the quality bar, and exit positions on price action rather than on the next analyst loop. The lessons from public AI portfolio benchmarks — Alpha Arena, Grok 4.20, Claude $50K — converge on two missing primitives: a server-side trailing stop manager and a per-trade dollar-risk hard cap. We add both. We also calibrate the existing gates with data, tighten the LLM prompts to self-filter weak setups, and add observability so future calibration is empirical rather than guesswork.

## Architecture Decisions

- **Discipline > prediction**, mirrored from Qwen's Alpha Arena Season 1 win. Every trade gets a hard $-at-risk cap and a server-side trailing stop. These are non-negotiable; gate calibration is secondary.
- **Specialist roles, not duplicate prompts.** Grok 4.20's winning entry used 4 role-specialised agents (fundamental / technical / sentiment / risk-manager) debating before deciding. Our arbiter currently runs N copies of the same prompt across providers. We replace duplicate ensembling with role-specialisation; the arbiter resolves disagreement instead of averaging.
- **Counterfactual logging on every blocked signal.** Every gate change must be measurable. Without counterfactuals, the next "should we lower the floor?" debate has no data.
- **Trailing stop is server-side, not engine-side.** A trailing stop computed in our Go process and re-submitted every tick is fragile; submitting/replacing a real STOP order with the broker decouples exit from our process uptime.
- **Pyramiding is gated by the per-ticker cap from Epic A.** Pyramiding without `max_per_ticker` would resurrect the JMIA-style burst problem.

## Technical Approach

### Backend

- **Gate calibration** (`internal/risk/manager.go`, default config): drop `min_risk_reward` 1.50 → 1.40; replace `checkConfidence` with R:R-tiered version; loosen `rsi_delta_threshold` 3.0 → 2.0 for scanner-added tickers with conviction ≥ 0.75 only.
- **Prompt tightening** (`internal/analysis/prompts/stock.go`): both `buildSystem` and `buildMomentumSystem` instruct the model to downgrade BUY → WAIT when its R:R < 1.5.
- **SELL ownership short-circuit** (`internal/risk/manager.go`, `internal/engine/engine.go`): reorder gates so position ownership is checked first; ideally short-circuit the LLM call entirely for SELL strategies on tickers we don't hold.
- **Per-trade max-$ at risk** (`internal/risk/manager.go`): new check `checkMaxDollarRisk`. Independent of `size_pct`. Default cap: 1% of portfolio equity.
- **Trailing stop manager** (new package: `internal/execution/trailing` or extend existing exit logic): on each portfolio update, recompute trailing stop and submit/replace at broker.
- **Specialist analysts** (`internal/analysis/prompts`, `internal/analysis/arbiter`): four role prompts, one provider per role configurable, arbiter resolves disagreement.
- **Multi-timeframe arbiter for borderline WAITs** (`internal/engine`, `internal/analysis/arbiter`): when arbiter resolves WAIT with conf 0.55–0.70, run a second pass on the alternate timeframe (1H ↔ 4H) and re-arbitrate.
- **Pyramiding** (`internal/engine`): if a long is up X% and the analyst says BUY again, add to position with tightened stop. Hard-blocked until Epic A's per-ticker cap is live.
- **Counterfactual logging** (`internal/risk`, `internal/engine`): for every blocked signal, also evaluate against R:R 1.30 / conf 0.55 and log "would_have_passed" plus the flipping reason.
- **Daily summary counters** (`internal/store/portfolio.go` or new): per-day signals generated, blocked-by-reason, executed, executed → winning. Surface via existing API.

### Frontend

- Canvas dashboard: counterfactual rate, daily-counters block, trailing-stop-active indicator next to each open position.

### Infrastructure

- No infra changes. Trailing stops use the existing broker abstraction.

## Implementation Strategy

- **Order of landing:**
  1. Quick wins (1–2 days each, parallelisable): gate calibration, prompt tightening, SELL short-circuit, per-trade $-at-risk cap, counterfactual logging, daily counters.
  2. First-touch verification (regression-only).
  3. Multi-timeframe arbiter (one task, isolated change).
  4. Trailing stop manager (substantial; lands as its own PR with its own monitor).
  5. Specialist analysts (substantial; touches arbiter and prompts; lands after gate calibration is stable).
  6. Pyramiding (last; requires Epic A complete).

- Each task is one PR per the repository convention.

## Task Breakdown Preview

1. Gate calibration: R:R floor + tiered confidence + scanner-RSI loosen (config + risk/manager.go).
2. Prompt tightening (R:R self-filter) + first-touch regression check.
3. SELL ownership short-circuit (reorder + skip LLM call).
4. Per-trade max-$-at-risk hard cap (new risk check).
5. Multi-timeframe arbiter for borderline WAITs.
6. Server-side trailing stop manager.
7. Specialist analysts (role prompts + arbiter resolution).
8. Pyramiding into winners (depends on Epic A).
9. Observability: counterfactual logging + daily-summary counters + canvas surface.

## Dependencies

- **Cross-epic**: pyramiding (task 8) hard-depends on Epic A's distinct-ticker counting + `max_per_ticker` (`te-correctness-unblock` task 002).
- **Internal**: counterfactual logging (task 9) depends on gate calibration (task 1) so the looser thresholds are calibrated against the new defaults, not the old ones.
- **External**: broker support for STOP order replacement (Alpaca confirmed; IBKR validated post-launch).

## Success Criteria (Technical)

- Weekly BUY-signal volume ≥ 3× the current six-week baseline (currently ~0.4 BUY/week) within 4 weeks of the gate-calibration + counterfactual-logging tasks landing.
- Win rate of executed trades ≥ 50% maintained (current 100% on n=2 is statistically meaningless; this becomes meaningful at n ≥ 10).
- 100% of long trades opened post-launch have a server-side trailing stop active within 30 seconds of fill.
- 0 trades opened with > 1% portfolio risk after the per-trade-risk-cap task lands.
- Counterfactual log lines appear for ≥ 95% of blocked signals.
- Specialist analyst rollout: arbiter disagreement rate logged; no degradation in win rate over the first 50 specialist-arbitrated trades.

## Estimated Effort

- 9 tasks. Most XS/S; trailing stop and specialist analysts are M.
- ~40–60 engineering hours total, single-developer pace.
- Quick-win tasks (calibration, prompt, SELL short-circuit, $-at-risk, counterfactuals, daily counters) deliver most of the throughput uplift. The two M tasks deliver most of the discipline uplift.

## Tasks Created

- [ ] #317 - Gate calibration — R:R floor + tiered confidence + scanner-RSI loosen (parallel: true)
- [ ] #318 - Prompt tightening (R:R self-filter) + first-touch regression check (parallel: true; conflicts_with #323)
- [ ] #319 - SELL ownership short-circuit (parallel: true)
- [ ] #320 - Per-trade max-$-at-risk hard cap (parallel: true)
- [ ] #321 - Multi-timeframe arbiter for borderline WAITs (parallel: true)
- [ ] #322 - Server-side trailing stop manager (parallel: true)
- [ ] #323 - Specialist analysts (parallel: false; depends_on #317; conflicts_with #318)
- [ ] #324 - Pyramiding into winners (parallel: true; cross-epic depends on #312 (te-correctness-unblock))
- [ ] #325 - Observability — counterfactual logging + daily summary counters (parallel: true; depends_on #317)

Total tasks: 9
Parallel tasks: 8
Sequential tasks: 1 (#323 must wait for #317 and not run alongside #318)
Cross-epic dependency: #324 hard-depends on #312 (te-correctness-unblock)
Estimated total effort: 40–60 hours
