# Premarket episodic-pivot scanner

The premarket episodic scanner is an additive stock-discovery source for
meaningful pre-open gaps. It does not place orders, create signals, or bypass
the existing LLM, watchlist, engine-entry, and risk-control path.

## Data entitlement

This scanner requires Alpaca's consolidated `sip` stock feed. The IEX feed
covers only one venue and is not accepted for executable premarket liquidity,
spread, volume, or trade-count decisions. A missing or insufficient SIP
entitlement fails the checkpoint with an explicit
`consolidated premarket data entitlement unavailable` error. The provider never
silently falls back to IEX.

## Schedule and cutoff

`scanner.premarket_episodic.checkpoints` and `hard_cutoff` are New York exchange
times. The scheduler uses the US exchange calendar and `America/New_York`, so
weekends, holidays, and daylight-saving transitions do not shift the scan.

The default checkpoints are 08:00, 09:00, and 09:20 ET. The default hard cutoff
is 09:25 ET, five minutes before the regular open. A checkpoint receives a
durable identity based on session, checkpoint, universe version, and compatible
decision configuration. Restarts reclaim expired work rather than duplicating
it. A result that reaches the completion fence after the cutoff is not
published or promoted.

## Evidence and gates

The scanner first applies the gap, price, and market-cap floors to consolidated
snapshots from the immutable point-in-time stock universe. It then obtains
detailed evidence only for the bounded shortlist:

- gap from the completed prior-session close;
- consolidated premarket volume, dollar volume, and trade count;
- time-adjusted RVOL against the median volume accumulated through the same
  exchange-clock minute in prior sessions;
- consolidated bid/ask spread and quote freshness;
- premarket high, low, and last-price location;
- bar coverage through the checkpoint;
- market-cap risk and explicit float-data availability;
- setup-specific ADR(14) and NATR(14), without a universal ADR threshold;
- the newest catalyst inside the configured news window, a confirmed
  news-free outcome, or a distinct provider-unavailable outcome.

Missing quotes, stale quotes, incomplete bars, incomplete same-clock history,
partial snapshot coverage, and provider failures fail closed. News-free gaps
remain observable and may qualify, but receive a lower catalyst component than
a confirmed fresh catalyst. News-provider failures are not represented as
confirmed absence.

## Candidate lineage

Every evaluated shortlist member is persisted in
`premarket_episodic_candidates` with its raw evidence, active thresholds,
catalyst reference, outcome, and removal stage. Qualified names use the source
tag `premarket_episodic`. When promotion is enabled, the latest completed
checkpoint contributes a bounded set of candidates to the ordinary market
scanner.

If another source discovers the same ticker, the scanner deduplicates the
ticker while retaining every source tag and independent source score.
Watchlist insertion consumes all source claims atomically. Telegram summaries
include the premarket gap, participation, executable-liquidity, and catalyst
outcome that caused the name to surface.

## Configuration

The feature is default-off:

```yaml
scanner:
  enabled: true
  universe:
    enabled: true
  premarket_episodic:
    enabled: false
    promote_candidates: false
    checkpoints: ["08:00", "09:00", "09:20"]
    hard_cutoff: "09:25"
    feed: sip
```

Start with `enabled: true` and `promote_candidates: false` to collect
prospective observations. Promotion can be enabled independently after
reviewing coverage, qualification rate, and false-positive removal stages.

Use the prospective report after migration 064:

```bash
te report premarket-episodic --from 2026-07-01 --to 2026-07-31
te report premarket-episodic --qualified-only
```
