# Swing universe foundation

Issue [#484](https://github.com/romeo-folie/trade-engine/issues/484) adds the
point-in-time denominator used by the leadership scanners in epic
[#482](https://github.com/romeo-folie/trade-engine/issues/482).

## Boundary

The existing market scanner records candidates sourced from movers, actives,
the knowledge graph, and news sympathy. Those candidate snapshots remain the
scanner funnel's numerator. A universe snapshot is a separate immutable record
of every stock considered, including excluded and data-incomplete members. It
must not be inserted into `scanner_candidate_snapshots`, because doing so would
make unsourced stocks appear to have entered the existing funnel.

This foundation does not change candidate discovery or entry behavior. The
industry, weekly-leader, and premarket scanners consume it in later, separately
gated work.

## Point-in-time contract

Each completed universe version records:

- the provider and provider/capture evidence;
- the market-data `as_of` time and the later capture time;
- the exact filter configuration as JSON evidence;
- total, eligible, unknown-taxonomy, and stale-taxonomy member counts;
- an immutable member row for every asset returned by the provider.

Member rows retain the provider asset ID, ticker, security type, exchange,
active/tradable flags, price, median dollar volume, spread, market
capitalization, eligibility result, taxonomy value and taxonomy timestamps.
`Unknown` is a real taxonomy state, not an empty sector that can accidentally
form an industry group.

Only a `completed` version is consumable. A refresh builds a new version; it
never mutates a previous completed version. A partial provider failure remains
visible on the affected member. If the refresh cannot establish a coherent
asset list, no empty version is published and the last completed version
remains available.

## No-lookahead rules

Historical callers request a universe by decision time. The store may return
only a completed version whose `as_of` is at or before that time. It must never
fall back to the current universe or to taxonomy first observed after the
decision.

Taxonomy observations are append-only. A lookup selects the newest observation
at or before the requested time and reports whether it exceeds the configured
maximum age. Missing, future-only, or stale taxonomy cannot qualify an
industry-level signal. It may remain eligible for a scanner that does not
require industry evidence.

The current-state knowledge graph `entities.sector` field is not historical
research evidence and is deliberately not used for universe reconstruction.

## Providers and cache policy

Provider interfaces are narrow capabilities rather than additions to
`MarketDataProvider`, so existing brokers, mocks, and backtests do not acquire
unrelated methods.

- Alpaca's trading API supplies the complete US-equity asset master; active and
  tradable state are retained for the configurable eligibility gates. Alpaca does
  not reliably distinguish common stock, ETFs, ADRs, and warrants in this
  payload, so the safe built-in security type is the evidenced `us_equity`
  asset class. A narrower filter requires a richer asset-master provider.
- Alpaca's market-data API supplies bounded batches of split-adjusted daily bars
  and current snapshots. Liquidity uses completed US sessions only; an
  in-progress daily candle is excluded.
- Finnhub's free company profile supplies market capitalization and a broad
  `finnhubIndustry` label. That label is stored with provider provenance; it is
  not represented as full GICS sector/industry data. Universe refresh requires
  the configured news provider and credential to actually be Finnhub; a
  Benzinga key is never forwarded to the Finnhub endpoint.

Company profiles are rate-limited and cached as observations. A refresh bounds
the amount of missing/stale taxonomy it will fetch in one run. Never-attempted
symbols are prioritized, while fresh Unknown/Unavailable observations wait for
the taxonomy-age retry window so a fixed ticker prefix cannot consume every
refresh quota. Successful
members are retained when an individual profile or market-data batch fails, and
the failed members receive an explicit failure stage. The configured missing
evidence policies decide whether those members remain eligible; the decision is
persisted with the snapshot.

A total snapshot or daily-bar outage publishes no version, preventing an empty
failed-data universe from superseding the last healthy completed snapshot.

## Eligibility stages

The filter is deterministic and records the first failing stage:

1. security type;
2. exchange;
3. active status;
4. tradability;
5. market-data availability;
6. minimum price;
7. minimum market capitalization;
8. minimum median dollar volume;
9. maximum spread.

Unknown taxonomy is not a universe-exclusion stage. It is persisted and later
fails closed only for scanners whose thesis depends on an industry group.

## Operating model

`scanner.universe.enabled` defaults to `false`. `te universe refresh` creates a
current version and prints its counts; it deliberately accepts no historical
date. `te universe list --as-of <RFC3339>` reads the latest version that had
already completed at that decision time. It lists eligible members by default;
`--all` enumerates every excluded and failed asset-master row.

When enabled, each ordinary scanner run links itself to the latest completed
version inside `snapshot_ttl_hours`. A missing or expired version remains
explicit rather than falling forward. Later scanner PRs will schedule the
refresh itself after the close and consume this linked denominator.

Before any leadership scanner affects the watchlist, universe refreshes must run
in observation mode and the prospective reports must show stable member counts,
bounded failure rates, fresh taxonomy coverage, and reproducible as-of results.
