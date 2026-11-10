# Issue #599 PR 4: mirrored short entry gates, side labels, edge-case tests, and the short trailing stop

Status: design. Written 2026-11-10 against `main` @ `2d816551`, after #602 was merged and deployed.

PR 4 is split into two reviewable PRs:

- **PR 4a:** mirrored entry gates (§1), side labels (§2), and the remaining acceptance tests (§3).
- **PR 4b:** the short trailing stop (§4).

Invariant for both: **disabled means today.** With `risk.shorts.enabled: false`, nothing changes for BUY or SELL. Every new branch is SHORT-only. Long audit records keep their exact JSON shape: new snapshot fields are `omitempty` and are only populated for shorts.

## 1. Mirrored entry gates (PR 4a)

Today SHORT skips every entry gate. Each gate gets a short mirror that runs at two boundaries:

- **Signal generation**, in `generateSignal`, inside a new `resp.Action == SHORT` block next to the BUY block.
- **Execution release**, in `executeShortEntry`, on evidence fetched fresh at the bid, mirroring the long `universal_anti_chase_release`.

Gate decisions are recorded in `entry_gate_decisions` exactly as for BUY, with short-specific names.

| Long gate | Long blocks when | Short mirror blocks when | Missing evidence |
|---|---|---|---|
| Anti-chase rule 1: `stale_catalyst_runup` | stale bullish catalyst (>24h) AND 3-session return ≥ +8% AND (weak volume OR price ≥ 1.75 ATR above EMA20) | `stale_catalyst_selloff`: stale **bearish** catalyst AND 3-session return ≤ −8% AND (weak volume OR price ≤ −1.75 ATR below EMA20) | blocks, as for BUY (`unavailable_block`) |
| Anti-chase rule 2: `prior_high_extension` | within 5% of the 52-week high AND ≥ 2 ATR above EMA20 AND weak volume | `prior_low_extension`: within 5% of the 52-week **low** AND ≥ 2 ATR **below** EMA20 AND weak volume | blocks |
| Anti-chase rule 3: `momentum_exhaustion` | (RSI ≥ 70 OR Stoch %K ≥ 80 OR price above the upper Bollinger band) AND 3-session return ≥ +5% AND weak volume | `momentum_capitulation`: (RSI ≤ 30 OR Stoch %K ≤ 20 OR price below the **lower** band) AND 3-session return ≤ −5% AND weak volume | blocks |
| Trend alignment | price below a declining daily EMA9 and EMA21 (a clear downtrend). Missing data fails open | **Required downtrend** (#599 §4: "shorts require a downtrend"): the short passes only when price is below a declining daily EMA9 and EMA21 | **blocks** (fail-closed: shorts must prove the downtrend) |
| Momentum run-up (`momentum_max_runup_pct`) | `momentum` lens AND price up more than X% vs the prior close | `momentum` lens AND price **down** more than X% vs the prior close (chasing a drop) | fails open, as for BUY |
| News tape confirmation (`news_min_runup_pct`) | `news_sentiment` lens AND price up less than X% vs the prior close | `news_sentiment` lens AND price **down** less than X% (the tape has not confirmed the bearish headline) | fails open, as for BUY |

**Catalyst freshness (#599 §4).** Rule 1 uses the newest **bearish** news item as the catalyst for a short, mirroring the use of bullish news for a long. This also covers "into stale negative news".

**Implementation.** `antichase.Snapshot` gains:

- `Side` (`""`/`"long"` or `"short"`);
- `BBLower`, `PriorLow52Week`, `PriorLowProximity`.

All are `omitempty`. `antichase.Evaluate` branches on `Side`. The long branch is untouched. `PolicyVersion` stays the same: the long semantics are unchanged, and short decisions are distinguishable by `Side` and their rule IDs.

**Not mirrored:**

- **Pullback strength** is a long-only construct, and SHORT already refuses it.
- **First-tick deferral** stays BUY-only. It is cheap to add later if wanted.

## 2. Side shown in web, TUI, and Telegram (PR 4a)

- **Web `mapVerdict`** labels SELL as "SHORT". SELL should read "SELL" (a long exit). SHORT and COVER get their own labels and colours.
- Every place that renders a signal action or a trade direction shows SHORT, COVER, and `direction=short` explicitly, so nothing falls back to BUY or SELL styling. That covers web pages, the TUI, and Telegram signal and trade messages.
- Telegram's exit-reason label for `cover_signal` already exists (#602).

## 3. Remaining acceptance tests for shorts (PR 4a)

Each test runs end to end through the real `stockorders.Coordinator` against a fake Alpaca broker and Postgres:

- a target (take-profit) leg fill closes the short with positive P&L;
- partial cover: a partial stop or target fill reduces quantity, the remainder stays protected, and P&L is correct per lot;
- a partially filled entry: the trade's quantity converges, and protection containment applies while the parent is partial;
- a cancelled entry with zero fill: no trade, and the reservation is released;
- restart mid-order with a partial fill: recovery projects exactly one short trade with the filled quantity.

### Found while writing these tests (shared long and short code, pre-existing on `main`)

- **Containment liveness.** Once containment booked a parent-reported, terminal bracket-child fill (for example a cancelled take-profit with a partial fill), every later pass re-read the same parent snapshot and deferred again. The position was never flattened, and the remaining shares stayed unprotected indefinitely.
  - Fix: a child whose fill is now booked against the trade's projected row (`bookParentReportedBracketChildFill` reports `booked`) and is terminal no longer defers.
  - Containment reloads the trade before flattening, so it only buys or sells the quantity still held.
  - If nothing could be booked (no local child row, or a read error), it still defers, as before.
  - Regression tests cover both sides.
- **Data race.** `verifyOrContainBrokerEntryAsync` read the global retry delay on every retry, from a goroutine that can outlive its caller. It now reads the delay once, when the goroutine is spawned, as `pollUnfilledStockIntent` already does. Behaviour is unchanged in production.

## 4. Short trailing stop (PR 4b, implemented)

All the side-specific logic is in `internal/execution/trailing/direction.go`. The manager calls those helpers at every point that previously assumed a long, and each helper's long branch is the original expression. All 83 existing long tests pass unchanged.

`execution/trailing` is long-only (`isLongStockTrade`). The short mirror:

- **Favourable extreme.** For a short this is the **low**-water mark, stored in the existing `high_water_mark` column, whose meaning is "most favourable price since entry". No migration is needed. The trade's direction decides how the column is read.
- **Arming.** The trail arms at a **peak gain** of `activate_at_gain_pct` from entry, measured as (entry − low) / entry.
- **Ratchet.** The stop is `low + atr_multiple × ATR`. It may only move **down**, never up: `min(current stop, candidate)`.
- **Broker stop.** A **buy** stop above price. Replacement uses the same durable PATCH and replace path, with the side taken from the trade direction.
- **Crossing.** If price rises through the stop, cover at market through the durable software-exit path, with side BUY.
- **EMA9 rule (`rule: ema9`).** For a short, exit on a daily close **above** the EMA9.
- **Bracket coexistence.** As for longs, the trail PATCHes the bracket's stop leg. The bracket's target leg stays in place.

Acceptance:

- the trail never raises a short's stop;
- it never places a sell;
- it arms only after a favourable move;
- a crossing covers exactly the held quantity;
- restarts preserve the low-water mark;
- long behaviour is byte-identical, which existing tests prove.
