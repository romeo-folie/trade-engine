package orb

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// appendForcedOptionsUnderlyings injects the options-router underlyings into the scan
// result as synthetic candidates. Options-mode symbols (e.g. SPY/IWM/NVDA) are FIXED,
// rule-based ORB targets — they rarely gap, so the LLM gap scan never surfaces them.
// Injecting them here (every scan, independent of the gap scan) is what makes the live
// path watch them the way the backtest (optionsreplay.AssembleSignals) does, even on a
// day with zero gap movers or when the LLM scorer is rate-limited.
//
// They are added to the persisted `scored` set — NOT just `selected` — because a
// mid-session resume rehydrates s.candidates from `scored`; injecting only into
// `selected` would silently drop the force-watch after a restart. Conviction 0 and a
// zero GapPct are deliberate: the gap-sign direction consumer never sees these tickers
// (tickExecuting intercepts options-router tickers before the gap-sign branch), and the
// dual-sided breakout entry decides call vs put from the actual breakout.
//
// No-op when options-ORB is disabled: Underlyings() is nil-safe and returns nil on a
// nil router, leaving the equity/forex/futures scan path byte-identical.
func appendForcedOptionsUnderlyings(scored []ScoredCandidate, router *OptionsRouter) []ScoredCandidate {
	for _, u := range router.Underlyings() {
		if i := indexOfTicker(scored, u); i >= 0 {
			// The gap scan already surfaced this underlying. Force it to "trade" regardless
			// of the LLM's action: a configured options underlying is watched BY RULE, so an
			// organic "skip"/"HOLD" score must not drop it from the selected filter below (its
			// gap%/conviction/reasoning are left intact for display). Already-"trade" is a no-op.
			if !isTradeAction(scored[i].Score.Action) {
				scored[i].Score.Action = "trade"
			}
			// This underlying is watched for options execution regardless of how the
			// gap scan surfaced it, so tag it "option" for the UI (overriding any
			// "equity" tag from the gapper path).
			scored[i].AssetClass = "option"
			continue
		}
		scored = append(scored, ScoredCandidate{
			AssetClass: "option",
			Gap:        data.GapCandidate{Ticker: u},
			Score:      ORBAnalysisResult{Action: "trade", Conviction: 0, Reasoning: "options-router force-watch"},
		})
	}
	return scored
}

// ensureForcedOptionsSelected re-adds any options-router underlying that the
// MaxCandidates truncation evicted from `selected`. Forced entries carry Conviction 0,
// so they sort last and a tight cap would drop them first — this guarantees the
// force-watch survives the cap. Pulls the (already-injected) entry back out of `scored`.
func ensureForcedOptionsSelected(selected, scored []ScoredCandidate, router *OptionsRouter) []ScoredCandidate {
	for _, u := range router.Underlyings() {
		if containsTicker(selected, u) {
			continue
		}
		for _, sc := range scored {
			if strings.EqualFold(sc.Gap.Ticker, u) {
				selected = append(selected, sc)
				break
			}
		}
	}
	return selected
}

func indexOfTicker(cands []ScoredCandidate, ticker string) int {
	for i, sc := range cands {
		if strings.EqualFold(sc.Gap.Ticker, ticker) {
			return i
		}
	}
	return -1
}

func containsTicker(cands []ScoredCandidate, ticker string) bool {
	return indexOfTicker(cands, ticker) >= 0
}

// candlesAfter returns the candles strictly after t. The breakout watch must start
// AFTER the range-lock bar (matching AssembleSignals' watch = bars[rangeEndIdx+1:]);
// confirmedSide fetches from r.DefinedAt inclusive, which would let the lock bar itself
// fire a spurious breakout. Allocates a fresh slice so the input is untouched.
func candlesAfter(candles []data.Candle, t time.Time) []data.Candle {
	out := make([]data.Candle, 0, len(candles))
	for _, c := range candles {
		if c.Timestamp.After(t) {
			out = append(out, c)
		}
	}
	return out
}

// optionsBreakoutEntry runs one tick of the dual-sided, breakout-direction ORB for a
// force-watched options underlying, mirroring optionsreplay.AssembleSignals: it computes
// buy+sell brackets from the locked opening range, replays the post-range 1-minute
// candles, and takes the FIRST breakout (orb.FirstBreakout — the exact shared fill rule
// the backtest uses) within the entry window (session open + 60m). The winning side is
// routed to a long option via routeOptionsEntry (buy→call, sell→put).
//
// Returns done=true once the session's single entry attempt is spent — either an order
// was placed, or the breakout fired but the option gate/broker aborted it (one attempt
// per session, matching the backtest's single as-of gate evaluation; s.optionsAttempted
// prevents re-evaluation on a later, drifted chain). Returns done=false while no breakout
// has fired yet so the caller keeps the session in Executing and retries next tick until
// the entry window closes.
func (s *ORBScheduler) optionsBreakoutEntry(ctx context.Context, sessionID, ticker string, r *OpeningRange, portfolioVal float64) (done bool, line string) {
	if s.optionsAttempted[ticker] {
		return true, "" // already spent this session's entry attempt
	}
	if r == nil || s.marketData == nil {
		return false, ""
	}

	buy := ComputeBracket(r, "buy", portfolioVal, s.cfg)
	sell := ComputeBracket(r, "sell", portfolioVal, s.cfg)

	candles, err := s.marketData.FetchCandles(ctx, ticker, data.Timeframe1Min, r.DefinedAt, s.now())
	if err != nil {
		slog.Warn("orb: options breakout-watch: fetch candles failed — retry next tick", "ticker", ticker, "err", err)
		return false, "" // no data this tick → no trade, keep watching
	}
	watch := candlesAfter(candles, r.DefinedAt)

	cm := s.sessionCleanupMinutes()
	sd := s.now().In(s.sessionTimezone())
	entryDeadline := time.Date(sd.Year(), sd.Month(), sd.Day(), cm/60, cm%60, 0, 0, s.sessionTimezone())

	side, _, ok := FirstBreakout(watch, buy, sell, entryDeadline)
	if !ok {
		return false, "" // no breakout yet — keep watching this session
	}

	// First breakout fired: spend the session's one entry attempt regardless of the gate
	// outcome, so a gate/broker abort isn't re-evaluated against a later, drifted chain.
	s.optionsAttempted[ticker] = true
	// A gate/broker abort creates no orb_trades row, so this KV entry is the ONLY record of
	// the spent attempt across a restart — without it, a process restart before the entry
	// window closes would rebuild optionsAttempted empty and replay the same breakout against
	// a later chain (placing a trade the one-attempt/backtest-parity would have skipped).
	s.persistOptionsAttempted(ctx, sessionID)

	params := buy
	if side == "sell" {
		params = sell
	}
	orderID, line := s.routeOptionsEntry(ctx, sessionID, ticker, side, params)
	if orderID != "" {
		// Options have no broker bracket to mark the ticker as entered, so record the
		// order ID for the per-ticker re-entry guard.
		if side == "sell" {
			s.orderIDs[ticker] = [2]string{"", orderID}
		} else {
			s.orderIDs[ticker] = [2]string{orderID, ""}
		}
		s.publish(events.TradeSimulated, events.TradeExecutedPayload{
			Ticker: ticker, Direction: side, Strategy: "orb",
		})
	}
	return true, line
}

// optionsScoringCutoffReached reports whether the equities LLM scoring loop should stop
// early (leaving optionsScanCutoffBufferMin before session open) so the time-critical
// options force-watch reaches candidate_ready before open. now is compared in the session
// timezone against openMinutes (minutes-of-day, session-local).
func optionsScoringCutoffReached(now time.Time, tz *time.Location, openMinutes int) bool {
	n := now.In(tz)
	h, m, _ := n.Clock()
	return h*60+m >= openMinutes-optionsScanCutoffBufferMin
}

func optionsAttemptedKey(sessionID string) string { return "options_attempted:" + sessionID }

// persistOptionsAttempted best-effort saves this session's spent-attempt set to engine_state
// so a mid-session restart restores it (see the call site — gate aborts leave no orb_trades
// row, so this KV entry is the only cross-restart record). The whole set is written under one
// session-keyed value; a failure is logged but never blocks the entry (the in-memory set still
// guards the running process). Stale keys from past sessions are harmless — keyed by session
// ID, they are never re-read.
func (s *ORBScheduler) persistOptionsAttempted(ctx context.Context, sessionID string) {
	// store.SetEngineState → GetDB() PANICS on a DB-less engine start (unlike GetEngineState,
	// it has no IsConnected guard), so gate on IsConnected here — mirroring
	// engine.PersistReconciliation. The in-memory set still guards the running process.
	if sessionID == "" || !store.IsConnected() {
		return
	}
	spent := make([]string, 0, len(s.optionsAttempted))
	for t := range s.optionsAttempted {
		spent = append(spent, t)
	}
	b, err := json.Marshal(spent)
	if err != nil {
		return
	}
	if err := store.SetEngineState(ctx, optionsAttemptedKey(sessionID), string(b)); err != nil {
		slog.Warn("orb options: failed to persist spent-attempt set — a restart may replay a gate-aborted breakout",
			"session", sessionID, "err", err)
	}
}

// loadOptionsAttempted best-effort restores the spent-attempt set on resume so a restart
// within the entry window does not replay an already-decided breakout. No-op when the map is
// unset or nothing was persisted.
func (s *ORBScheduler) loadOptionsAttempted(ctx context.Context, sessionID string) {
	if sessionID == "" || s.optionsAttempted == nil {
		return
	}
	val, ok, err := store.GetEngineState(ctx, optionsAttemptedKey(sessionID))
	if err != nil || !ok || val == "" {
		return
	}
	var spent []string
	if json.Unmarshal([]byte(val), &spent) != nil {
		return
	}
	for _, t := range spent {
		s.optionsAttempted[t] = true
	}
}
