# Issue #599 PR 3: short execution state map

Status: design. Written 2026-11-10 against `main` @ `6ad04b5e`, after #600 and #601 merged.

Goal: let a Jev v2 **SHORT** decision open a short through the same durable,
crash-safe entry path that longs use, and let **COVER** (and protective
exits) close it, without ever opening a position by accident.

## 1. Stage-by-stage map (current long behaviour → short requirement)

| # | Stage | Code | Long today | Short: what changes | Status |
|---|---|---|---|---|---|
| 1 | Decision | `jev` v2 | BUY / SELL / HOLD / WAIT | SHORT / COVER decided and recorded | done (#601) |
| 2 | Signal creation | `engine.generateSignal` | BUY, SELL → signal | SHORT/COVER currently refused | **remove guard for SHORT/COVER** |
| 3 | Risk checks | `risk.CheckAll` | confidence, R:R, exposure, ownership | R:R is already side-aware (stop above, target below); add `risk.EvaluateShortEligibility` on fresh broker, asset, and market facts; ownership: SHORT requires flat, COVER requires a held short | **wire** |
| 4 | Execution dispatch | `engine.executeSignal` | non-SELL = BUY | refuses all but BUY/SELL | **add SHORT → short entry, COVER → cover exit** |
| 5 | Entry admission | `stock_entry_admission.go` | fresh account, asset, **ask**, buying power, reservations, durable intent | price on **bid**; capacity = short buying power, capped by `risk.shorts` exposure; asset must be shortable and ETB *now* (re-checked at order time); intent side `sell` | **parameterise by side** |
| 6 | Order placement | `AlpacaBroker.PlaceOrder` | buy, bracket optional | side `sell` with `order_class: bracket` (stop above, target below). **Bracket is mandatory for shorts**, so the broker holds the stop from the first fill | supported by adapter |
| 7 | Fill polling | `executeSignal` fill poll + `store.ApplyEntryFill` | until terminal; qty converges | identical; the trade row is `direction = short` | reuse |
| 8 | Protective stop | `initial_stop.go` | standalone **sell** stop | buy stop above entry. With mandatory brackets this is the fallback / repair path only | **parameterise side** |
| 9 | Recovery of entry intents | `stockorders/coordinator.go` | intent side `sell` → `direction = short` already mapped | verify bracket recovery for sell-side parents | **verify + test** |
| 10 | Position monitor | `engine` SL/TP check | side-aware comparisons; exit side flips to **buy** for shorts | already supported | verify + test |
| 11 | Bracket exit reconcile | `bracket_exits.go` | side-aware (`short` branches present) | reuse | verify + test |
| 12 | Exit fill reconcile | `exit_reconcile.go`, `tradePnL` | side-aware P&L | reuse | verify + test |
| 13 | Analyst/Jev exit | `sell_exit.go` (`executeSellExit`) | closes long lots only | new `executeCoverExit`: buy-to-cover for the open short lots; never touches longs | **new** |
| 14 | Trailing stop | `execution/trailing` | `isLongStockTrade` → long only | v1: **shorts excluded** (static bracket stop/target only); a down-ratcheting trail is PR 4 | explicit non-goal |
| 15 | Broker drift | `reconcile.go` | signed quantities; side mismatch never adopted | short positions reconcile by sign | verify + test |
| 16 | Orphan adoption | `AdoptBrokerOrphans` | adopts long orphans when enabled | v1: **never adopt short orphans**; alert only (human territory) | **guard** |
| 17 | Portfolio and prompts | `buildPortfolioContext` | positions with side | already carries `Side`; Jev v2 reads it | done |
| 18 | UI / notifications | web `mapVerdict`, Telegram | SELL labelled "SHORT" in web | relabel (PR 4) | PR 4 |

## 2. Invariants (each enforced in code and tested)

1. **No accidental position.**
   - A long exit can never open a short.
   - A cover can never open a long.
   - A SHORT is placed only when the account holds **no** position in the ticker (no flips, no adding).
2. **Never unprotected.** Every short entry is a broker-side bracket. If protection cannot be verified, the existing containment path cancels or flattens the entry, as for longs.
3. **Fresh eligibility.** `EvaluateShortEligibility` must pass on facts observed at order time: account, asset, quote, SSR, exposure, earnings calendar. Otherwise the signal is blocked with the recorded reasons.
4. **Durable and idempotent.** Short entries use the same order-intent lifecycle as longs. A restart at any point recovers to one consistent trade row, or none.
5. **Disabled means today.** With `risk.shorts.enabled: false`, behaviour is identical to current `main`.

## 3. Acceptance checks (end to end, per CLAUDE.md)

Short entry: accepted, filled, partially filled, cancelled, rejected (for example HTB at order time), delayed fill, retried submit, duplicate signal, stale signal or quote, restart before submit, after submit, and after fill.
Protection: bracket legs verified, a missing leg contained, a stop fill closes the short with correct P&L.
Cover: COVER signal → buy-to-cover → trade closed. Partial cover. A cover attempted with no short held is blocked. COVER never buys more than the held short.
Opposite position: SHORT while a long is held → blocked. BUY while a short is held → blocked. SELL while only a short is held → blocked.
Eligibility at order time: shorting disabled on the account, not ETB, SSR, below floors, earnings, exposure cap. Each blocks with its reason.
Recovery: broker short position vs DB reconciles. A short orphan is alerted, never adopted.
Disabled: an identical run with shorts disabled produces no short orders anywhere.

## 4. Delivery

PR 3 implements stages 2–13, 15, and 16 with the checks above, on paper (Alpaca paper margin account, verified `shorting_enabled: true`). PR 4 adds the short trailing stop, the mirrored anti-chase and trend-alignment entry gates, and UI labels.
