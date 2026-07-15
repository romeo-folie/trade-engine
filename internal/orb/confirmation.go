package orb

import (
	"context"
	"log/slog"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

// Breakout-confirmation modes (Phase 3). See config.Instrument.BreakoutConfirmation.
const (
	ConfirmStopTouch = "stop_touch" // resting stop fills on a touch (current behaviour)
	ConfirmClose1m   = "close_1m"   // one 1-minute close beyond the breakout level
	ConfirmClose3m   = "close_3m"   // three consecutive 1-minute closes beyond it
)

// RequiresConfirmation reports whether a mode needs candle-close confirmation rather than
// the default touch-fill. "" and "stop_touch" do not; the close_* modes do.
func RequiresConfirmation(mode string) bool {
	switch mode {
	case ConfirmClose1m, ConfirmClose3m:
		return true
	default:
		return false
	}
}

// confirmCloses returns the number of consecutive 1-minute closes a mode requires beyond
// the breakout level (1 for close_1m, 3 for close_3m). 0 for non-close modes.
func confirmCloses(mode string) int {
	switch mode {
	case ConfirmClose1m:
		return 1
	case ConfirmClose3m:
		return 3
	default:
		return 0
	}
}

// DetectConfirmation walks 1-minute candles and returns the first confirmed breakout under
// the given mode: the side ("buy"/"sell"), the index of the candle AT which confirmation
// completes, and ok. buyTrigger is the long breakout level (range high + buffer), sellTrigger
// the short level (range low − buffer); a non-positive trigger disables that side.
//
//   - stop_touch (or ""): the first bar whose high reaches buyTrigger or low reaches
//     sellTrigger (a touch); if a single bar straddles both, the bar's own direction breaks
//     the tie (bullish → buy, bearish → sell).
//   - close_1m / close_3m: the first bar at which there have been the required number of
//     consecutive 1-minute closes beyond a trigger. A close back inside resets that side's
//     streak, so a wick that closes back in does not confirm.
//
// This is the single source of breakout-confirmation truth shared by the live scheduler and
// the backtest, so they cannot diverge.
func DetectConfirmation(mode string, candles []data.Candle, buyTrigger, sellTrigger float64) (side string, idx int, ok bool) {
	if !RequiresConfirmation(mode) {
		for i, c := range candles {
			buyHit := buyTrigger > 0 && c.High >= buyTrigger
			sellHit := sellTrigger > 0 && c.Low <= sellTrigger
			switch {
			case buyHit && sellHit:
				if c.Close >= c.Open {
					return "buy", i, true
				}
				return "sell", i, true
			case buyHit:
				return "buy", i, true
			case sellHit:
				return "sell", i, true
			}
		}
		return "", 0, false
	}

	need := confirmCloses(mode)
	var buyStreak, sellStreak int
	for i, c := range candles {
		if buyTrigger > 0 && c.Close >= buyTrigger {
			buyStreak++
		} else {
			buyStreak = 0
		}
		if sellTrigger > 0 && c.Close <= sellTrigger {
			sellStreak++
		} else {
			sellStreak = 0
		}
		if buyStreak >= need {
			return "buy", i, true
		}
		if sellStreak >= need {
			return "sell", i, true
		}
	}
	return "", 0, false
}

// confirmedSide replays the 1-minute candles since the opening range locked and returns
// the side ("buy"/"sell") whose breakout is confirmed under the given mode, or "" if
// neither has confirmed yet. Unlike the entry-quality filters this does NOT fail open:
// if candles can't be fetched there is no confirmation, so no trade is placed — the safe
// direction for a confirmation gate. Only called for confirmation modes.
func (s *ORBScheduler) confirmedSide(ctx context.Context, ticker string, r *OpeningRange, buy, sell BracketParams, mode string) string {
	if s.marketData == nil || r == nil {
		return ""
	}
	candles, err := s.marketData.FetchCandles(ctx, ticker, data.Timeframe1Min, r.DefinedAt, s.now())
	if err != nil {
		slog.Warn("orb: confirmation: fetch candles failed — not confirming this tick", "ticker", ticker, "err", err)
		return ""
	}
	// Respect direction eligibility: a side zeroed out (Qty 0) is not a candidate, so its
	// trigger is disabled (0) and DetectConfirmation will never confirm it.
	buyTrig, sellTrig := buy.EntryStop, sell.EntryStop
	if buy.Qty == 0 {
		buyTrig = 0
	}
	if sell.Qty == 0 {
		sellTrig = 0
	}
	side, _, ok := DetectConfirmation(mode, candles, buyTrig, sellTrig)
	if !ok {
		return ""
	}
	return side
}

// retestEntry replays the 1-minute candles since the opening range locked and, if a retest
// bounce has triggered, returns the retest MARKET bracket (entry at the bounce, stop at the
// retest swing or the range-opposite level per mode, RR target, re-sized qty) and its side.
// ok=false when no retest has triggered yet — the caller retries next tick. Like confirmedSide
// this does NOT fail open: no candles or no bounce means no entry, the safe direction for an
// entry gate. Only called for retest_* modes. cfg/inst are the effective (mode-mapped) config.
func (s *ORBScheduler) retestEntry(ctx context.Context, ticker string, r *OpeningRange, buy, sell BracketParams, cfg config.ForexORBConfig, inst config.Instrument, portfolioValue float64) (BracketParams, string, bool) {
	if s.marketData == nil || r == nil {
		return BracketParams{}, "", false
	}
	candles, err := s.marketData.FetchCandles(ctx, ticker, data.Timeframe1Min, r.DefinedAt, s.now())
	if err != nil {
		slog.Warn("orb: retest: fetch candles failed — not entering this tick", "ticker", ticker, "err", err)
		return BracketParams{}, "", false
	}
	// A zeroed side (direction eligibility / commission) is not a candidate: disable its trigger.
	buyTrig, sellTrig := buy.EntryStop, sell.EntryStop
	if buy.Qty == 0 {
		buyTrig = 0
	}
	if sell.Qty == 0 {
		sellTrig = 0
	}
	tol := inst.RetestTolerance
	if tol <= 0 {
		tol = 0.25
	}
	timeoutBars := inst.RetestTimeoutMinutes
	if timeoutBars <= 0 {
		timeoutBars = 30
	}
	res := DetectRetest(candles, buyTrig, sellTrig, r.High, r.Low, inst.PipBuffer, tol, timeoutBars)
	if !res.OK {
		return BracketParams{}, "", false
	}
	// Stop placement: retest_swing → just beyond the retest swing (tighter); retest_range →
	// the opposite side of the opening range (the range bracket's stop). Mirrors the backtest.
	var stopLevel float64
	if inst.BreakoutConfirmation == ConfirmRetestSwing {
		if res.Side == "buy" {
			stopLevel = res.SwingLow - inst.PipBuffer
		} else {
			stopLevel = res.SwingHigh + inst.PipBuffer
		}
	} else { // retest_range
		if res.Side == "buy" {
			stopLevel = buy.StopLoss
		} else {
			stopLevel = sell.StopLoss
		}
	}
	rb := ComputeRetestBracket(ticker, res.Side, res.EntryPrice, stopLevel, portfolioValue, cfg, inst)
	return rb, res.Side, true
}

// withinEntryWindow reports whether the current time is before the session cleanup cutoff
// (60 minutes after open, in the session timezone) — i.e. unfilled entries would not yet
// have been cancelled live. The breakout-watch keeps watching only while this holds.
func (s *ORBScheduler) withinEntryWindow() bool {
	sn := s.now().In(s.sessionTimezone())
	h, m, _ := sn.Clock()
	return h*60+m < s.sessionCleanupMinutes()
}
