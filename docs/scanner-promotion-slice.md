# Scanner promotion diversification

The scanner promotion slice adds an optional `scanner.max_promotions_per_industry`
cap. It is disabled at zero, preserves the existing conviction and total-cap
ordering, and only counts known taxonomy labels. Blank, `Unknown`, and
`Unavailable` taxonomy values are left uncapped so a taxonomy outage cannot
silently suppress the legacy movers/actives path.

The cap requires `scanner.universe.enabled: true`, because ordinary mover and
active candidates must be hydrated from a completed, point-in-time universe
before industry counts are applied. If that evidence cannot be loaded for a
run, the scanner fails open and preserves the legacy uncapped path.

Every suppressed candidate retains a `promotion_suppressed` terminal stage and
the deterministic reason (`duplicate_key`, `industry_cap`, or `total_cap`) in
the scanner candidate snapshot. Ticker keys and industry labels are normalized
before selection, and the original candidate index is carried through the
selector so duplicate aliases cannot misattribute the rejection.

Correlation-aware promotion is an additive opt-in. With
`scanner.promotion_correlation_enabled: true`, the scanner evaluates up to 60
completed daily return observations, requires 30 overlapping returns, rejects same-ticker
open-position exposure first, and then applies an absolute Pearson threshold
against active watchlist/open-position exposure and already-selected
candidates. Provider transport failures fail closed for that scan; an individual
ticker with insufficient bars simply has no correlation evidence and remains
eligible. Per-symbol provider failures also fail open, but are recorded as
`correlation_unavailable` evidence so the audit can distinguish unavailable
data from an uncorrelated pair. Every rejection retains its conflict ticker in
the scanner snapshot. The default remains disabled.

Weekly-leader and potent-industry candidates use source-specific TTLs
(`ttl_days`, defaults 10 and 5) so a slower after-close observation is not
expired by the regular intraday default. Regular scanner cycles also acquire a
durable interval lease, making restarts and concurrent engine processes
idempotent.

Promotion attribution is available after each run. The CLI appends rejected
correlation alternatives to `te report scanner-funnel`; the web API exposes
the same records at `GET /api/scanner/promotion-evidence` with optional
`from=YYYY-MM-DD` and `to=YYYY-MM-DD` bounds. Telegram summaries report the
number of correlation-filtered alternatives retained for audit, while the
full conflict ticker and measured correlation remain in the scanner snapshot.

Validation currently covers the selector, scanner-to-recorder flow, and all
existing package/store tests. A live `MarketScanner.runScan` test against a
real database/provider remains a follow-up because the current scanner test
harness has no isolated DB/provider fixture; the pure seam exercises the exact
selection and snapshot-attribution logic used by `runScan`.
