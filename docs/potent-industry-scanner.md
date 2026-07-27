# Potent industry scanner

The potent-industry scanner is a default-off, observation-only discovery lens.
It measures whether capital is rotating into an industry broadly enough to be
useful for later candidate selection. It does not authorize orders and, in this
release, cannot promote candidates into the normal scanner funnel.

## Decision boundary

Each run is tied to one completed US equity session and an immutable swing
universe version visible at the configured decision time. The runner:

1. loads eligible universe members and their point-in-time taxonomy and market
   capitalisation;
2. requests split-adjusted daily history for those members, the market
   benchmark, and only the sector/industry benchmarks needed by present groups;
3. excludes future bars and requires the exact target-session bar;
4. evaluates and persists every industry, including failed and non-potent
   groups; and
5. atomically completes the run with group, member, transition, and benchmark
   evidence.

A missing market benchmark fails the run. Missing group benchmarks, stale
taxonomy, incomplete member history, and provider failures fail the affected
evidence closed with an explicit stage and reason.

The scheduler uses the exchange calendar, including DST, holidays, exceptional
closures, and early closes. Startup catches up the latest completed session.
Failures retry after 5, 10, and 20 minutes, then defer to the next exchange
close. Durable leases make restarts and overlapping processes idempotent.

## Group construction and benchmark mapping

Industry is the primary grouping key. Sector is context and selects the
configured sector ETF when no exact industry override exists.

Mappings use exact labels after case and whitespace normalization; they do not
use fuzzy aliases. The scanner never guesses a sector and never substitutes SPY
for a missing sector-relative benchmark. An industry with no exact override and
no mapped, fresh sector is recorded as `benchmark_unmapped` and cannot qualify.

The current universe foundation commonly receives a provider-defined industry
label while sector is `Unknown`. Operators must supply
`industry_benchmark_overrides` for those labels or improve the upstream
point-in-time sector taxonomy. This limitation is intentionally visible rather
than hidden behind a fabricated mapping.

## Deterministic evidence

For each group, the scorer retains:

- equal-weighted, capitalisation-weighted, and median member returns over 1, 5,
  and 20 sessions;
- equal-weighted excess return versus SPY and the mapped group benchmark;
- advance share and normalized advance/decline;
- up-volume share;
- shares above EMA20 and EMA50;
- shares closing at 20-session and 252-session highs;
- median RVOL20 and median dollar-volume expansion;
- breadth acceleration versus the preceding five sessions;
- the number of the last five sessions that showed positive broad,
  market-relative and benchmark-relative participation; and
- positive capitalisation-weighted leadership over equal-weighted returns.

Capitalisation-weighted return is diagnostic and confirmatory. It is not the
primary strength input. A dominance penalty reduces the final score when one
large constituent makes the capitalisation-weighted result materially stronger
than equal-weighted participation.

The bounded potency score uses:

| Component | Weight |
| --- | ---: |
| Multi-horizon market and group-relative strength | 30% |
| Breadth and participation | 35% |
| Volume expansion | 10% |
| Breadth acceleration | 10% |
| Five-session persistence | 15% |

Each component blends bounded absolute evidence with deterministic
cross-sectional percentiles. Qualification also requires the configured
absolute score, percentile, taxonomy, member-count, coverage, median-return,
advance-share, and persistence gates.

Every group receives one diagnostic archetype:

- `broad_strong`
- `strong_narrow`
- `broad_weak`
- `weak`

Only `broad_strong` groups may qualify. The other archetypes remain in the
prospective dataset so narrow leadership and strengthless breadth can be studied
separately.

## Member ranking

Members are ranked only inside qualified groups. The score combines group
potency, stock-versus-peer-industry strength, stock-versus-market strength,
trend quality, and liquidity/participation. Peer-industry return excludes the
stock being ranked so a large winner cannot manufacture its own relative
strength baseline.

A member must exceed the absolute member-score floor, have positive 5- and
20-session stock-versus-industry strength, and remain inside the configured
per-group rank limit. Qualified rows still receive
`promotion_status=not_requested` in this release.

## Configuration

The complete example lives in `config.example.yaml` under
`scanner.potent_industry`. Both `enabled` and `promote_candidates` default to
false. Setting `promote_candidates: true` is rejected until the follow-up
multi-source promotion work is installed.

Important controls include:

- `sector_benchmarks` and `industry_benchmark_overrides`
- `min_fresh_taxonomy_coverage`
- `min_group_members` and `min_group_coverage`
- `min_potency_score` and `selection_percentile`
- `min_advance_share` and `min_persistence_sessions`
- `max_cap_equal_divergence`
- `min_member_score` and `max_members_per_group`

Changing a scoring or mapping control—or the after-close delay that sets the
taxonomy decision cutoff—changes the compatibility hash and starts a new
transition lineage. Operational batching, leases, universe staleness selection,
and the future promotion switch do not change scoring-history identity.

## Reporting

After observation runs exist:

```bash
go run ./cmd/te report potent-industries \
  --from 2026-07-01 \
  --to 2026-07-31 \
  --top-members 3
```

Use `--industry "Software"` for one exact industry label. The report selects one
canonical completed run per exchange session, so incompatible reruns do not
double-count observations. It shows run coverage and transitions, then group
archetype, outcome, score, breadth, persistence, benchmark, failure reason, and
top members.

## Data limitations and rollout

The current Alpaca stock-history adapter requests the IEX feed. IEX is not
consolidated market volume, so volume-derived diagnostics must be treated as
prospective research evidence rather than execution-grade proof. The scanner
remains observation-only until:

1. taxonomy and benchmark coverage are measured;
2. data-feed limitations are resolved or explicitly accepted;
3. prospective conversion and outcome results are reviewed; and
4. the follow-up atomic multi-source promotion path passes PAPER validation.
