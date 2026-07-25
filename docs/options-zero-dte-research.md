# 0DTE Options-ORB Research Specification

## Status

Research proposal only. This is not a validated production strategy and must not
be enabled with real capital without an independent forward paper pilot.

## Why this is a separate strategy

The existing options-ORB strategy deliberately selects contracts with
`0 < DTE <= 5` and holds intraday until the underlying ORB stop, target, or the
15:50 ET flatten. A 0DTE contract expires that day and has materially different
gamma, theta, execution, and broker-cutoff behavior.

Changing the existing expiry filter to admit DTE 0 would therefore invalidate
its research assumptions. A focused 0DTE strategy should have its own signal
confirmation, contract policy, risk budget, exits, replay, observability, and
operational controls while reusing broker and market-data adapters.

## Initial research hypothesis

A same-day long call or put can express a confirmed opening-range continuation
with defined premium risk, provided the move begins promptly and the strategy
exits weak or stalled breakouts much faster than the 1-5 DTE strategy.

The hypothesis is specifically about immediate post-open continuation. It is
not a claim that a cheaper option or a larger percentage return creates an edge.

## Proposed v1 research universe

- Begin with SPY only.
- Require the real-time Alpaca OPRA feed.
- Add IWM only after SPY produces enough independent observations.
- Keep single names such as NVDA out of v1 because they add earnings, event,
  liquidity, expiration-calendar, and earlier broker-cutoff differences.

## Signal variants to compare

Use the same 15-minute, 09:30-09:45 ET opening range as the existing ORB and
compare these entry rules independently:

1. First touch beyond the buffered range boundary.
2. One-minute candle close beyond the boundary.
3. Confirmed close followed by a successful retest of the broken boundary.

Candidate filters to test rather than assume:

- breakout volume relative to the opening baseline;
- minimum breakout distance beyond the boundary;
- opening-range width relative to ATR and the session's implied move;
- short-term trend alignment;
- macro-event exclusion or explicit event-day segmentation.

The initial entry window should be narrower than the existing 09:45-10:30 ET
window. Start by testing 09:45-10:15 and evaluate later windows separately.

## Contract selection

- Expiration must equal the current ET trading date.
- Compare near-ATM (`abs(delta)` approximately 0.50-0.60) with slightly ITM
  contracts (approximately 0.60-0.70).
- Never select cheap far-OTM contracts merely to increase contract count.
- Require both a percentage spread ceiling and an absolute dollar-spread
  ceiling.
- Prefer high open interest and live quote depth when the data provider exposes
  them.
- Reject stale quotes, crossed markets, missing Greeks, or an unavailable
  real-time feed.

The existing trailing IV-rank gate is not sufficient by itself. Research should
also capture the remaining same-day implied move, opening-range width as a
fraction of that move, intraday IV expansion/contraction, and realized
volatility at entry.

## Risk and sizing

The strategy remains long-option-only, so theoretical maximum loss is premium
paid. Operational sizing must nevertheless assume that a fast adverse move can
consume most or all of that premium before an exit fills.

Initial paper controls:

- one contract per trade, independent of the premium-budget formula;
- one entry attempt per underlying per day;
- no averaging down;
- no automatic re-entry;
- a dedicated, substantially smaller per-trade premium cap;
- a 0DTE-specific daily loss limit;
- a maximum number of failed or rejected orders per session.

Scaling must be based on forward results and executable fill quality, not on the
number of inexpensive contracts a nominal premium budget can afford.

## Exit model

The existing 15-second sweep and opposite-side-of-range stop are too coarse for
a focused 0DTE strategy. Use real-time OPRA and underlying events to evaluate:

1. **Structural invalidation:** exit when price closes back inside the broken
   range or violates a separately researched breakout stop.
2. **Premium catastrophe stop:** cap loss when the option reprices faster than
   the underlying-based exit can react.
3. **No-progress time stop:** exit promptly when continuation does not appear
   within the researched window.
4. **Profit management:** compare fixed underlying targets with break-even and
   trailing-underlying policies.
5. **Mandatory flatten:** close and reconcile the position before Alpaca begins
   expiration-day risk handling.

Alpaca currently documents expiration-day restrictions and position evaluation
beginning around 15:30 ET, with earlier published cutoffs for some individual
stocks/options. The existing 15:50 ET options flatten must not be reused.
Research should start with a conservative mandatory flatten around 15:10 ET and
confirm the exact broker behavior before implementation.

## Execution model

- Enter with a short-lived marketable limit near the NBBO midpoint.
- Cancel rather than chase when the breakout has moved beyond a configured
  slippage allowance.
- Record decision quote, submitted limit, acknowledgement time, fill time,
  fill price, spread, and underlying movement during latency.
- Use limit-first exits when time permits, escalating to a marketable limit or
  market order for protective and mandatory exits.
- Confirm broker-held quantity reaches zero; alert and retry safely on rejected
  or pending closes without creating a naked short.

## Backtest requirements

Minute option bars with a modeled spread are suitable only for rough screening.
A decision to paper-pilot should require:

- historical NBBO quotes or defensible bid/ask reconstruction;
- sub-minute underlying ordering where possible;
- exact same-day contract availability;
- time-varying Greeks and implied volatility;
- missed, partial, and delayed fill modeling;
- entry and exit slippage;
- broker order cutoffs and forced-liquidation behavior;
- explicit ordering when a bar contains both stop and target prices.

Report results separately by entry variant, delta band, time of entry, volatility
regime, day of week, spread, and exit reason. Use walk-forward periods and retain
an untouched forward-paper phase.

## Initial experiment matrix

| Dimension | Initial values |
| --- | --- |
| Underlying | SPY |
| Opening range | 15 minutes |
| Entry window | 09:45-10:15 ET |
| Confirmation | touch, 1m close, close-and-retest |
| Delta band | 0.50-0.60, 0.60-0.70 |
| Sizing | one contract |
| Attempts | one per day |
| Invalidation | range re-entry plus premium backstop |
| Time exit | short no-progress windows to be swept |
| Mandatory flatten | approximately 15:10 ET, pending broker verification |
| Data | real-time/historical OPRA-quality NBBO |

## Promotion criteria

Do not promote the strategy because aggregate P&L is positive. Require:

- positive expectancy after realistic spread and slippage;
- stability across independent time periods and volatility regimes;
- no dependence on one ticker, month, or exceptional trend day;
- acceptable drawdown and full-premium-loss frequency;
- clean broker reconciliation and zero overnight positions;
- reliable closure before expiration controls;
- a forward paper sample large enough to expose operational failures.

## References

- Cboe, 0DTE Trading Resources:
  <https://www.cboe.com/tradable-products/0dte>
- Alpaca, Options Trading Overview:
  <https://docs.alpaca.markets/us/docs/options-trading-overview>
- Alpaca, 0DTE Options Explained:
  <https://alpaca.markets/learn/0dte-options>
- OCC, Characteristics and Risks of Standardized Options:
  <https://www.theocc.com/company-information/documents-and-archives/options-disclosure-document>
