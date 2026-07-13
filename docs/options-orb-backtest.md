# Options-ORB backtest

Runnable historical backtest for options-ORB (issue #417). It replays historical ORB
breakouts on the configured underlyings, executes each as a long single-leg option,
and reports win rate / P&L / gate aborts / exit reasons.

## Parity by reuse

The backtest does **not** reimplement any trading decision. `Engine.Replay` calls the
exact live functions the production path uses:

| Decision            | Live + backtest function                     |
|---------------------|----------------------------------------------|
| Strike selection    | `options.Engine.Select` (fail-closed gates)  |
| Contract sizing     | `orb.SizeOptionContracts`                    |
| Exit decision       | `orb.DecideOptionExit`                       |
| Realized P&L        | `orb.ExitPnLM` (×100 multiplier)             |
| Opening range/bracket | `orb.NewRangeBuffer` / `orb.ComputeBracket` |

Because the backtest calls these rather than copying them, its accept/abort and exit
decisions are identical to live by construction — verified by
`TestReplay_ParityWithLiveGate`. The backtest's only job is to supply **as-of
historical data**; the trading logic is production's.

## Architecture

```
te backtest orb-options
   └─ optionsreplay.Runner
        ├─ AssembleSignals(underlying 1m bars) ──► []Signal   (live ORB range+breakout)
        ├─ HistChainProvider.BuildIVSeries      ──► IV-Rank seed (BS-inverted ATM IV)
        ├─ options.Engine.Select (gates)        ──► chosen contract
        │     └─ HistChainProvider.GetOptionChain (as-of quotes → BS delta/IV)
        ├─ HistoricalOptionData.OptionBars       ──► option-mid exit path
        └─ Engine.Replay (size → walk → P&L)     ──► SimTrade → Aggregate → Report
```

- **`HistoricalOptionData`** (`internal/backtest/optionsreplay/histdata.go`) is the
  historical I/O seam. The concrete implementation is
  `internal/data.AlpacaOptionsHist`, which reads the contract listing
  (`/v2/options/contracts`, trading API) and option **bars** (`/v1beta1/options/bars`,
  data API), plus equity underlying bars via the existing `AlpacaClient`.
- **Quotes are modeled from bars (free-data mode).** Alpaca's basic plan serves
  historical option **bars** and **trades** but **not** historical NBBO quotes
  (`/v1beta1/options/quotes` 404s without the OPRA/Algo Trader Plus subscription). So
  `QuoteAt`/`QuotesAt` take each contract's most recent 1-minute bar **close** as the
  last-trade price and bracket it with a **modeled spread** (`--modeled-spread-pct`,
  default 3%). The mid is a last-trade proxy; the **spread gate therefore runs on a
  modeled — not real — spread** (see *Free-data trade-offs* below).
- **Greeks are derived, not fetched.** Alpaca serves delta/IV on *live* snapshots
  only. `HistChainProvider` inverts each contract's mid through `options.ImpliedVol`
  and computes `options.BSDelta` at that vol, so the delta/IV gates run on the same
  fields they'd see live. (The modeled spread is symmetric, so it leaves the mid — and
  thus the derived IV/delta — unchanged.)
- **As-of discipline.** Every read is bounded by its request's `start`/`end`, and the
  gate's evaluation instant + spot travel through the context (`WithAsOf`), so the
  chain provider can never price with post-signal data.

## Honest accounting

The report distinguishes real outcomes from data gaps — a backtest number is only as
trustworthy as the data behind it:

- **Carried** — the option didn't trade at the exit minute, so the exit price is
  **forward-filled** (last-observation-carried-forward): from the last observed bar, or —
  for an immediate exit before the first post-entry trade — from the **entry mid** (always
  a valid gate quote). The trade **is** counted in win/loss/P&L (at a stale-but-honest
  estimate) and flagged. This matters for tuning: *dropping* these instead would remove a
  config-dependent subset of trades (different contracts have different liquidity →
  different drop rates), biasing every cross-config comparison.
- **Unpriced** — the option was *never* observed on the exit path **and** there was no
  entry mid to seed the carry (degenerate — an accepted trade always has one). Only these
  are excluded from win/loss/P&L.
- **Unresolved** (`path_exhausted`) — the historical Path ran out before any modeled
  exit fired. No real outcome; excluded from win/loss/P&L.

Win rate is `wins / (wins + losses)` over **priced, resolved** trades only.

## Running

```bash
# Requires Alpaca API creds in the env vars named by markets.stocks.{api_key_env,secret_env}.
# Runs on the FREE historical option bars — no OPRA subscription needed.
te backtest orb-options --from 2024-06-01 --to 2024-08-31
te backtest orb-options --from 2024-06-01 --to 2024-06-30 --underlyings SPY,QQQ
te backtest orb-options --from 2024-06-01 --to 2024-08-31 --modeled-spread-pct 0.05  # 5% pessimistic
```

Config is read from `options_orb` (gates, sizing, IV window) and `orb`
(range minutes, risk-reward) — the **same** knobs that drive live, so a backtest
describes the code that would ship.

### Persisted IV seed (write-through)

The IV-Rank gate needs a trailing daily-ATM-IV series. Rather than re-derive ~250 days
of it in memory on every run (slow — Alpaca throttles the bar fetches), the backtest
**write-through-caches** it into `option_iv_history` (migration 040/042): the first run
seeds the window, and every later run reads it back and fetches **nothing**. This is
the *same table and reader the live gate uses*, so backtest and live rank against an
identical series. Requires a DB connection. To pre-seed explicitly (e.g. nightly):

```bash
te options backfill-iv --underlyings SPY,QQQ --lookback-days 300
```

The stored `as_of` is the exact 09:45-ET sample instant (not just the day), so the
gate's as-of filter never includes a same-day sample taken *after* a breakout — no
lookahead, even for a short opening range.

## Free-data trade-offs

The backtest runs on Alpaca's free option bars (no $99/mo OPRA subscription), which
means there is no real historical bid/ask. Two consequences worth understanding:

- **The spread gate is modeled, not real.** With a fixed `--modeled-spread-pct` every
  contract gets the same synthetic spread, so the gate doesn't *discriminate* liquid
  from illiquid contracts the way live NBBO would — it's effectively a uniform
  pass/fail at the configured width. It still applies the *cost* of the spread (below).
- **Set the spread conservatively for a lower bound.** Because you choose the spread,
  set it wide (e.g. `0.05`) to stress-test: if the strategy survives a fat modeled
  spread, real (tighter) NBBO would only improve it. A passing modeled-spread backtest
  is a **floor** on performance, not an optimistic ceiling.
- **Exit pays the spread; entry doesn't.** Live enters with a limit-at-mid (no spread
  paid if filled) and exits with a market sell (receives the bid). The backtest mirrors
  this: entry fills at the mid, exit fills a half-spread below it. This fixes the
  mid-fill optimism a naive backtest would have.

To upgrade to real NBBO later, add Alpaca's Algo Trader Plus (OPRA) subscription and
re-point the provider at `/v1beta1/options/quotes`; the rest of the pipeline is
unchanged.

## Data requirements & limits

- **No OPRA subscription required** for the default free-bars mode. (Real historical
  NBBO quotes *do* need Algo Trader Plus — that's the upgrade path above.)
- **History depth (data floor).** Alpaca option history begins ~**mid-January 2024**
  (verified: the first bars appear 2024-01-18, even for multi-year LEAPs listed long
  before — so it's a data floor, not a listing date). Nothing exists before 2024.
  - A window ending before the floor errors out (`--to` before ~2024-01-15).
  - The IV-seed lookback is **clamped** to the floor (`AlpacaOptionHistoryStart`), so
    it never wastes a fetch on pre-floor days that can never have data (and so could
    never be cached — they'd re-fetch every run).
  - A window that starts near the floor has a short IV-rank lookback (few seeded days),
    so more breakouts abort at `iv_rank`. For a fully-cached, fast repeat run, choose a
    window whose whole lookback (~1 year) sits inside coverage — i.e. **2025 onward**.
- **Option-mid proxy.** The mid is each option 1-minute bar's *close* (last trade).
  Bars with no trade in a minute leave that contract unpriced for that instant (see
  *Honest accounting*).
- **Close-only exit path → slightly optimistic.** Entries use each bar's High/Low (the
  resting-stop touch), but the exit walk evaluates the underlying stop/target against
  the bar *close*. So an intrabar stop/target touch that reverses before the close is
  invisible to the backtest — the trade is not exited on that bar. This mirrors the
  live 15-second snapshot sweep (which also only sees discrete prices, not every tick),
  but it does bias results **slightly optimistic** vs. a tick-level fill model: read win
  rates with that in mind.
- **IV-rank cold start.** The IV-Rank gate needs `iv_min_samples` seeded days before
  the first backtest day. The runner seeds `[from − lookback, to]`; a window whose
  lookback predates Feb-2024 coverage yields a short series and the gate fails closed
  (aborts) until enough samples exist — the correct conservative behavior.
```
