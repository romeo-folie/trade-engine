# Currency-strength robustness study — 2026-08

> **Archived:** The rolling-origin and leave-one-pair-out follow-up was completed
> on 2026-08-09 and is recorded in
> [currency-strength-backtest-results.md](currency-strength-backtest-results.md).
> Runtime integration and paper deployment are deferred. This document preserves
> the earlier fixed-split study that motivated the corrected follow-up.

## Decision

Do not activate the revised strategy for paper or live orders yet. Keep all 15
researched pairs eligible in research: the available evidence does not justify
excluding an individual pair, but it also does not establish a cost-robust strategy.

## Data and protocol

- Exness M1 consolidated exports, 2022-01-03 through 2026-08-07.
- Complete graph of 15 crosses among AUD, CHF, EUR, GBP, JPY, and USD.
- 2022-2024 train, 2025 validation, and 2026-01-01 through 2026-08-07 untouched
  forward report. Forward trades are excluded from candidate scoring and gating.
- Fixed 100,000 base units per trade.
- Explicit per-pair spread estimates, IBKR-style spot-FX commission, and
  contemporaneous quote-currency-to-USD conversion.
- Decision snapshots must be no more than two minutes old.
- Control: original five-pair additive score.
- Expanded model: connected least-squares currency score; a partial basket is
  accepted only while the configured currency graph remains connected.

## Results

### Original five-pair control

Selected candidate: 06:00-09:00 ranking window, 18:00 exit, 0.005 score gap,
breadth 2.

| Window | Trades | Win rate | Profit factor | Net P&L | Avg/trade |
|---|---:|---:|---:|---:|---:|
| Train | 561 | 52.9% | 1.23 | $22,507 | $40.12 |
| Validate | 181 | 54.7% | 1.39 | $9,684 | $53.50 |
| Forward | 99 | 44.4% | 0.73 | -$4,545 | -$45.91 |

The control failed out of sample despite attractive train and validation results.

### Expanded model, sparse selection

Selected candidate: 06:00-09:00 ranking window, 14:00 exit, 0.005 latent-score
gap, breadth 2.

| Window | Trades | Win rate | Profit factor | Net P&L | Avg/trade |
|---|---:|---:|---:|---:|---:|
| Train | 177 | 51.4% | 1.30 | $7,505 | $42.40 |
| Validate | 32 | 59.4% | 1.96 | $2,523 | $78.83 |
| Forward | 8 | 62.5% | 4.36 | $1,745 | $218.17 |

The result remained profitable through 3x modeled spread, but eight forward
trades are not an adequate activation sample.

### Expanded model, 500-trade selection floor

Selected candidate: 06:00-09:00 ranking window, 18:00 exit, 0.001 latent-score
gap, breadth 3.

| Window | Trades | Win rate | Profit factor | Net P&L | Avg/trade |
|---|---:|---:|---:|---:|---:|
| Train | 771 | 47.2% | 1.01 | $843 | $1.09 |
| Validate | 257 | 49.4% | 1.10 | $3,249 | $12.64 |
| Forward | 149 | 51.0% | 1.14 | $2,566 | $17.22 |

This denser candidate lost money in 2023 and 2024 and became negative at 1.5x
modeled spread. Its edge is too small relative to execution-cost uncertainty.

## Pair evidence

The high-sample candidate produced negative aggregate splits for AUD/CHF,
CHF/JPY, EUR/CHF, EUR/GBP, EUR/JPY, EUR/USD, GBP/AUD, GBP/JPY, and USD/CHF.
Those are descriptive results, not exclusion evidence: several samples are
small, the split was inspected after parameter selection, and persistence across
rolling folds and cost levels has not been established. All 15 pairs therefore
remain in the research universe.

## Required next research gate

1. Run rolling-origin selection rather than one fixed train/validation boundary.
2. Run leave-one-pair-out scoring and execution ablations for every pair.
3. Require pair-level direction and economics to persist across folds and at 2x
   modeled spread before excluding a pair.
4. Require a materially larger untouched sample for sparse candidates.
5. Shadow the final frozen candidate before enabling paper orders.
