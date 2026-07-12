package study

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

func bar(t time.Time, o, h, l, c float64) data.Candle {
	return data.Candle{Timestamp: t, Open: o, High: h, Low: l, Close: c}
}

var t0 = time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC) // 10:00 ET

func mins(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

func TestSimMarketEntry_FillsAtFirstBarOpen(t *testing.T) {
	bars := []data.Candle{
		bar(mins(0), 100, 101, 99, 100.5),
		bar(mins(1), 100.5, 102, 100, 101),
	}
	fill := SimMarketEntry(bars, mins(1).Add(-30*time.Second))
	if !fill.OK || fill.Idx != 1 || fill.Price != 100.5 {
		t.Fatalf("got %+v, want fill at bar 1 open 100.5", fill)
	}
	if f := SimMarketEntry(bars, mins(5)); f.OK {
		t.Fatalf("expected no fill after last bar, got %+v", f)
	}
}

func TestSimLimitEntry_LongFillsOnTouchAndGapsFillAtOpen(t *testing.T) {
	bars := []data.Candle{
		bar(mins(0), 100, 101, 99.5, 100.5), // low 99.5 > limit 99 — no fill
		bar(mins(1), 100, 100.5, 98.9, 99),  // touches 99 → fill at limit
	}
	fill := SimLimitEntry(bars, mins(0), mins(60), "long", 99)
	if !fill.OK || fill.Idx != 1 || fill.Price != 99 {
		t.Fatalf("got %+v, want fill at limit 99 on bar 1", fill)
	}

	// Gap below the limit fills at the (better) open, not the limit.
	gapBars := []data.Candle{bar(mins(0), 98, 98.5, 97.5, 98)}
	fill = SimLimitEntry(gapBars, mins(0), mins(60), "long", 99)
	if !fill.OK || fill.Price != 98 {
		t.Fatalf("got %+v, want gap fill at open 98", fill)
	}
}

func TestSimLimitEntry_TTLExpires(t *testing.T) {
	bars := []data.Candle{
		bar(mins(0), 100, 101, 99.5, 100.5),
		bar(mins(10), 100, 100.5, 98, 99), // would fill, but past TTL
	}
	fill := SimLimitEntry(bars, mins(0), mins(5), "long", 99)
	if fill.OK {
		t.Fatalf("expected TTL to block the fill, got %+v", fill)
	}
}

func TestSimLimitEntry_Short(t *testing.T) {
	bars := []data.Candle{
		bar(mins(0), 100, 100.4, 99.5, 100), // high 100.4 < limit 100.5 — no fill
		bar(mins(1), 100, 100.6, 99.8, 100), // touches 100.5 → fill at limit
	}
	fill := SimLimitEntry(bars, mins(0), mins(60), "short", 100.5)
	if !fill.OK || fill.Idx != 1 || fill.Price != 100.5 {
		t.Fatalf("got %+v, want short fill at 100.5 on bar 1", fill)
	}
}

func TestSimBracketExit_StopWinsAmbiguousBar(t *testing.T) {
	// One bar touches both stop (98) and target (102) — conservative: stop first.
	bars := []data.Candle{bar(mins(0), 100, 103, 97, 101)}
	sim := SimBracketExit(bars, 0, "long", 98, 102)
	if sim.Reason != "stop" || sim.Price != 98 {
		t.Fatalf("got %+v, want stop at 98", sim)
	}
}

func TestSimBracketExit_GapThroughStopFillsAtOpen(t *testing.T) {
	bars := []data.Candle{bar(mins(0), 95, 96, 94, 95.5)} // opens below stop 98
	sim := SimBracketExit(bars, 0, "long", 98, 0)
	if sim.Reason != "stop" || sim.Price != 95 {
		t.Fatalf("got %+v, want gap stop fill at open 95", sim)
	}
}

func TestSimBracketExit_TargetLimitNeverFillsWorse(t *testing.T) {
	// Gap open above the target: limit sell fills at the better open.
	bars := []data.Candle{bar(mins(0), 104, 105, 103.5, 104.5)}
	sim := SimBracketExit(bars, 0, "long", 98, 102)
	if sim.Reason != "target" || sim.Price != 104 {
		t.Fatalf("got %+v, want target fill at open 104", sim)
	}
}

func TestSimBracketExit_UnresolvedMarksToLastClose(t *testing.T) {
	bars := []data.Candle{bar(mins(0), 100, 100.5, 99.5, 100.2)}
	sim := SimBracketExit(bars, 0, "long", 98, 102)
	if sim.Resolved || sim.Reason != "unresolved" || sim.Price != 100.2 {
		t.Fatalf("got %+v, want unresolved at last close 100.2", sim)
	}
}

func TestClassifyBreach(t *testing.T) {
	// Intraday: previous bar 1 minute earlier, breach bar opens above the stop.
	bars := []data.Candle{
		bar(mins(0), 100, 101, 99, 100),
		bar(mins(1), 99, 99.2, 97.5, 98), // opens 99 > stop 98, crosses intraday
	}
	if got := ClassifyBreach(bars, 1, "long", 98); got != BreachIntraday {
		t.Fatalf("got %s, want intraday", got)
	}
	// Gap: 17h session break and the bar opens already below the stop.
	bars = []data.Candle{
		bar(mins(0), 100, 101, 99, 100),
		bar(mins(0).Add(17*time.Hour), 96, 97, 95, 96),
	}
	if got := ClassifyBreach(bars, 1, "long", 98); got != BreachGap {
		t.Fatalf("got %s, want gap", got)
	}
}

func TestExcursions(t *testing.T) {
	bars := []data.Candle{
		bar(mins(0), 100, 101, 98, 100), // low 98 → MAE 2%
		bar(mins(1), 100, 105, 99, 104), // high 105 → MFE 5%
	}
	mae, mfe := Excursions(bars, 0, 1, "long", 100)
	if mae != 2 || mfe != 5 {
		t.Fatalf("got mae=%.2f mfe=%.2f, want 2 and 5", mae, mfe)
	}
	// Short direction mirrors: adverse is up, favorable is down.
	mae, mfe = Excursions(bars, 0, 1, "short", 100)
	if mae != 5 || mfe != 2 {
		t.Fatalf("short: got mae=%.2f mfe=%.2f, want 5 and 2", mae, mfe)
	}
}

func TestATR14(t *testing.T) {
	daily := make([]data.Candle, 15)
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i := range daily {
		// Constant 2-point range, no gaps: TR = 2 every day → ATR = 2.
		daily[i] = bar(day.AddDate(0, 0, i), 100, 101, 99, 100)
	}
	if atr := ATR14(daily); atr != 2 {
		t.Fatalf("got %.4f, want 2", atr)
	}
	if atr := ATR14(daily[:14]); atr != 0 {
		t.Fatalf("insufficient bars should return 0, got %.4f", atr)
	}
}

func TestSimTrailingExit_RatchetsAndNoIntrabarLookahead(t *testing.T) {
	// ATR 1, mult 1: after bar 0 (high 105) the stop ratchets to 104.
	// Bar 1's low touches 104 → trail exit at 104, NOT at bar 1's own
	// raised level (bar 1 high 106 would imply 105 — lookahead).
	bars := []data.Candle{
		bar(mins(0), 100, 105, 100, 104.5),
		bar(mins(1), 104.5, 106, 103.9, 104),
	}
	sim := SimTrailingExit(bars, 0, "long", 100, 95, 0, 1, 1)
	if sim.Reason != "stop" || sim.Price != 104 {
		t.Fatalf("got %+v, want trail stop at 104", sim)
	}
}

func TestSimTrailingExit_NeverBelowOriginalStop(t *testing.T) {
	// Price falls immediately: the trail never ratchets above the original
	// stop, so the exit is the original 98.
	bars := []data.Candle{
		bar(mins(0), 100, 100.2, 99, 99.5),
		bar(mins(1), 99.5, 99.6, 97.5, 98),
	}
	sim := SimTrailingExit(bars, 0, "long", 100, 98, 0, 1, 5)
	if sim.Reason != "stop" || sim.Price != 98 {
		t.Fatalf("got %+v, want original stop 98", sim)
	}
}

func TestSimTrailingExit_ZeroATRFallsBackToBracket(t *testing.T) {
	bars := []data.Candle{bar(mins(0), 100, 103, 99, 102)}
	sim := SimTrailingExit(bars, 0, "long", 100, 98, 102, 0, 1.5)
	if sim.Reason != "target" || sim.Price != 102 {
		t.Fatalf("got %+v, want bracket target 102", sim)
	}
}

func TestPctReturnDirections(t *testing.T) {
	if got := pctReturn("long", 100, 105); got != 5 {
		t.Fatalf("long: got %.2f, want 5", got)
	}
	if got := pctReturn("short", 100, 95); got != 5 {
		t.Fatalf("short: got %.2f, want 5", got)
	}
}
