package orb

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

func confBar(min int, o, h, l, c float64) data.Candle {
	return data.Candle{
		Timestamp: time.Date(2026, 4, 13, 8, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute),
		Open:      o, High: h, Low: l, Close: c,
	}
}

func TestRequiresConfirmation(t *testing.T) {
	for _, m := range []string{"", "stop_touch"} {
		if RequiresConfirmation(m) {
			t.Errorf("RequiresConfirmation(%q) = true; want false", m)
		}
	}
	for _, m := range []string{"close_1m", "close_3m"} {
		if !RequiresConfirmation(m) {
			t.Errorf("RequiresConfirmation(%q) = false; want true", m)
		}
	}
}

func TestDetectConfirmation_StopTouch(t *testing.T) {
	const buyTrig, sellTrig = 1.1012, 1.0998
	// A wick to 1.1015 high (no close beyond) still confirms a stop-touch buy at bar 0.
	got, idx, ok := DetectConfirmation("stop_touch", []data.Candle{
		confBar(0, 1.1009, 1.1015, 1.1008, 1.1010),
	}, buyTrig, sellTrig)
	if !ok || got != "buy" || idx != 0 {
		t.Fatalf("got (%q, %d, %v); want (buy, 0, true)", got, idx, ok)
	}
}

func TestDetectConfirmation_Close1m(t *testing.T) {
	const buyTrig, sellTrig = 1.1012, 1.0998
	t.Run("wick high but close inside does not confirm", func(t *testing.T) {
		_, _, ok := DetectConfirmation("close_1m", []data.Candle{
			confBar(0, 1.1009, 1.1015, 1.1008, 1.1010), // high 1.1015 ≥ trig but close 1.1010 inside
		}, buyTrig, sellTrig)
		if ok {
			t.Error("a wick that closes back inside must not confirm close_1m")
		}
	})
	t.Run("one close beyond confirms", func(t *testing.T) {
		got, idx, ok := DetectConfirmation("close_1m", []data.Candle{
			confBar(0, 1.1009, 1.1011, 1.1008, 1.1010), // inside
			confBar(1, 1.1010, 1.1016, 1.1009, 1.1014), // close 1.1014 ≥ buyTrig → confirm
		}, buyTrig, sellTrig)
		if !ok || got != "buy" || idx != 1 {
			t.Fatalf("got (%q, %d, %v); want (buy, 1, true)", got, idx, ok)
		}
	})
}

func TestDetectConfirmation_Close3m(t *testing.T) {
	const buyTrig, sellTrig = 1.1012, 1.0998
	t.Run("two closes beyond is not enough", func(t *testing.T) {
		_, _, ok := DetectConfirmation("close_3m", []data.Candle{
			confBar(0, 1.1010, 1.1016, 1.1009, 1.1014),
			confBar(1, 1.1014, 1.1016, 1.1011, 1.1013),
		}, buyTrig, sellTrig)
		if ok {
			t.Error("two consecutive closes must not confirm close_3m")
		}
	})
	t.Run("three consecutive closes confirm at the third", func(t *testing.T) {
		got, idx, ok := DetectConfirmation("close_3m", []data.Candle{
			confBar(0, 1.1010, 1.1016, 1.1009, 1.1014),
			confBar(1, 1.1014, 1.1016, 1.1011, 1.1013),
			confBar(2, 1.1013, 1.1016, 1.1011, 1.1015), // third consecutive → confirm
		}, buyTrig, sellTrig)
		if !ok || got != "buy" || idx != 2 {
			t.Fatalf("got (%q, %d, %v); want (buy, 2, true)", got, idx, ok)
		}
	})
	t.Run("a close back inside resets the streak", func(t *testing.T) {
		_, _, ok := DetectConfirmation("close_3m", []data.Candle{
			confBar(0, 1.1010, 1.1016, 1.1009, 1.1014), // beyond
			confBar(1, 1.1014, 1.1016, 1.1009, 1.1010), // back inside → reset
			confBar(2, 1.1013, 1.1016, 1.1011, 1.1015), // beyond (streak only 1)
			confBar(3, 1.1013, 1.1016, 1.1011, 1.1015), // beyond (streak 2)
		}, buyTrig, sellTrig)
		if ok {
			t.Error("a close back inside must reset the streak, so 3 total but not consecutive must not confirm")
		}
	})
}

func TestDetectConfirmation_ShortSide(t *testing.T) {
	const buyTrig, sellTrig = 1.1012, 1.0998
	got, _, ok := DetectConfirmation("close_1m", []data.Candle{
		confBar(0, 1.1000, 1.1001, 1.0996, 1.0997), // close 1.0997 ≤ sellTrig → confirm sell
	}, buyTrig, sellTrig)
	if !ok || got != "sell" {
		t.Fatalf("got (%q, %v); want (sell, true)", got, ok)
	}
}

func TestConfirmedSide(t *testing.T) {
	r := &OpeningRange{Ticker: "EUR/USD", High: 1.1010, Low: 1.1000,
		DefinedAt: time.Date(2026, 4, 13, 8, 14, 0, 0, time.UTC)}
	buy := BracketParams{Side: "buy", EntryStop: 1.1012}
	sell := BracketParams{Side: "sell", EntryStop: 1.0998}
	now := func() time.Time { return time.Date(2026, 4, 13, 8, 30, 0, 0, time.UTC) }

	t.Run("close beyond confirms", func(t *testing.T) {
		md := &qMarketData{candles: []data.Candle{confBar(15, 1.1011, 1.1016, 1.1010, 1.1014)}} // close 1.1014 ≥ 1.1012
		s := &ORBScheduler{marketData: md, now: now}
		if side := s.confirmedSide(context.Background(), "EUR/USD", r, buy, sell, "close_1m"); side != "buy" {
			t.Errorf("confirmedSide = %q; want buy", side)
		}
	})
	t.Run("no close beyond does not confirm", func(t *testing.T) {
		md := &qMarketData{candles: []data.Candle{confBar(15, 1.1011, 1.1016, 1.1010, 1.1011)}} // wick only, closes inside
		s := &ORBScheduler{marketData: md, now: now}
		if side := s.confirmedSide(context.Background(), "EUR/USD", r, buy, sell, "close_1m"); side != "" {
			t.Errorf("confirmedSide = %q; want empty", side)
		}
	})
	t.Run("nil market data does not confirm", func(t *testing.T) {
		s := &ORBScheduler{now: now}
		if side := s.confirmedSide(context.Background(), "EUR/USD", r, buy, sell, "close_1m"); side != "" {
			t.Errorf("confirmedSide = %q; want empty (no data → no confirmation, never fail-open)", side)
		}
	})
}

func TestWithinEntryWindow(t *testing.T) {
	fx := &config.ForexORBConfig{Enabled: true, Session: "london", Timezone: "UTC"} // cutoff 09:00 UTC
	at := func(h, m int) *ORBScheduler {
		return &ORBScheduler{forexCfg: fx, now: func() time.Time { return time.Date(2026, 4, 13, h, m, 0, 0, time.UTC) }}
	}
	if !at(8, 30).withinEntryWindow() {
		t.Error("08:30 UTC should be within the London entry window (cutoff 09:00)")
	}
	if at(9, 30).withinEntryWindow() {
		t.Error("09:30 UTC should be past the entry window")
	}
}
