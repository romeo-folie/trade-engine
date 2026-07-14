package orb

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/events"
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
		if containsTicker(scored, u) {
			continue // an organic gap candidate already covers it — don't double-watch
		}
		scored = append(scored, ScoredCandidate{
			Gap:   data.GapCandidate{Ticker: u},
			Score: ORBAnalysisResult{Action: "trade", Conviction: 0, Reasoning: "options-router force-watch"},
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

func containsTicker(cands []ScoredCandidate, ticker string) bool {
	for _, sc := range cands {
		if strings.EqualFold(sc.Gap.Ticker, ticker) {
			return true
		}
	}
	return false
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
