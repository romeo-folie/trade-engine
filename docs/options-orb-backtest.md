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
  `internal/data.AlpacaOptionsHist`, which reads three Alpaca endpoints:
  contract listing (`/v2/options/contracts`, trading API), historical NBBO quotes
  (`/v1beta1/options/quotes`, data API), and option bars (`/v1beta1/options/bars`,
  data API), plus equity underlying bars via the existing `AlpacaClient`.
- **Greeks are derived, not fetched.** Alpaca serves delta/IV on *live* snapshots
  only. `HistChainProvider` inverts each historical quote mid through
  `options.ImpliedVol` and computes `options.BSDelta` at that vol, so the delta/IV
  gates run on the same fields they'd see live.
- **As-of discipline.** Every read is bounded by its request's `start`/`end`, and the
  gate's evaluation instant + spot travel through the context (`WithAsOf`), so the
  chain provider can never price with post-signal data.

## Honest accounting

The report distinguishes real outcomes from data gaps — a backtest number is only as
trustworthy as the data behind it:

- **Unpriced** — the option quote was missing at the exit bar (common on fast
  stop-outs). P&L is a fallback, so the trade is excluded from win/loss/P&L and
  counted separately.
- **Unresolved** (`path_exhausted`) — the historical Path ran out before any modeled
  exit fired. No real outcome; excluded from win/loss/P&L.

Win rate is `wins / (wins + losses)` over **priced, resolved** trades only.

## Running

```bash
# Requires Alpaca API creds in the env vars named by markets.stocks.{api_key_env,secret_env}.
te backtest orb-options --from 2024-06-01 --to 2024-08-31
te backtest orb-options --from 2024-06-01 --to 2024-06-30 --underlyings SPY,QQQ
```

Config is read from `options_orb` (gates, sizing, IV window) and `orb`
(range minutes, risk-reward) — the **same** knobs that drive live, so a backtest
describes the code that would ship.

## Data requirements & limits

- **OPRA entitlement.** Alpaca historical option bars/quotes require the Algo Trader
  Plus (OPRA) data subscription. Without it, the option-data calls return HTTP errors,
  which the runner logs — it never fabricates a fill.
- **History depth.** Alpaca option history reaches back to roughly **February 2024**.
  Earlier windows return empty.
- **Option-mid proxy.** The exit path uses each option 1-minute bar's *close* (last
  trade) as the mid. A full NBBO-mid reconstruction would be one quote read per minute
  per trade; the last-trade proxy is the standard, bounded approximation. Bars with no
  trade in a minute leave that path point unpriced (see *Honest accounting*).
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
