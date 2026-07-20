# Strategy Transfer From Forex Bot

Date: 2026-07-20

## Purpose

This note captures what can be transferred from the MT5 forex-bot research and
implementation cycle into TradeEngine's speculative trading systems. It is a
read-only assessment of the current TradeEngine code/config shape, not an
implementation plan.

## Current TradeEngine Shape

TradeEngine is no longer a simple generic ORB bot. The active local config
already carries several lessons that forex-bot had to learn through iteration:

- Per-instrument strategy settings rather than one shared ORB template.
- Hard max-hold exits to prevent stale opening trades becoming multi-day bets.
- ATR-normalized opening-range filters.
- Spread/risk and commission/risk gates.
- Direction eligibility using higher-timeframe EMA.
- Close-confirmation and retest entry modes.
- Tick-size snapping and point-value-aware sizing for futures.
- Notional caps to avoid broker precautionary-order prompts.
- Adaptive exits: break-even, ATR trailing, partial exits.
- Broker-truth reconciliation for orphaned fills and stale bracket legs.
- Live/backtest parity through shared rule functions.

The active local config currently enables:

- `XAU/USD` CFD ORB on the New York session with 30-minute range, `close_1m`
  confirmation, trend direction eligibility, 90-minute max hold, and adaptive
  stop settings.
- `XAG/USD` CFD ORB, but with fewer entry-quality/adaptive settings than gold.
- `MNQ` futures ORB with 30-minute range, `retest_range`, 1R target, and
  120-minute max hold.
- Options-ORB for `SPY`, `IWM`, and `NVDA` in paper mode.

## Main Assessment

The highest-value transfer is not more ORB complexity. The bigger lesson from
forex-bot is that different market behaviors deserve separate strategy families:
breakout, late momentum, opening shock reversal, sweep/reclaim, and trend
continuation should not all be squeezed into ORB.

TradeEngine already has a strong ORB foundation. The next gains should come from
researching additional entry models with the same discipline:

1. coarse sweep,
2. focused refinement,
3. walk-forward validation,
4. live/backtest parity before arming.

## Recommendations

### 1. Treat XAG/USD As Unvalidated Until Proven

`XAG/USD` is active locally, but it does not currently carry the same validated
entry-quality settings as `XAU/USD`. Do not assume the gold config transfers to
silver.

Suggested research:

- Sweep `XAG/USD` across New York and London sessions.
- Compare `stop_touch`, `close_1m`, `close_3m`, `retest_range`, and
  `retest_swing`.
- Include direction eligibility, range/ATR bands, and max-hold variants.
- Keep silver shadow/paper-only until it survives walk-forward validation.

### 2. Bring ORB Entry-Quality Gates To Equities

The forex/futures ORB path is more mature than the equities ORB path. Equities
still lean heavily on gap direction plus LLM scoring before placing a bracket.

Candidates to transfer into equities ORB:

- ATR-normalized opening-range quality.
- Close-position guard using where the range locks.
- Close confirmation before entry.
- Retest entry mode for gap movers that break and pull back.
- Hard intraday time exit by default.

This should be tested before implementation because equities have different
microstructure: gaps, halts, SSR, earnings catalysts, and stock-specific news
can make a simple mechanical rule less portable.

### 3. Add Opening Shock Reversal As A Separate Strategy

forex-bot's `OpeningShockBot` idea is distinct from ORB: it fades an unusually
large opening move when the move ranks high relative to recent history.

TradeEngine candidates:

- `MNQ`
- `MES`
- `MYM`
- possibly `M2K` if data quality is adequate

Do not apply this to equities blindly. Single-stock opening shocks are often
catalyst-driven and can continue much longer than index futures.

Research shape:

- Opening move percentile filter.
- Directional fade entry after the shock window.
- Short max hold.
- Optional trend/regime veto as a diagnostic first, not a hard rule.
- Year/month robustness report, but avoid overfitting calendar skips.

### 4. Add Sweep/Reclaim As A Separate Strategy

forex-bot's reclaim logic is a mean-reversion/liquidity-sweep strategy:
prior/overnight levels are swept, price reclaims, then the bot fades the failed
breakout.

TradeEngine candidates:

- `MNQ`
- `MES`
- `GC`
- `SI`

Suggested levels:

- Prior day high/low.
- Overnight high/low.
- Asia/London session high/low where relevant.
- Opening range high/low as secondary levels.

Core parameters to sweep:

- Minimum sweep depth.
- Reclaim confirmation close.
- Reclaim timeout.
- Stop placement beyond sweep extreme.
- 1.5R to 3R targets.
- 60-240 minute max hold.

This should be separate from ORB because the entry thesis is failed breakout /
liquidity sweep, not continuation through the opening range.

### 5. Test Trend Continuation Separately From ORB

Gold may support more than one intraday strategy. ORB captures the range break.
Trend continuation captures a later sustained move after the morning range has
already shown direction.

Candidates:

- `GC` / `XAU/USD`
- `MNQ`

Possible shape:

- Morning or session-window range percentile gate.
- Higher-timeframe trend alignment.
- Retained-move requirement.
- Entry after continuation confirmation, not on the first breakout touch.
- Same-day max hold unless explicitly validated otherwise.

This should be evaluated as its own strategy, not as extra ORB logic.

### 6. Keep Calendar Filters Diagnostic First

The forex-bot process showed that calendar/month filters can explain historical
weakness but are easy to overfit. Because trades are operator-monitored, prioritize
entry quality over broad calendar vetoes.

Recommended handling:

- Report performance by year, month, weekday, and session.
- Use those reports to understand fragility.
- Do not implement skips unless the same weakness survives train, validation,
  and forward windows with enough trades.

### 7. Strengthen Broker-Rejection Diagnostics

The MT5 `invalid price` incident reinforces a general execution lesson:
broker rejections should print enough context to explain the failure without
guesswork.

For TradeEngine order placement failures, log:

- symbol,
- asset class,
- order type,
- side,
- requested entry,
- normalized/tick-snapped entry,
- stop,
- target,
- quantity,
- bid/ask or last quote,
- tick size,
- minimum price increment,
- broker stop-distance or equivalent where available,
- order-value/notional,
- margin estimate,
- broker error code/message.

For retry behavior, avoid blind retries. Retry only when a fresh quote confirms
the intended order is still valid. If price has already crossed the trigger,
either skip or enter market only if a separately validated max-chase threshold
allows it.

## Suggested Research Queue

1. `XAG/USD` ORB validation sweep.
2. Equity ORB entry-quality sweep: ATR range, close confirmation, retest, time
   exit.
3. Opening shock reversal for index futures, starting with `MNQ` and `MES`.
4. Sweep/reclaim strategy for `MNQ`, `MES`, `GC`, and `SI`.
5. Trend continuation strategy for gold and `MNQ`.
6. Execution diagnostics audit for all broker order-placement paths.

## Guardrails

- Keep each new strategy as its own explicit strategy family unless a sweep
  proves it is just an ORB variant.
- Require a live/backtest twin before arming anything.
- Prefer entry-quality filters over calendar vetoes.
- Keep paper/shadow mode for unvalidated instruments.
- Validate per instrument. Do not assume `XAU/USD` results transfer to
  `XAG/USD`, or `MNQ` results transfer to `MES`.

