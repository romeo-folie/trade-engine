package study

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

// fakeBars serves canned candles per timeframe, standing in for Alpaca.
type fakeBars struct {
	minute []data.Candle
	daily  []data.Candle
}

func (f *fakeBars) FetchCandles(_ context.Context, _ string, tf data.Timeframe, from, to time.Time) ([]data.Candle, error) {
	src := f.minute
	if tf == data.Timeframe1Day {
		src = f.daily
	}
	var out []data.Candle
	for _, b := range src {
		if !b.Timestamp.Before(from) && !b.Timestamp.After(to) {
			out = append(out, b)
		}
	}
	return out, nil
}

// TestRunOne_NoBracketCreditAfterRecordedClose locks in the PR-review fix:
// a trade closed by another path (e.g. the trailing manager) whose fixed
// stop is only touched AFTER the recorded close must come back unresolved —
// not be credited with a post-close bracket exit and a negative lag.
func TestRunOne_NoBracketCreditAfterRecordedClose(t *testing.T) {
	et, _ := time.LoadLocation("America/New_York")
	open := time.Date(2026, 6, 1, 10, 0, 0, 0, et)   // Monday 10:00 ET
	closed := open.Add(30 * time.Minute)             // recorded close 10:30
	breach := closed.Add(20 * time.Minute)           // stop touched 10:50 — after the close

	var minute []data.Candle
	for ts := open; ts.Before(open.Add(2 * time.Hour)); ts = ts.Add(time.Minute) {
		b := data.Candle{Timestamp: ts, Open: 100, High: 100.5, Low: 99.5, Close: 100}
		if !ts.Before(breach) {
			// price collapses through the 98 stop only after the recorded close
			b = data.Candle{Timestamp: ts, Open: 97, High: 97.5, Low: 96.5, Close: 97}
		}
		minute = append(minute, b)
	}

	runner, err := NewRunner(&fakeBars{minute: minute}, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	res := runner.runOne(context.Background(), TradeCase{
		ID: "t1", Ticker: "TEST", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, ExitPrice: 99.8, Quantity: 10,
		StopLoss: 98, TakeProfit: 110, PnLPct: -0.2,
		OpenedAt: open, ClosedAt: closed,
		SignalPrice: 100, SignalAt: open, HasSignal: true,
	})
	if res.Skipped != "" {
		t.Fatalf("unexpected skip: %s", res.Skipped)
	}
	if res.SimExitReason != "unresolved" {
		t.Fatalf("got sim exit %q at %.2f, want unresolved — bracket credit leaked past the recorded close",
			res.SimExitReason, res.SimExitPrice)
	}
	// MAE must also be bounded at the close: the post-close collapse to 96.5
	// (3.5%) must not appear; within the hold the low is 99.5 (0.5%).
	if res.MAEPct > 1 {
		t.Fatalf("MAE %.2f%% includes post-close bars", res.MAEPct)
	}
}

// TestRunOne_EntryMinuteStopFires locks in the entry-anchor fix: a stop
// touched during the remainder of the entry's own 1-minute bar fires the
// bracket sim (a broker bracket is armed at entry, mid-minute).
func TestRunOne_EntryMinuteStopFires(t *testing.T) {
	et, _ := time.LoadLocation("America/New_York")
	barStart := time.Date(2026, 6, 1, 10, 0, 0, 0, et)
	open := barStart.Add(20 * time.Second) // entry mid-bar
	closed := barStart.Add(90 * time.Minute)

	minute := []data.Candle{
		{Timestamp: barStart, Open: 100, High: 100.2, Low: 97.9, Close: 98.2}, // entry bar wicks through the 98 stop
	}
	for ts := barStart.Add(time.Minute); ts.Before(closed.Add(time.Minute)); ts = ts.Add(time.Minute) {
		minute = append(minute, data.Candle{Timestamp: ts, Open: 99, High: 99.5, Low: 98.5, Close: 99})
	}

	runner, err := NewRunner(&fakeBars{minute: minute}, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	res := runner.runOne(context.Background(), TradeCase{
		ID: "t2", Ticker: "TEST", Direction: "long", Strategy: "momentum",
		EntryPrice: 100, ExitPrice: 99, Quantity: 10,
		StopLoss: 98, TakeProfit: 110, PnLPct: -1,
		OpenedAt: open, ClosedAt: closed,
		SignalPrice: 100, SignalAt: open, HasSignal: true,
	})
	if res.Skipped != "" {
		t.Fatalf("unexpected skip: %s", res.Skipped)
	}
	if res.SimExitReason != "stop" || res.SimExitPrice != 98 {
		t.Fatalf("got %q at %.2f, want stop at 98 fired in the entry minute",
			res.SimExitReason, res.SimExitPrice)
	}
}
