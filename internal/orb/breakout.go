package orb

import (
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

// FirstBreakout applies the live resting-stop fill rule to a post-range candle
// stream: the first bar whose High crosses the buy entry stop or whose Low crosses
// the sell entry stop wins; a bar that trips both resolves by its own direction
// (close vs open). Bars at/after entryDeadline are ignored — live would have
// cancelled the resting entry orders by then. A side with Qty<=0 (sizing said
// "cannot trade") or no entry stop is not eligible.
//
// Shared by the forex (orbreplay) and options (optionsreplay) backtests so the
// live-parity fill rule cannot drift between them.
func FirstBreakout(watch []data.Candle, buy, sell BracketParams, entryDeadline time.Time) (side string, idx int, ok bool) {
	for i, c := range watch {
		if !c.Timestamp.Before(entryDeadline) {
			break // past the entry window — live would have cancelled the orders
		}
		buyHit := buy.Qty > 0 && buy.EntryStop > 0 && c.High >= buy.EntryStop
		sellHit := sell.Qty > 0 && sell.EntryStop > 0 && c.Low <= sell.EntryStop
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
