# Currency-Strength Backtest Results

## Status

This note records the corrected rolling-origin currency-strength experiment generated on 2026-08-09. It supersedes earlier experimental output from before the research-runner correction.

Further runtime work and the proposed paper pilot were deferred on 2026-08-09. The strategy is not being promoted or configured for operation. These results and the supporting backtest tooling are retained so the research can be resumed later without repeating the investigation.

The corrected artifact covers 2022-01-03 through 2026-08-07, evaluates 480 parameter combinations, and uses the following 15-pair universe:

- AUD/CHF, AUD/JPY, AUD/USD
- CHF/JPY
- EUR/AUD, EUR/CHF, EUR/GBP, EUR/JPY, EUR/USD
- GBP/AUD, GBP/CHF, GBP/JPY, GBP/USD
- USD/CHF, USD/JPY

## Method

The experiment used rolling-origin selection. Each test year was evaluated using parameters selected only from data available before that year:

| Test fold | Selection data |
| --- | --- |
| 2023 | 2022 |
| 2024 | 2022–2023 |
| 2025 | 2022–2024 |
| 2026 through August 7 | 2022–2025 |

Every fold was tested with the normal transaction-cost model (`1x`) and a doubled-cost stress model (`2x`). Parameter selection required at least 75 trades per selection year. Currency scores used the least-squares model and allowed a partial basket only when the available currency graph remained connected.

The pair-removal study tested each pair in two roles:

- scoring removal: omit the pair from currency-strength estimation;
- execution removal: retain its information but prevent the strategy from trading it.

It also compared frozen parameters with parameters reselected after removal. Exclusion evidence was required to improve at least three folds under both cost assumptions, affect at least 50 days, and improve aggregate P&L by at least $250 under both cost assumptions.

## Rolling out-of-sample results

| Test period | Selected window and filters | Costs | Trades | Win rate | Profit factor | Net P&L | Average trade |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 2023 | 06:00–10:00, exit 18:00, gap 0.005, breadth 4 | 1x | 79 | 48.10% | 0.92 | -$1,019.82 | -$12.91 |
| 2023 | same | 2x | 79 | 46.84% | 0.83 | -$2,423.29 | -$30.67 |
| 2024 | 06:00–10:00, exit 12:00, gap 0.005, breadth 1 | 1x | 56 | 44.64% | 0.39 | -$2,868.74 | -$51.23 |
| 2024 | same | 2x | 56 | 32.14% | 0.27 | -$3,865.88 | -$69.03 |
| 2025 | 06:00–08:00, exit 18:00, gap 0.002, breadth 1 | 1x | 163 | 49.08% | 1.08 | $1,905.26 | $11.69 |
| 2025 | same | 2x | 163 | 47.85% | 0.96 | -$851.58 | -$5.22 |
| 2026 through August 7 | 06:00–08:00, exit 18:00, gap 0.002, breadth 1 | 1x | 62 | 51.61% | 1.79 | $5,365.50 | $86.54 |
| 2026 through August 7 | same | 2x | 62 | 46.77% | 1.59 | $4,313.72 | $69.58 |

The stitched out-of-sample folds contain 360 trades. Results are not stable across the full history: 2023 and 2024 lost money, 2025 was marginal, and 2026 is substantially stronger. The strategy therefore does not qualify as an all-regime system on this evidence.

The current regime is nevertheless encouraging. The 2026 test remained profitable after doubling modeled transaction costs, with a 1.59 profit factor and $4,313.72 net P&L at `2x` costs. This would be the most relevant evidence if the work resumes with a monitored paper pilot, but it is a partial-year sample of 62 trades and must not be treated as proof that the improvement will persist.

## Configuration selected for a possible future paper pilot

The 2026 fold selected this configuration using 2022–2025 data only:

```yaml
strength_start: "06:00"
strength_end: "08:00"
exit_time: "18:00"
min_gap: 0.002
broad_min: 1
confirm_min_move: 0
units: 100000
score_method: least_squares
allow_partial_basket: true
```

If research resumes, all 15 pairs should initially remain in both the ranking and tradeable universes.

## Pair-removal result

No pair produced sufficiently robust evidence to justify exclusion. The final verdict for every pair was `keep_insufficient_exclusion_evidence`.

CHF/JPY and USD/CHF deserve monitoring. Preventing their execution and then reselecting parameters improved at least three folds under both cost assumptions:

| Pair | Affected days | Aggregate change at 1x | Aggregate change at 2x |
| --- | ---: | ---: | ---: |
| CHF/JPY | 164 | $8,953.09 | $8,086.72 |
| USD/CHF | 78 | $3,641.34 | $4,103.76 |

That evidence was conditional on reselecting the strategy after removal. It was not corroborated consistently by frozen-parameter execution removal or by removing the pair from scoring. Cutting either pair now would therefore rely on a fragile, specification-dependent result. Both remain in the retained research universe and should receive pair-level monitoring if a pilot is eventually started.

## Research feedback

- Use the least-squares currency model. It is the model selected in every rolling fold.
- Permit missing-pair data only while the remaining basket is connected. One failed pair should not invalidate other valid candidates.
- Keep all 15 pairs eligible. The ablation study found no pair with broad enough evidence for exclusion.
- If the work resumes, treat the 06:00–08:00 ranking window, 18:00 exit, 0.002 minimum gap, breadth 1, and zero confirmation threshold as a frozen pilot specification. Do not tune these values from day-to-day paper results.
- Judge any future pilot primarily on whether current-regime performance persists, while retaining the 2023–2024 losses as evidence that the edge can disappear or reverse.
- Monitor realized spread, commission, slippage, pair contribution, skip reasons, data completeness, and the difference between expected and actual exit time.
- Reassess after a meaningful forward sample. A useful first checkpoint is 40–60 completed paper trades, with earlier review if execution behavior materially diverges from the backtest.
- Do not enable strategy orders without a new decision to resume the project. If resumed, remain paper-only until entry, recovery, and timed-exit behavior are reliable in addition to acceptable P&L.

## Source artifact

The figures in this note come from:

`/private/tmp/currency-strength-rolling-origin-corrected-2026-08.json`

Because `/private/tmp` is not durable repository storage, the summarized results in this document are the persistent research record.
