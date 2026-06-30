package orb

import (
	"context"
	"log/slog"

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
	side, _, ok := DetectConfirmation(mode, candles, buy.EntryStop, sell.EntryStop)
	if !ok {
		return ""
	}
	return side
}

// withinEntryWindow reports whether the current time is before the session cleanup cutoff
// (60 minutes after open, in the session timezone) — i.e. unfilled entries would not yet
// have been cancelled live. The breakout-watch keeps watching only while this holds.
func (s *ORBScheduler) withinEntryWindow() bool {
	sn := s.now().In(s.sessionTimezone())
	h, m, _ := sn.Clock()
	return h*60+m < s.sessionCleanupMinutes()
}
