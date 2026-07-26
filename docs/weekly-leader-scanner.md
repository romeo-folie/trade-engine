# Full-universe weekly-leader scanner

The weekly-leader scanner is the second discovery source in epic #482. It
evaluates the complete eligible point-in-time stock universe created by
`te universe refresh`; it does not derive its denominator from daily movers,
the watchlist, or stocks for which market data happened to succeed.

“Weekly leader” describes the multi-week leadership lens. The job runs after
every completed trading session so expansion, contraction, additions, removals,
and persistence are observable without waiting until Friday.

## Safety boundary

A weekly-leader result is discovery evidence, not permission to trade.

When `promote_candidates` is enabled, only qualified rows are handed to the
ordinary market scanner with source `weekly_leader` and their independent
composite score. They still face:

1. active-watchlist deduplication;
2. the scanner candidate cap;
3. news enrichment;
4. the configured LLM conviction threshold;
5. the engine's normal entry, risk, and execution controls.

If a leader reaches the watchlist, `weekly_leader` is retained as an additive
strategy tag beside the LLM's setup strategy. A ticker that was also a mover or
most-active stock retains every source and every independent source score.

## Point-in-time contract

Each run fixes two timestamps:

- `as_of`: the official completed-session close. Daily bars after this boundary
  are discarded, including a forming or provider-corrected future bar.
- `decision_at`: close plus the configured provider-finalization delay. The
  universe must have been effective, captured, and completed by this boundary.

Both the benchmark and every successfully evaluated stock must contain the
target session's completed daily bar. A long but stale history ending on the
prior session fails closed rather than being mislabeled as the current run.

The run records the exact universe version, configuration hash, feature version,
provider, benchmark, first/last input bar, and completed-session count. Names
with incomplete history or provider failures remain in the eligible denominator
as explicit failed observations. Missing values never become numeric zero.
If even one requested universe symbol has a provider-history failure, the run
retains all available measurements for diagnosis but suppresses every
qualification. A partial surviving batch is never renormalized into promotable
leaders.

The default history requirement is 253 completed sessions. New IPOs with less
history receive `insufficient_history`; the scanner does not redefine their
52-week high as a since-IPO high. Configuration validation converts any custom
session requirement to a conservative calendar-day window (252 sessions/year
plus a two-week buffer), preventing an impossible fetch configuration. Fetch
windows are capped at 36,525 calendar days (100 years) so an extreme integer
cannot wrap Go's date arithmetic.

## Measurements

All returns use completed-session log returns.

- The exact final close used by every qualified evaluation and downstream
  scanner prompt.
- 5-, 20-, 60-, and 126-session returns.
- Closing-price proximity to the highest daily high in the last 252 sessions.
- Stock-minus-SPY relative strength over every return horizon.
- Stock-minus-median-industry 60-session strength, requiring known point-in-time
  taxonomy and the configured minimum peer count.
- Relative-strength acceleration: short-horizon cross-sectional ranks minus
  long-horizon ranks.
- Annualized 60-session log-price regression slope and R².
- 60-session price-path efficiency and 126-session close-to-close max drawdown.
- ATR(14) divided by price and volatility-adjusted 60-session return.
- 20-session median dollar volume and stability based on log-dollar-volume MAD.
- Normalized 20-session OBV flow and Chaikin Money Flow.
- The share of positive 60-session progress attributable to the largest
  positive overnight gap.

52-week-high proximity and relative strength are persisted and scored as
separate concepts.

## Normalization and qualification

Continuous measurements use deterministic cross-sectional midrank
percentiles. Equal values receive equal percentiles; ticker order only resolves
display rank. Global ranks are blended with market-cap-band and
volatility-band ranks when a band has at least five observations. This prevents
a raw high-volatility or small-cap spike from winning merely because its return
has a larger magnitude, without imposing artificial small-cap quotas.

The composite weights are:

| Component | Weight |
|---|---:|
| Multi-horizon momentum | 25% |
| 52-week-high proximity | 10% |
| Market/industry relative strength | 20% |
| Relative-strength acceleration | 10% |
| Trend quality | 15% |
| Volatility-adjusted return | 5% |
| Dollar-volume stability | 5% |
| OBV/CMF accumulation | 10% |

Industry strength receives 40% of the relative-strength component when it is
available. Otherwise the market-relative component receives that subweight and
coverage records the missing industry evidence. The score is then multiplied
by `1 - 0.35 × gap_domination`. Both 60- and 126-session returns must be
positive. Qualification requires the configured feature coverage, absolute
score floor, and selection percentile.

## Scheduling and idempotency

The scheduler uses the exchange calendar rather than a 24-hour ticker. It
handles weekends, observed holidays, daylight-saving changes, Black Friday,
Christmas Eve when open, and the applicable pre-Independence-Day early close.
On startup it catches up the most recent completed session.

The database atomically claims
`(session_date, universe_version_id, config_hash)`. A completed run is a no-op;
an actively leased run is retried at its exact lease boundary, and failed or
expired claims are reclaimable. Results,
transitions, breadth, and the completed status publish in one transaction, so
readers never observe a partial run.

Raw point-in-time measurements, scores, and outcomes remain immutable. Streaks,
transitions, breadth turnover, and predecessor links are derived projections:
if an earlier session completes late, the database transactionally repairs that
session's later same-config lineage so chronological history remains coherent.

Candidate promotion has a separate atomic, token-fenced claim. Concurrent scanner
processes cannot score or write the same pending leader. It uses the configured
run lease so a crashed scanner cannot strand the latest leaders in `processing`.
Unfinished claims are released at the end of the scanner cycle; terminal status
is recorded only after the actual outcome is known (`rejected`,
`cap_suppressed`, `failed`, `active_watchlist`, or successfully `promoted` to
the watchlist).

Only the highest-ranked `max_promotions_per_run` qualified rows are made
pending (20 by default). This bounds paid LLM work even when a 90th-percentile
cutoff identifies hundreds of leaders in a large universe.

## Persistence and breadth

`weekly_leader_qualifications` contains one row for every eligible member:
qualified, not qualified, or failed. It stores typed raw measurements,
normalized components, failure stage/reason, rank, score, previous
qualification state, streak, and promotion outcome.

`weekly_leader_transitions` records additions, persistence, and removals,
including universe exits and data-failure removals. Comparisons use only the
previous completed run with a compatible configuration hash.

Overall leader breadth is:

`qualified / full eligible universe`

Coverage is reported separately as:

`successfully evaluated / full eligible universe`

This prevents a provider outage from masquerading as a collapsing leadership
regime. Sector, industry, and market-cap-band breadth rows are also persisted.

## Configuration and operation

Both the universe and weekly-leader source default off:

```yaml
scanner:
  enabled: true
  universe:
    enabled: true
  weekly_leader:
    enabled: true
    promote_candidates: false
    max_promotions_per_run: 20
```

Start in observation mode (`promote_candidates: false`). Ensure a completed
universe refresh exists before the scheduled decision time. Review prospective
history with:

```text
te report weekly-leaders --from YYYY-MM-DD --to YYYY-MM-DD
```

The report prints the full denominator, evaluation coverage, failures, leader
count, normalized breadth, and added/removed/persistent turnover per session.
Only after those diagnostics are stable should candidate promotion be enabled.

Alpaca history is split-adjusted IEX data. Returns are therefore price returns,
not dividend-total returns, and IEX volume is not consolidated SIP volume. That
provenance is retained rather than silently overstating the evidence.
