# Options-ORB

Options-ORB runs the existing Opening Range Breakout signal on a curated set of
liquid mega-cap underlyings but **executes via long single-leg equity options** on
Alpaca instead of shares. A long breakout buys a call, a short breakout buys a put
— always a *buy*, so **max loss = premium paid**. Positions are intraday-only
(flattened before the close), so there is no overnight, assignment, or expiration
risk.

It attaches to the equities/newyork ORB scheduler and is **default-off**.

## Prerequisites

1. **Alpaca Level 2 options approval** (buy calls/puts).
2. **Real-time OPRA options data** — set `markets.stocks.option_feed: opra`
   (requires the Algo Trader Plus subscription). The free `indicative` feed is
   15-minute delayed and unusable for an open-momentum strategy; the adapter logs
   a warning when it runs on a non-`opra` feed.
3. A configured Alpaca stocks broker (`markets.stocks`).
4. **Seeded IV-Rank history** — the IV-Rank gate reads `option_iv_history` and
   **fails closed** below `iv_min_samples` stored days, so an unseeded table makes
   options-ORB place **zero trades**. Run `te options backfill-iv` before starting
   an options session, and daily (e.g. from te-switch), to keep the trailing IV
   window populated:
   ```bash
   te options backfill-iv --lookback-days 60   # seeds option_iv_history (source=derived)
   ```
   It derives daily ATM IV from Alpaca historical option quotes using the *same*
   `BuildIVSeries` derivation the backtest uses, so the live gate ranks against an
   identical series. Idempotent; a `derived` row never overwrites a live-recorded one.

## How it works

1. ORB fires a breakout on a configured underlying (e.g. SPY).
2. The **gate pipeline** (fail-closed) picks a contract: IV-Rank < ceiling →
   `0 < DTE ≤ max_dte` → `|delta|` in band → bid-ask spread < ceiling. Single
   names also get an earnings-blackout gate; ETFs are exempt.
3. The **router** sizes by premium (`floor(premium_budget / (mid × 100))`, capped
   by `max_loss_per_trade`) and places a **limit order at mid**.
4. **App-managed exits** watch the *underlying* against the ORB stop/target and
   market-close the option; an IV-crush time exit closes a stale position; and any
   open option is flattened at 15:50 ET.
5. A dedicated **OCC-keyed reconciler** alerts on broker option positions no
   managed trade explains.

## IV Rank & the cold-start guard

The IV-Rank gate fails closed when it has fewer than `iv_min_samples` stored days
of IV history for an underlying — so a fresh series aborts trades until history
fills. Populate it two ways:

- **Bounded up-front derivation** (`BackfillIVHistory`) — Black-Scholes IV from
  historical option quotes, to seed the series (concrete Alpaca historical-quote
  provider is a follow-up).
- **Daily forward snapshot** (`RecordLiveATMIV`) — records the live chain's ATM IV
  each day.

Until the series reaches `iv_min_samples`, the gate aborts every trade (fail
closed). This is intentional: it prevents trading blind to IV-crush risk.

## Universe & earnings

v1 enables **SPY and QQQ** (index ETFs — earnings-free, tightest option spreads).
Single names (NVDA, TSLA, …) can be added to `underlyings`, but **only with an
earnings feed wired**: with no earnings provider, a single name fails closed at the
earnings gate (never trades). List ETFs in `etf_underlyings` to exempt them.

## Configuration

See `options_orb:` in `config.example.yaml`. Enable with `options_orb.enabled:
true` and `markets.stocks.option_feed: opra`. Key knobs: `underlyings`,
`premium_budget`, `max_loss_per_trade`, the gate thresholds (`iv_rank_ceiling`,
`max_dte`, `delta_min/max`, `spread_pct_ceiling`), `time_exit_minutes`, and the IV
window (`iv_lookback_days`, `iv_min_samples`).

**Validate in paper first.** Real-money enablement should wait until a paper
forward test shows clean contract identity, broker-truth reconciliation with no
unexplained orphans, and no position held past the close.
