# Exit-policy study — findings & apply-plan (handoff)

**Status (2026-07-25):** study and harness complete (PR #470); Branch 1 JSON recovery merged (PR #473); Branch 2 live exit policy merged and enabled in paper config (PR #474); cumulative partial-exit accounting merged (PR #475). The full six-ticker/five-variant matrix was replayed from cache after implementation and reproduced every documented profit factor exactly. **Phase 2 is active in issue #472.**

## Why this exists
Our stock swing path sits at ~27% win rate; prior work (`te study execution`, PR #433) found **exits, not entries, are the leak** (~+1.53%/trade left on the table). A study of the 2025 US Investing Champion's pullback method (Martin Luk) pointed at the same thing: he runs a low win rate but wins via **asymmetry** — tight stops, and letting winners run on a **close-below-9-EMA** trailing stop instead of a fixed take-profit. We built a backtest harness to test whether our **fixed take-profit truncates winners**, and it does.

## Source method — Martin Luk's pullback strategy (video transcript)
Source: https://youtu.be/F0vssXyWizc (breakdown of the 2025 US Investing Champion's pullback method). Watched via the `watch` skill (full captions + chart/stat frames). Captured here so the lineage from source → study → plan is complete — Phase 2 (the entry filter) is essentially this setup.

**Who / track record:** Martin Luk — ~969% return in the championship year, ~283% the year before (after a ~50% drawdown early in his career). The pullback entry is his own technique (broader style inspired by Kristjan Kullamägi).

**Core philosophy — asymmetry via tiny risk:** "risk as little as possible while maximizing upside." He describes a *parabolic* relationship between stop width and reward: tightening a stop from 3%→1.5% adds ~8 extra R on a 25% winner, ~17 R on a 50% winner. So he obsesses over **tight entries**, not accuracy.

**Universe / scan:** strongest momentum stocks — best performers over 1/3/6-month, up **>30%** in those windows, take the top 20–30%; filter mcap > $100M and $-volume > $1M.

**Setup (buy weakness in *strength*, never weakness in weakness):** a long consolidation (8+ weeks ideal) *inside an uptrend* — **higher lows, narrowing range, price surfing rising 9 & 21 EMA, and reclaiming an anchored VWAP (AVWAP)** anchored to the consolidation's swing high/low. Avoids stocks below *declining* 9/21 EMAs making lower highs/lows.

**Entry:** buy the **intraday flush** down to *stacked* support (AVWAP + 9 EMA, or AVWAP + a horizontal level) the moment price turns positive on the day / closes back above support. **Stop at the low of the day → very tight (often 1–4%, sometimes <1%).**

**Exit:** **no fixed profit target.** Trail with a **close below the 9 EMA** (tight) or **21 EMA** (looser, bigger). His cited examples ran 24%→126% = **20R–31R**.

**Stats (from the on-screen championship table):** 2025 ≈ **22% win rate** (205W / 731L), avg win ~14% vs avg loss ~2%; 2024 ≈ 34% win rate (164W / 319L), avg win 14.76% / avg loss −2.71%; winners held **~4–5 days**. Wrong ~85% of the time; **average win ≈ 5–7× average loss.** The edge is entirely payoff asymmetry, not hit rate.

**Chart examples cited (for reference):** IonQ (2025), Tesla, AMPX, SanDisk (SNDK), RMLD, SYRE. (The channel's own variant applies the same technicals on *weekly* charts for a more passive version.)

**How it maps to our work:**
- **What our study tested (exits only):** uncapping the fixed TP + trailing (9-EMA vs ATR) and a high-conviction SELL gate. Confirmed the exit truncation and the payoff-asymmetry thesis. → **Branch 2 (#471).**
- **What we have NOT tested / built yet:** the **tight-stop lever** (stop-width sweep — our model stops look too wide), and **AVWAP + 9/21 EMA indicators** (we only compute 20/50/200 EMA + session VWAP), and the **pullback-in-strength entry** (scan strong momentum → consolidation w/ higher lows + EMA/AVWAP reclaim → flush entry to stacked support → day's-low stop). → **Phase 2 (#472)**, plus optional follow-ups noted below.

## The study
- **Harness:** `te backtest run` with exit-policy flags (see below). Replays the SAME cached entry signals under different exit rules — so comparing exits costs no LLM calls after the first (baseline) pass per ticker.
- **Setup:** recent 6-month window `2026-01-15 → 2026-07-15`, daily candles, `trend_following`. Universe = the 6 longest-tenured watchlist names: **NVDA, IBKR, META, HIMS** (tradeable, baseline PF > 1) and **OPEN, JMIA** (underperformers, baseline PF < 1).
- **Variants per ticker:** `baseline` (fixed SL/TP + ungated SELL) · `trail-atr-1.5` (uncap TP, ATR-1.5× trail) · `trail-ema-9` (uncap TP, close-below-9-EMA) · `gate-0.72` (baseline but SELL exits only at conf ≥ 0.72) · `trail+gate`.

### Results — profit factor (★ = best per ticker)
| Ticker | baseline | trail-ATR | trail-EMA-9 | gate | trail+gate | baseline exit story |
|---|---|---|---|---|---|---|
| NVDA | 1.70 | 1.92 | **2.60**★ | 1.93 | 2.36 | 7/17 exits hit the fixed TP |
| IBKR | 1.45 | 1.98 | **2.31**★ | 1.47 | 2.08 | 9/24 TP-capped; EMA caught a **17.6R** runner |
| META | 1.04 | 1.17 | **1.66**★ | 1.08 | 1.17 | choppy; ATR whipsawed |
| HIMS | 1.47 | **3.11**★ | 2.55 | 1.47 | **3.11**★ | 9/22 TP-capped at **2R max** → uncap = 5.7× expectancy |
| OPEN | 0.52 | 0.42 | 0.86 | 0.65 | 0.41 | loser regardless (bad entries) |
| JMIA | 0.64 | 0.28 | 0.39 | **1.05**★ | 0.69 | loser; only the gate turned it profitable |

### Findings
1. **We systematically cap winners.** On every tradeable name the fixed take-profit was a top-2 exit (~40–45% of trades). Removing it is where the gains come from.
2. **Uncap + trail ≈ doubles profit factor** on tradeable names. IBKR's 9-EMA trail caught a 17.6R runner the TP would have capped at ~5R.
3. **`trail-ema-9` (close below 9-EMA — Martin's rule) is the most ROBUST single change** — best/near-best on all tradeable names AND least-harmful on the losers (cuts losers fast).
4. **`trail-atr-1.5` has a higher ceiling but HOLDS LOSERS** on choppy/bad names (made OPEN/JMIA worse). Do NOT ship ATR-trail as the sole rule.
5. **The exit-conviction gate is situational but safe** — decisive on NVDA, the only profitable variant on JMIA, harmless elsewhere. Its value unlocks only after the TP is uncapped.
6. **Exits can't fix bad entries.** OPEN & JMIA lost under every policy → need a "buy strength, not weakness" entry filter as a companion (Phase 2).
7. **Caveat:** modest sample, mostly modest winners (only IBKR showed a real fat tail). A 2025 trending-window run would test the ≥10R benefit further, and a **stop-width sweep** (untested; model stops look too wide) is likely another lever. Both are cheap now (cached signals) but deprioritized per operator (token budget).

## The harness (PR #470, on `main`)
- **Flags** on `te backtest run`: `--exit-mode baseline|trail`, `--trail-rule atr|ema`, `--trail-atr-mult` (1.5), `--trail-ema-period` (9), `--exit-conf-min` (0 = ungated), `--stop-width-pct` (0 = use model stop). All default to baseline.
- **Metrics** now include `AvgWinPct`, `AvgLossPct`, `PayoffRatio`, `AvgRMultiple`, `MaxRMultiple`, `RunnersOver10R`, and an exit-reason breakdown.
- **Code:** `internal/backtest/runner.go` (`RunOptions` exit knobs, `resolveExitPolicy`, `evaluateExit`, `atrSeries`/`emaSeries`), `internal/backtest/metrics.go`, `cmd/te/backtest.go`.
- **Resumability:** signals are cached per candle in the `prompt_cache` table (no TTL). A re-run of a completed window is 100% cache hits (instant). **Gotcha fixed in #470:** the cache Get/Put must use the SAME primary `(provider, model)` key — previously Put used `meta.Provider/Model` (the arbiter for arbitrated candles), a permanent miss. Don't reintroduce that.
- **Also fixed in #470:** the runner writes `direction: "long"` (the `trade_direction` enum is `long|short`; it wrote `"buy"` and never persisted).
- **Driver used for the study:** a per-ticker loop (baseline then variants). Each ticker's baseline is the only slow step (~30 min / ~700 LLM calls over 6 months daily).

---

## Branch 1 — gpt-5-nano JSON fix (DONE)
The `analyst_frontier`/fundamental role (`gpt-5-nano`) emitted invalid JSON (literal newlines inside string values; stray `/`), so its analyst vote was silently dropped and the ensemble ran a man down. **Fixed** in `internal/analysis/anthropic.go`'s shared `extractAnalysisJSON` (used by all providers): (1) `sanitizeJSONControlChars` escapes literal control chars inside strings and retries (full structured recovery); (2) the regex `recoverPartialAnalysis` fallback now fires on ANY parse failure, not just truncation. Tests in `anthropic_test.go`. Optional escalation if it persists: set OpenAI `response_format: json_object`, or bump `analyst_frontier` → `gpt-5-mini` (the live config comment already flags this).

---

## Branch 2 — apply the exit findings (DONE — PR #474)
**Goal:** in the LIVE stock exit path, uncap the fixed take-profit and ride a **close-below-9-EMA** trailing stop, plus a **high-conviction SELL gate**. Default-off config, PAPER-first.

**IMPORTANT:** the backtest proved the *concept*; the LIVE exit path is different code (broker brackets + a trailing manager), so this is a fresh implementation, not a reuse of `evaluateExit`. After implementing, validate by running the harness over the cached signals for the 6 study tickers and confirming the live rule's behavior matches.

**Where (grep to pin exact lines):**
- Broker-resident bracket exits (PR #435): `grep -rn "bracket\|TakeProfit\|GTC" internal/engine internal/execution` — the BUY that attaches a TP+SL bracket. **Change:** make the TP leg optional/omitted (or very wide) when the trailing rule is active, so it doesn't cap.
- Trailing-stop manager (#322): `grep -rn "trailing\|moveStopLeg\|atr_multiple\|hwm" internal/engine` — currently ATR-1.5× that PATCHes the SL leg up. **Change:** add a `close-below-9-EMA` trailing rule (needs a 9-EMA series — the backtest's `emaSeries` in `internal/backtest/runner.go` is a reference implementation; live likely uses `internal/indicators`).
- SELL-closes-long (PR #436/#437): `grep -rn "ActionSell\|closes.*long\|SELL" internal/engine` — **Change:** require the SELL analysis confidence ≥ `risk.exit_conf_min` (new knob) to close the long; else hold and let the trail manage.

**Config knobs to add (default-off, wire into `internal/config/config.go` + `config.example.yaml`; live config is `~/.tradeengine/config.yaml`):**
- `risk.trailing_stop.rule: atr | ema9` (default `atr` = current behavior)
- `risk.uncap_take_profit: false` (when true + a trailing rule is set, don't attach the fixed TP)
- `risk.exit_conf_min: 0.0` (SELL must clear this confidence to close a long; 0 = current behavior)

**Rollout:** default-off → enable in PAPER (`~/.tradeengine/config.yaml`, engine is `mode: paper`) → observe → operator decides on live. Recommended default once validated: `rule: ema9`, `uncap_take_profit: true`, `exit_conf_min: 0.72`. Do NOT default to ATR-trail (holds losers).

**Acceptance:** paper engine attaches no fixed TP on swing BUYs when configured; trailing SL follows the 9-EMA close; a low-conviction SELL does not close a long; existing baseline behavior preserved when the knobs are off; unit tests for the gate + the EMA trail.

---

## Phase 2 — entry-quality filter (ACTIVE — issue #472)
**Goal:** "buy strength, not weakness" — stop opening longs in downtrends (the OPEN/JMIA lesson: exits can't rescue bad entries). Add a trend-alignment gate: don't open a long when price is below declining 9/21 EMAs / making lower highs. Builds on existing `entry_gates` (PR #434: news tape-confirm, momentum anti-chase, first-tick deferral). Bigger piece — scope as its own effort after Branch 2. Consider adding 9/21 EMA + anchored-VWAP indicators (we only have 20/50/200 EMA + session VWAP today) since the pullback pattern keys on them.

**Initial implementation:** `risk.entry_gates.trend_alignment_enabled` is a default-off, BUY-only daily gate. It blocks only when live price is below both EMA9 and EMA21 and both completed-session EMAs are declining; non-daily or insufficient history fails open. Anchored VWAP, lower-high structure, and the full pullback-in-strength setup remain possible follow-ups rather than prerequisites for this conservative first gate.

## Conventions (for the takeover agent)
- Every feature: dedicated **branch → PR → `feature-dev:code-reviewer` before push → Monitor the PR**. Never push to `main` directly. Use a plain branch (NOT a worktree) unless asked.
- All new trading behavior is **default-off config + PAPER-first**. Live config is `~/.tradeengine/config.yaml` (NOT the repo's gitignored `./config.yaml`).
- After a PR merges, save a `project` memory summarizing it (see `MEMORY.md`).
- Relevant memories: `project_te_exit_policy_study_pr470`, `project_te_stock_signal_baseline`, `project_te_execution_study_pr433`, `project_te_bracket_exits_pr435`, `project_te_sell_exit_and_rth_guard`, `project_te_entry_quality_gates_pr434`.
