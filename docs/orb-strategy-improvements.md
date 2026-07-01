# ORB Strategy Improvement Plan

## Purpose

This document captures proposed refinements for the Opening Range Breakout (ORB)
strategy based on realized trade performance. The goal is not to remove
underperforming instruments. The goal is to keep all configured instruments
eligible while making entries, exits, sizing, and filters adapt to each
instrument's behavior.

This is strategy research and engineering guidance, not financial advice.

## Current Observations

Closed ORB trades reviewed from 2026-04-13 through 2026-06-18:

| Metric | Value |
| --- | ---: |
| Closed trades | 73 |
| Wins / losses / scratch | 20 / 51 / 2 |
| Win rate | 28.2% |
| Realized PnL | -353.67 |
| Average winner | +1.74R |
| Average loser | -1.32R |

The nominal bracket is 2R, but realized execution is worse than the planned
geometry. Winners do not average +2R, and losers are worse than -1R. This moves
the practical break-even win rate to roughly 43%, before any additional costs.

The largest observed issue is not instrument choice. It is that the system
currently treats ORB as a generic fixed-rule strategy:

```text
configured instrument
  -> place both breakout directions
  -> fixed opening range
  -> fixed 2R bracket
  -> hold until bracket resolves
```

That makes EUR/USD, GBP/USD, and XAU/USD behave as if they have the same
volatility, spread, wick behavior, session rhythm, and mean-reversion risk. They
do not.

## Key Failure Modes

### 1. ORB Trades Can Become Multi-Day Bracket Trades

Filled trades are intentionally left live after session cleanup so broker
brackets can close later. That preserves factual PnL accounting, but it changes
the strategy from an intraday opening-range system into a hold-until-bracket
system.

Observed impact:

- Trades held longer than 12 hours contributed most of the total loss.
- The worst observed loss was a GBP/USD buy held from 2026-05-14 to 2026-05-20,
  closing near -9.62R.

ORB should have a hard time boundary. If the opening move does not resolve in
the expected window, the setup is stale.

### 2. Forex Scanner Always Trades Configured Instruments

The forex scanner emits every configured instrument as a trade candidate with
maximum conviction. This means the strategy does not currently ask whether the
session is favorable, whether the range is useful, whether spreads are normal,
or whether the instrument has a directional regime.

### 3. Dual-Sided Brackets Let Noise Pick Direction

Forex mode places both buy and sell stop brackets around the range. This is
simple and neutral, but it lets a single wick choose direction. In choppy
sessions, this produces false breakouts and stop-outs.

### 4. Fixed 2R Exits Are Not Realized as 2R

The data showed:

- Winners average about +1.74R.
- Losers average about -1.32R.

That is a poor payoff profile for a low-win-rate breakout system. The strategy
needs better control over adverse movement, partial exits, trailing, or earlier
failure detection.

## Proposed Strategy Shape

Replace the current generic flow with an adaptive one:

```text
configured instrument
  -> macro event risk check
  -> session quality check
  -> range quality check
  -> directional bias check
  -> confirmed breakout
  -> adaptive bracket
  -> time/trailing exit
```

Every configured instrument remains eligible. The system simply requires the
instrument to present a tradeable setup before risking capital.

## Recommended Improvements

### 1. Add Hard Time Exits

Add an explicit max-hold rule for filled ORB trades. At the time limit:

1. Cancel remaining bracket child orders.
2. Close the open position at market or a controlled limit.
3. Record the result with an exit type such as `time_exit`.

Suggested starting values:

| Instrument | Max hold |
| --- | ---: |
| EUR/USD | 3-4 hours |
| GBP/USD | 2-3 hours |
| XAU/USD | 60-120 minutes |

The time exit should be configurable per instrument and per session.

Rationale:

- ORB edge, if present, should appear near the session open.
- A setup still unresolved hours later is no longer the same trade.
- Time exits prevent -1R failures from becoming multi-R overnight losses.

### 2. Add ATR-Normalized Range Quality Filters

Do not trade only because an opening range exists. Compare the opening range
width to recent volatility.

Example:

```text
range_atr_ratio = opening_range_width / ATR
```

Initial thresholds to test:

| Instrument | Minimum | Maximum |
| --- | ---: | ---: |
| EUR/USD | 0.25 ATR | 0.90 ATR |
| GBP/USD | 0.30 ATR | 1.00 ATR |
| XAU/USD | 0.40 ATR | 1.25 ATR |

Interpretation:

- Too narrow: likely noise; stop entries are easy to wick through.
- Too wide: much of the move may already be spent; target distance becomes
  unrealistic.

This should be measured on recent intraday volatility, not daily candles only.

### 3. Require Breakout Confirmation

Current stop-entry behavior can trigger on a wick. Add confirmation before
entering.

Candidate confirmation rules:

- 1-minute close outside the range.
- 3-minute close outside the range.
- Breakout candle body closes outside the range by at least a minimum fraction
  of ATR.
- Price retests the range boundary and holds outside it.

Suggested starting rule:

```text
Long:
  price closes above range_high + buffer
  and close remains above range_high on the next candle

Short:
  price closes below range_low - buffer
  and close remains below range_low on the next candle
```

For XAU/USD, use stricter confirmation because wick noise and slippage are
larger.

### 4. Replace Blind Dual-Sided Entry With Direction Eligibility

Keep both directions possible, but make each direction earn eligibility.

Example long eligibility:

- Price above session VWAP or opening midpoint.
- 15-minute or 1-hour trend is up.
- Breakout close is above the range, not just an intrabar touch.
- Spread is within allowed bounds.

Example short eligibility:

- Price below session VWAP or opening midpoint.
- 15-minute or 1-hour trend is down.
- Breakout close is below the range.
- Spread is within allowed bounds.

If both long and short are eligible, the strategy may place both sides. If only
one side is eligible, place only that side. If neither side is eligible, skip the
instrument for that session.

### 5. Add Macro Event Calendar Filters

Forex ORB should consider scheduled macro events, but first as an event-risk
filter rather than a directional news-sentiment model.

Examples of high-impact events:

- Central bank rate decisions and press conferences.
- US CPI, PCE, GDP, nonfarm payrolls, and unemployment releases.
- ECB, BoE, BoJ, and Fed speeches marked high impact by the data source.
- Pair-specific releases, such as EUR events for EUR/USD or GBP events for
  GBP/USD.

Generic headlines should not be part of the first forex ORB implementation.
They are noisy, difficult to backtest consistently, and can encourage the
strategy to infer direction from unstructured text. The first version should use
deterministic macro-calendar metadata:

- Scheduled release time.
- Country/currency.
- Event category/name.
- Impact/importance.

Suggested starting behavior:

```text
skip a setup when a high-impact event affecting either currency in the pair
falls inside [session_open - 30m, session_open + 90m]
```

For XAU/USD, treat high-impact USD, Fed, inflation, employment, GDP, and broad
risk events as relevant. XAU/USD should use a stricter blackout than EUR/USD or
GBP/USD because metals tend to reprice sharply around US macro releases.

Alternative behavior to test after the skip rule:

```text
medium-impact event -> apply risk_multiplier_event = 0.50
high-impact event   -> apply risk_multiplier_event = 0.00
```

Important backtest constraint:

- Before the release timestamp, the replay may use only scheduled metadata.
- `actual`, `forecast`, `previous`, and surprise values may be used only after
  the simulated clock has passed the release timestamp.

### 6. Make Exits Instrument-Specific

Do not force every instrument into the same 2R bracket.

Suggested exit experiments:

| Instrument | Exit Model To Test |
| --- | --- |
| EUR/USD | 1.5R-2R target, break-even after +0.8R |
| GBP/USD | 1.5R target, break-even after +0.75R, tighter time stop |
| XAU/USD | partial at 1R, trail remainder to 2R, strict max hold |

Specific mechanics to evaluate:

- Move stop to break-even after +0.75R or +1.0R.
- Take partial profit at 1R.
- Trail using range midpoint, VWAP, or ATR.
- Exit early when price re-enters the opening range after breakout.

The objective is not necessarily to maximize win rate alone. It is to improve
expectancy:

```text
expectancy = win_rate * avg_win - loss_rate * avg_loss
```

### 7. Add Instrument-Specific Risk Multipliers

Keep all instruments enabled, but size them according to volatility and realized
loss distribution.

Starting point:

| Instrument | Risk multiplier |
| --- | ---: |
| EUR/USD | 1.00x |
| GBP/USD | 0.75x |
| XAU/USD | 0.25x-0.50x |

This is not an instrument removal rule. It is an exposure normalization rule.
An instrument with larger slippage, larger gaps, and larger tail risk should not
receive the same effective risk as a calmer FX pair.

### 8. Add Spread and Slippage Guards

Before placing entry orders, reject sessions where the current spread is too
wide relative to the planned stop distance.

Example:

```text
spread_to_risk = current_spread / abs(entry_stop - stop_loss)

skip if spread_to_risk > max_spread_to_risk
```

Suggested starting values:

| Instrument | Max spread/risk |
| --- | ---: |
| EUR/USD | 8% |
| GBP/USD | 10% |
| XAU/USD | 15% |

Also record expected spread at entry time so performance can be analyzed by
liquidity regime.

### 9. Track Regime-Level Performance

Performance should be tracked by more than ticker.

Minimum breakdown:

- Instrument
- Side
- Session
- Macro event regime
- Day of week
- Opening range width
- ATR-normalized range width
- Breakout confirmation type
- Spread/risk ratio
- Time in trade
- Exit type

The strategy should learn from setup classes, not just from instruments.

Example labels:

```text
EUR/USD london long confirmed-close narrow-range no-event
GBP/USD newyork short retest-confirmed normal-spread post-us-cpi
XAU/USD london long momentum-close wide-range pre-fomc
```

## Suggested Config Shape

Add instrument-level ORB parameters under `forex_orb`.

Example:

```yaml
forex_orb:
  enabled: true
  session: london
  timezone: UTC
  range_minutes: 15
  max_risk_pct: 1.0
  macro_events:
    enabled: true
    provider: tradingeconomics
    blackout_before_minutes: 30
    blackout_after_minutes: 90
    min_skip_importance: high
    medium_event_risk_multiplier: 0.50
    high_event_risk_multiplier: 0.00
  instruments:
    - symbol: EUR/USD
      sec_type: CASH
      currencies: [EUR, USD]
      min_lot_size: 5000
      pip_buffer: 0.0002
      risk_multiplier: 1.0
      max_hold_minutes: 240
      min_range_atr: 0.25
      max_range_atr: 0.90
      breakout_confirmation: close_1m
      break_even_r: 0.8
      take_profit_r: 1.8
      max_spread_to_risk: 0.08

    - symbol: GBP/USD
      sec_type: CASH
      currencies: [GBP, USD]
      min_lot_size: 5000
      pip_buffer: 0.0002
      risk_multiplier: 0.75
      max_hold_minutes: 180
      min_range_atr: 0.30
      max_range_atr: 1.00
      breakout_confirmation: close_1m
      break_even_r: 0.75
      take_profit_r: 1.5
      max_spread_to_risk: 0.10

    - symbol: XAU/USD
      sec_type: CFD
      currencies: [XAU, USD]
      macro_event_tags: [USD, FED, INFLATION, EMPLOYMENT, GDP, RISK]
      min_lot_size: 1
      pip_buffer: 0.50
      risk_multiplier: 0.35
      max_hold_minutes: 90
      min_range_atr: 0.40
      max_range_atr: 1.25
      breakout_confirmation: close_3m
      partial_take_profit_r: 1.0
      runner_take_profit_r: 2.0
      break_even_r: 0.8
      max_spread_to_risk: 0.15
```

## Implementation Checklist

### Phase 1: Risk Containment

- Add `max_hold_minutes` and `time_exit` handling for filled ORB trades.
- Cancel child bracket orders before flattening a timed-out position.
- Record `exit_type='time_exit'` or add a dedicated enum/check allowance.
- Emit ORB trade update events for time exits.
- Add tests for time exit behavior.

### Phase 2: Better Entry Quality

- Add ATR calculation for recent intraday candles.
- Add `min_range_atr` and `max_range_atr`.
- Add spread/risk filter.
- Store range quality metrics on the trade or session for later analysis.
- Add tests for range rejection and spread rejection.

### Phase 3: Confirmation-Based Entries

- Replace immediate stop placement with a breakout watch state, or place orders
  only after candle-close confirmation.
- Add confirmation modes: `stop_touch`, `close_1m`, `close_3m`, `retest`.
- Record confirmation mode on each trade.

### Phase 4: Macro Calendar Event Filter

- Add a normalized `macro_events` store for scheduled and historical economic
  releases.
- Add an import command for historical event data.
- Add instrument-to-currency/event-tag mapping.
- Add a deterministic blackout/risk-reduction gate before placing ORB entries.
- Record the active macro event regime on each skipped setup or placed trade.
- Add replay support for a `macro-filter` variant.

### Phase 5: Adaptive Exit Management

- Add break-even stop movement.
- Add partial take profit support where broker/executor supports it.
- Add trailing stop support after a configurable R threshold.
- Record exit source and exit rule separately.

### Phase 6: Feedback and Auto-Throttling

- Build rolling performance summaries by instrument, side, session, and setup
  label.
- Reduce risk multiplier automatically when recent expectancy is negative.
- Restore risk multiplier only after a minimum recovery sample.

## Success Criteria

Evaluate improvements over at least 100 new closed trades or a robust replay
backtest.

Target metrics:

| Metric | Target |
| --- | ---: |
| Realized profit factor | > 1.20 |
| Realized expectancy | > 0.10R/trade |
| Average loss | <= -1.05R, excluding controlled time exits |
| Max single-trade loss | >= -2.00R |
| Win rate | >= 40%, unless avg win materially exceeds avg loss |
| Overnight ORB holds | 0 by default |

## Near-Term Operating Recommendation

Until the refinements are implemented and validated:

- Keep all desired instruments configured.
- Reduce live risk while collecting cleaner data.
- Do not allow ORB trades to remain open overnight.
- Review performance weekly by instrument, side, session, and exit type.

The main objective is to convert ORB from a fixed mechanical breakout into an
adaptive session strategy. Instruments should remain eligible, but every trade
must pass setup-quality checks before receiving risk.

## Expected Outcomes

The expected outcome is not that every instrument wins every week. The expected
outcome is that each instrument receives a rule set that better matches its
volatility, spread, and breakout behavior, improving portfolio-level expectancy
without removing instruments from the universe.

### Outcome 1: Losses Stay Bounded

Hard time exits and bracket cleanup should prevent stale ORB trades from turning
into multi-day losses.

Expected measurable changes:

| Metric | Current Observation | Expected After Phase 1 |
| --- | ---: | ---: |
| Overnight ORB holds | Present | 0 by default |
| Losses worse than -2R | Present | Rare or eliminated |
| Average loser | -1.32R | -1.00R to -1.10R |
| Max single-trade loss | -9.62R observed | No worse than configured cap |

Primary effect:

- Profitability improves even if win rate does not change, because tail losses
  are removed.

### Outcome 2: Fewer False Breakouts

ATR-normalized range filters, macro event filters, spread filters, and
candle-close confirmation should reduce trades triggered by random wicks or
macro-release noise.

Expected measurable changes:

| Metric | Expected Direction |
| --- | --- |
| Number of triggered trades | Down |
| Cancellation/skip rate | Up |
| Win rate | Up |
| Average R per trade | Up |
| Trades stopped within first 15-30 minutes | Down |
| Trades opened immediately before high-impact releases | Down or zero |

Primary effect:

- The strategy trades less often, but the average setup quality should improve.
- Avoiding scheduled macro shocks should reduce slippage, whipsaw entries, and
  false breakout fills around known event windows.

### Outcome 3: More Stable Instrument Behavior

Instrument-specific settings should reduce the performance gap between calm FX
pairs and higher-volatility instruments such as XAU/USD.

Expected measurable changes:

| Instrument | Expected Improvement |
| --- | --- |
| EUR/USD | Preserve or improve win rate while reducing low-quality short trades |
| GBP/USD | Reduce deep losses, especially failed longs and stale holds |
| XAU/USD | Lower loss severity through smaller risk, stricter confirmation, and faster exits |

Primary effect:

- Instruments remain enabled, but risk is allocated according to their realized
  volatility and setup quality.

### Outcome 4: Better Realized Payoff Geometry

Adaptive exits should make realized R closer to planned R.

Expected measurable changes:

| Metric | Current Observation | Expected Direction |
| --- | ---: | --- |
| Average winner | +1.74R | Stable or higher |
| Average loser | -1.32R | Closer to -1R |
| Profit factor | 0.37 overall | > 1.20 target |
| Expectancy | Negative | > +0.10R/trade target |

Primary effect:

- The strategy no longer needs an unrealistically high win rate to break even.

### Outcome 5: Better Diagnostics

Setup labels and richer trade metadata should make future strategy reviews more
actionable.

Expected measurable changes:

- Every closed trade can be grouped by instrument, side, session, range quality,
  confirmation mode, spread regime, macro event regime, and exit rule.
- Weekly reviews can identify which setup types are profitable instead of only
  ranking instruments by raw PnL.

### Outcome 6: Macro Event Risk Is Measurable

The strategy should be able to answer whether scheduled macro events help or
hurt ORB expectancy.

Expected measurable changes:

| Metric | Expected Direction |
| --- | --- |
| Trades skipped because of high-impact events | Tracked |
| PnL around macro blackout windows | Separately reported |
| Slippage around macro releases | Separately reported |
| Expectancy with and without event filter | Comparable in replay |

Primary effect:

- The team can decide whether event filters improve expectancy using historical
  replay instead of intuition.

## Test Plan

The strategy changes should be tested in layers. Unit tests should prove rule
math and state transitions. Integration tests should prove broker/order
behavior. Replay tests should prove that the strategy improves on historical
paths before live risk is increased.

### Unit Tests

#### Time Exit

Test cases:

- A filled trade younger than `max_hold_minutes` is left open.
- A filled trade older than `max_hold_minutes` is closed with
  `exit_type='time_exit'`.
- Remaining TP/SL child orders are cancelled before or during flattening.
- If child order cancellation fails, the system logs the failure and does not
  silently mark the trade closed unless the position was actually flattened.
- A pending, unfilled entry is cancelled at session cleanup and is not marked as
  a time exit.

Expected assertions:

- `CloseORBTrade` or a new close method records the configured exit type.
- `closed_at` is set to the actual time-exit timestamp.
- PnL is computed from actual exit price, not the planned stop or target.
- Re-running the time-exit sweep is idempotent.

Suggested files:

- `internal/orb/*time_exit*_test.go`
- `internal/store/orb.go` tests if new store methods are added

#### ATR-Normalized Range Filter

Test cases:

- Range below `min_range_atr` is skipped.
- Range above `max_range_atr` is skipped.
- Range inside bounds is eligible.
- Missing ATR data causes a safe skip or explicit fallback, depending on config.
- Instrument-specific thresholds override global defaults.

Expected assertions:

- The strategy does not place brackets for rejected ranges.
- Rejection reason is recorded or logged.
- The computed `range_atr_ratio` matches expected candle data.

#### Spread Filter

Test cases:

- Spread/risk above threshold skips the setup.
- Spread/risk equal to threshold is handled consistently.
- Zero or invalid bid/ask values cause a safe skip.
- XAU/USD can use a wider threshold than EUR/USD.

Expected assertions:

- `spread_to_risk` is computed as `spread / abs(entry_stop - stop_loss)`.
- No order is placed when spread quality is poor.

#### Breakout Confirmation

Test cases:

- Wick touches range high but candle closes back inside: no long entry.
- Candle closes above range high and next candle holds: long entry eligible.
- Candle closes below range low and next candle holds: short entry eligible.
- Breakout closes outside the range but spread filter fails: no entry.
- Confirmation mode `stop_touch` preserves old behavior when explicitly set.

Expected assertions:

- Direction eligibility is based on confirmation mode.
- Both sides are placed only when both directions independently qualify.
- The chosen confirmation mode is recorded on the trade metadata.

#### Macro Calendar Filter

Test cases:

- A high-impact USD event inside the configured blackout window skips EUR/USD,
  GBP/USD, and XAU/USD setups.
- A high-impact EUR event skips EUR/USD but does not skip GBP/USD unless USD is
  also affected.
- A medium-impact event applies the configured risk reduction when skip mode is
  disabled.
- Events outside the blackout window do not affect eligibility.
- Missing macro-event data fails according to config: either fail closed for
  live trading or fail open for replay experiments, but never silently.
- Event matching uses normalized currencies/tags, not free-form event names.

Expected assertions:

- `macro_event_regime` or equivalent setup metadata is recorded for every skip
  or trade.
- No order is placed when a high-impact event requires a skip.
- A risk-reduction event scales quantity without changing bracket geometry.
- Event windows are computed in UTC and are stable across local timezone
  settings.

#### Historical Macro Data Import

Test cases:

- Importing the same provider payload twice is idempotent.
- Historical events preserve scheduled release time, country, currency,
  category, importance, forecast, previous, actual, source, and provider ID when
  available.
- Provider event IDs are unique per provider.
- Events with missing importance are stored but do not become high-impact by
  default.
- Unknown countries/currencies are stored but ignored by instrument matching
  unless explicitly mapped.

Expected assertions:

- The import can backfill a date range without broker connectivity.
- Backtest queries return only events whose scheduled time intersects the
  simulated decision window.
- Surprise fields (`actual`, `forecast`, `previous`) are not exposed to the
  strategy before the release timestamp.

#### Adaptive Exits

Test cases:

- Break-even threshold reached: stop moves to entry or configured offset.
- Break-even threshold not reached: stop remains unchanged.
- Partial take profit threshold reached: partial exit order is placed.
- Runner target remains open after partial exit.
- Trail updates only in favorable direction.

Expected assertions:

- Stop movement never increases risk.
- Partial exits update remaining quantity correctly.
- Realized and unrealized PnL are not double-counted.

### Integration Tests

Integration tests should use fake or paper executors where possible, avoiding
live broker dependency.

#### Order Lifecycle

Scenarios:

- Confirmed breakout places only one eligible side.
- Both directions eligible places two brackets.
- One side fills, opposing entry is cancelled.
- Filled trade reaches max hold, child orders are cancelled, position is
  flattened, and DB state becomes closed.
- Restart during a filled trade resumes enough state to enforce time exit.

Expected assertions:

- `orb_trades.status` transitions are correct.
- `filled_at` and `closed_at` retain their separate meanings.
- No filled trade remains open past its max hold unless explicitly configured.

#### DB Migration/Schema

If new columns are added, test:

- Migrations are idempotent.
- Existing rows remain readable.
- New nullable fields do not break old trades.
- New check constraints allow `time_exit` if `exit_type` remains constrained.

Likely new fields:

- `range_atr_ratio`
- `spread_to_risk`
- `confirmation_mode`
- `setup_label`
- `macro_event_regime`
- `macro_event_ids`
- `max_hold_minutes`
- `risk_multiplier`

Likely new table:

```text
macro_events
```

Suggested fields:

- `provider`
- `provider_event_id`
- `release_time_utc`
- `country`
- `currency`
- `category`
- `event_name`
- `importance`
- `actual`
- `forecast`
- `previous`
- `revised`
- `unit`
- `source_url`
- `fetched_at`

### Replay / Backtest Tests

Before increasing live risk, run historical replay on recorded candle paths.

Replay comparisons:

1. Current strategy baseline.
2. Baseline plus time exit.
3. Time exit plus range filter.
4. Time exit plus range filter plus macro event filter.
5. Time exit plus range filter plus confirmation.
6. Full adaptive exit model.

Required metrics:

- Total trades
- Win rate
- Profit factor
- Average R
- Median R
- Max loss in R
- Average winner
- Average loser
- Time in trade
- Skipped trades by macro event reason
- PnL by macro event regime
- Trades by instrument/side/session

Expected result before live rollout:

| Metric | Minimum Gate |
| --- | ---: |
| Profit factor | > 1.10 in replay before paper rollout |
| Average R | > 0.00 before paper rollout |
| Max loss | No uncontrolled loss worse than -2R |
| Overnight holds | 0 unless explicitly allowed |

### Paper Trading Validation

After replay passes, validate in paper mode before increasing live size.

Minimum paper sample:

- 30 closed trades for basic sanity.
- 100 closed trades for confidence.
- At least 10 trades per instrument before judging instrument-specific
  thresholds.

Paper rollout gates:

| Gate | Requirement |
| --- | --- |
| Data integrity | Every filled trade has an exit |
| Time exit | No default overnight ORB holds |
| Risk | No loss materially exceeds configured R cap |
| Expectancy | Non-negative after 30 trades |
| Profit factor | > 1.10 after 50 trades, > 1.20 target after 100 |

### Live Rollout Tests

Live rollout should be staged through risk multipliers.

Suggested rollout:

1. 0.10x risk for first 20 live trades after paper validation.
2. 0.25x risk after no lifecycle or exit bugs are observed.
3. 0.50x risk after realized live slippage is acceptable.
4. 1.00x risk only after the strategy meets profit-factor and loss-cap gates.

Live monitoring assertions:

- No stuck `filled` rows older than configured `max_hold_minutes`.
- No child TP/SL orders remain live after a time exit.
- Broker position matches DB position after every close.
- Daily summary includes ORB realized PnL and exit-type breakdown.

## Acceptance Criteria

The improvement work is complete only when the following are true:

- All configured instruments can remain enabled.
- Each instrument has configurable risk, range, confirmation, and max-hold
  parameters.
- ORB trades do not remain open overnight by default.
- Historical replay shows improved expectancy over the current baseline.
- Unit tests cover time exits, range filters, spread filters, confirmation, and
  macro event filters.
- Unit tests cover adaptive exits.
- Integration tests cover order lifecycle and DB state transitions.
- The stats/reporting layer can break down performance by setup label, not only
  by ticker.

## Small Backtest Feature Proposal

The project already has a general `te backtest` command and backtest package,
but the current path is oriented around daily-candle strategy analysis. ORB needs
a smaller, purpose-built replay feature that can evaluate intraday opening-range
rules without invoking the LLM pipeline.

### Goal

Build a lightweight ORB replay engine that answers:

```text
If these ORB rules had been active over this date range,
which trades would have triggered, how would they have exited,
and which variant produced the best expectancy?
```

This should make it easy to compare:

- Current baseline ORB rules.
- Time-exit rules.
- Range-quality filters.
- Macro event filters.
- Breakout confirmation modes.
- Instrument-specific exits.
- Risk multipliers.

### MVP Scope

Add a new command:

```bash
te backtest orb \
  --from 2026-04-01 \
  --to 2026-06-30 \
  --session london \
  --instruments EUR/USD,GBP/USD,XAU/USD \
  --variant baseline
```

Additional examples:

```bash
te macro import \
  --provider tradingeconomics \
  --from 2024-01-01 \
  --to 2026-06-30
```

```bash
te backtest orb \
  --from 2026-04-01 \
  --to 2026-06-30 \
  --session london \
  --variant time-exit
```

```bash
te backtest orb \
  --from 2026-04-01 \
  --to 2026-06-30 \
  --session london \
  --variant macro-filter
```

```bash
te backtest orb \
  --from 2026-04-01 \
  --to 2026-06-30 \
  --session london \
  --variant adaptive \
  --config ~/.tradeengine/orb-backtest.yaml
```

The MVP should not require broker connectivity. It should use historical candles
from the local candle store when available and backfill through the configured
market data provider when missing.

### Backtest Inputs

Required inputs:

- Date range.
- Session name: `london`, `newyork`, or another configured session.
- Instrument list.
- Initial capital.
- Risk settings.
- ORB variant/rule set.

Optional inputs:

- Candle timeframe for replay, default `1m`.
- Historical macro calendar provider or local macro-event store.
- Macro blackout window and event importance thresholds.
- Slippage model.
- Spread model.
- Commission model.
- Export path for CSV/JSON results.

### Rule Variant Model

Use named variants so experiments are reproducible.

Suggested MVP variants:

| Variant | Description |
| --- | --- |
| `baseline` | Current fixed range, dual-sided bracket, fixed 2R, no time exit |
| `time-exit` | Baseline plus max-hold flattening |
| `range-filter` | Time exit plus ATR-normalized range filter |
| `macro-filter` | Range filter plus scheduled macro-event blackout/risk reduction |
| `confirmed` | Macro filter plus candle-close breakout confirmation |
| `adaptive` | Confirmed entries plus instrument-specific risk and exits |

Each run should persist the exact resolved rule config used for the test.

### Historical Macro Data Requirements

Backtesting macro-aware forex ORB requires historical scheduled-event data, not
only live news headlines.

Recommended data model:

```text
provider
provider_event_id
release_time_utc
country
currency
category
event_name
importance
actual
forecast
previous
revised
unit
source_url
fetched_at
```

Provider requirements:

- Supports historical date-range queries.
- Provides scheduled release timestamp.
- Provides country or currency.
- Provides an importance/impact field or enough metadata to map one.
- Provides stable provider IDs or enough fields for deterministic upsert keys.

Implementation notes:

- Store provider payloads in normalized form before using them in replay.
- Keep provider-specific mapping logic at the import boundary.
- For replay, join events to instruments by normalized currency and event tags.
- Do not use post-release surprise values before the simulated release time.
- Prefer scheduled macro calendars over unstructured headline feeds for the MVP.

### Suggested Config Shape

```yaml
orb_backtest:
  initial_capital: 100000
  timeframe: 1m
  macro_events:
    enabled: true
    source: local_store
    blackout_before_minutes: 30
    blackout_after_minutes: 90
    min_skip_importance: high
    medium_event_risk_multiplier: 0.50
    high_event_risk_multiplier: 0.00
  slippage:
    mode: fixed_spread
    default_spread_points: 0.0001
    per_instrument:
      EUR/USD: 0.00005
      GBP/USD: 0.00008
      XAU/USD: 0.30
  variants:
    baseline:
      range_minutes: 15
      risk_reward: 2.0
      dual_sided: true
      max_hold_minutes: 0
      confirmation: stop_touch

    adaptive:
      range_minutes: 15
      dual_sided: false
      instruments:
        EUR/USD:
          currencies: [EUR, USD]
          risk_multiplier: 1.0
          max_hold_minutes: 240
          min_range_atr: 0.25
          max_range_atr: 0.90
          confirmation: close_1m
          take_profit_r: 1.8
          break_even_r: 0.8
        GBP/USD:
          currencies: [GBP, USD]
          risk_multiplier: 0.75
          max_hold_minutes: 180
          min_range_atr: 0.30
          max_range_atr: 1.00
          confirmation: close_1m
          take_profit_r: 1.5
          break_even_r: 0.75
        XAU/USD:
          currencies: [XAU, USD]
          macro_event_tags: [USD, FED, INFLATION, EMPLOYMENT, GDP, RISK]
          risk_multiplier: 0.35
          max_hold_minutes: 90
          min_range_atr: 0.40
          max_range_atr: 1.25
          confirmation: close_3m
          partial_take_profit_r: 1.0
          runner_take_profit_r: 2.0
```

### Replay Logic

For each instrument and session day:

1. Load 1-minute candles for the session and lookback window.
2. Build the opening range from the first `range_minutes` candles.
3. Compute range-quality metrics, including ATR-normalized range width.
4. Load macro events that intersect the instrument's configured event window.
5. Apply macro blackout or event-risk sizing rules.
6. Decide eligible directions.
7. Watch subsequent candles for breakout confirmation.
8. Simulate entry at the configured trigger or next candle open, depending on
   confirmation mode.
9. Simulate stop, target, break-even, partial, trailing, and time-exit rules.
10. Close any open trade at end-of-session if max hold or no-overnight is active.
11. Record the simulated trade and setup metadata.

Avoid look-ahead bias:

- Do not use candles after the decision point to compute entry eligibility.
- Do not use macro `actual`, `forecast`, `previous`, or surprise data before the
  simulated release timestamp.
- Confirmation can only use closed candles.
- Entry after candle-close confirmation should occur at the next candle open or
  a configured limit/stop rule.

### Output

CLI summary should include:

```text
ORB backtest: adaptive · london · 2026-04-01 -> 2026-06-30

Trades: 68
Win rate: 42.6%
Profit factor: 1.31
Expectancy: +0.14R/trade
Avg winner: +1.42R
Avg loser: -0.93R
Max loss: -1.42R
Avg hold: 87m
Overnight holds: 0

By instrument:
EUR/USD  trades=24 win=45.8% pf=1.24 exp=+0.09R
GBP/USD  trades=31 win=41.9% pf=1.18 exp=+0.06R
XAU/USD  trades=13 win=38.5% pf=1.42 exp=+0.18R
```

The exact numbers above are example output shape, not expected results.

### Stored Results

The MVP can either reuse existing `backtest_runs` and `backtest_trades` tables
or add ORB-specific columns. Prefer reusing the existing tables where possible,
with `strategy='orb:<variant>'`.

Additional useful trade metadata:

- `session_name`
- `range_high`
- `range_low`
- `range_minutes`
- `range_atr_ratio`
- `confirmation_mode`
- `setup_label`
- `planned_r`
- `realized_r`
- `max_favorable_excursion_r`
- `max_adverse_excursion_r`
- `exit_type`

If the existing schema cannot hold this cleanly, add a JSONB `metadata` column
to `backtest_trades` rather than creating many nullable columns up front.

### Comparison Command

Add a comparison mode:

```bash
te backtest orb-compare \
  --from 2026-04-01 \
  --to 2026-06-30 \
  --session london \
  --variants baseline,time-exit,range-filter,confirmed,adaptive
```

Example output:

```text
Variant       Trades  Win%   PF    ExpR   MaxLossR  Overnight
baseline      73     28.2  0.37  -0.45    -9.62       5
time-exit     70     30.0  0.72  -0.12    -1.35       0
range-filter  54     37.0  1.02  +0.01    -1.28       0
confirmed     41     41.5  1.18  +0.08    -1.20       0
adaptive      38     44.7  1.34  +0.16    -1.12       0
```

Again, these numbers are illustrative. The command should make the real
trade-offs visible: fewer trades, better quality, smaller tail losses.

### Backtest Feature Tests

#### Unit Tests

Test the replay engine with deterministic candle sequences:

- Opening range high/low is computed from only the first N candles.
- Long breakout triggers only after the configured confirmation.
- Short breakout triggers only after the configured confirmation.
- If TP and stop are both touched in one candle, pessimistic ordering is used by
  default or the configured tie-breaker is applied.
- Time exit closes the trade at the first candle after max hold.
- No overnight mode closes unresolved trades at session end.
- ATR range filter rejects out-of-bounds ranges.
- Spread model changes entry/exit prices as expected.

#### Golden Scenario Tests

Create small fixed candle fixtures:

- Clean long breakout to target.
- Clean short breakout to target.
- Wick breakout that should be rejected by close confirmation.
- Breakout that re-enters range and stops out.
- Trade that never hits TP/SL and exits by time.
- XAU-style high-volatility candle path with stricter confirmation.

Each fixture should assert the exact simulated trades and metrics.

#### Regression Tests Against Current DB Sample

Use the current closed ORB trade period as a regression window:

- Baseline replay should approximate current behavior.
- Time-exit replay should eliminate overnight holds.
- Adaptive replay should not produce losses worse than configured max loss.

The baseline does not need to match broker PnL cent-for-cent because spread,
slippage, and broker fills differ. It should match directionally enough to be
useful.

#### CLI Tests

Test:

- Missing `--from` or `--to` fails with a clear error.
- Invalid variant fails with known variant list.
- Unknown instrument fails or skips with a clear warning.
- `--export` writes CSV/JSON.
- `orb-compare` runs multiple variants and prints one row per variant.

### MVP Acceptance Criteria

The small backtest feature is acceptable when:

- `te backtest orb` can run one variant over a date range.
- `te backtest orb-compare` can compare at least three variants.
- Results include win rate, profit factor, expectancy in R, max loss in R, and
  overnight hold count.
- Each simulated trade records enough metadata to explain why it entered and
  why it exited.
- Unit tests cover deterministic trade paths.
- The feature can run without LLM calls and without live broker connectivity.
