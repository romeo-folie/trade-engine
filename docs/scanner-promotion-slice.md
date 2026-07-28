# Scanner promotion diversification

The scanner promotion slice adds an optional `scanner.max_promotions_per_industry`
cap. It is disabled at zero, preserves the existing conviction and total-cap
ordering, and only counts known taxonomy labels. Blank, `Unknown`, and
`Unavailable` taxonomy values are left uncapped so a taxonomy outage cannot
silently suppress the legacy movers/actives path.

Every suppressed candidate retains a `promotion_suppressed` terminal stage and
the deterministic reason (`duplicate_key`, `industry_cap`, or `total_cap`) in
the scanner candidate snapshot. Ticker keys and industry labels are normalized
before selection, and the original candidate index is carried through the
selector so duplicate aliases cannot misattribute the rejection.

This slice deliberately does not enable pairwise-correlation limits or regime
routing. Those require their own point-in-time price/evidence and immutable
regime-snapshot contracts; they remain follow-up work in Epic #482 rather than
being exposed as inert configuration here.

Validation currently covers the selector, scanner-to-recorder flow, and all
existing package/store tests. A live `MarketScanner.runScan` test against a
real database/provider remains a follow-up because the current scanner test
harness has no isolated DB/provider fixture; the pure seam exercises the exact
selection and snapshot-attribution logic used by `runScan`.
