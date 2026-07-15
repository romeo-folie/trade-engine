package orb

import (
	"context"
	"math"
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
	buy := BracketParams{Side: "buy", EntryStop: 1.1012, Qty: 5000}
	sell := BracketParams{Side: "sell", EntryStop: 1.0998, Qty: 5000}
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

func TestConfirmedSide_RespectsDirectionEligibility(t *testing.T) {
	// A buy that closed beyond its trigger, but the buy side was ruled ineligible (Qty 0)
	// by direction eligibility — confirmedSide must not confirm it.
	r := &OpeningRange{Ticker: "EUR/USD", High: 1.1010, Low: 1.1000,
		DefinedAt: time.Date(2026, 4, 13, 8, 14, 0, 0, time.UTC)}
	buy := BracketParams{Side: "buy", EntryStop: 1.1012, Qty: 0}                            // ineligible
	sell := BracketParams{Side: "sell", EntryStop: 1.0998, Qty: 5000}                       // eligible
	md := &qMarketData{candles: []data.Candle{confBar(15, 1.1011, 1.1016, 1.1010, 1.1014)}} // closes above buy trig
	s := &ORBScheduler{marketData: md, now: func() time.Time { return time.Date(2026, 4, 13, 8, 30, 0, 0, time.UTC) }}
	if side := s.confirmedSide(context.Background(), "EUR/USD", r, buy, sell, "close_1m"); side != "" {
		t.Errorf("confirmedSide = %q; want empty (buy was ruled ineligible)", side)
	}
}

func TestDirectionEligibility(t *testing.T) {
	s := func(emaCandles []data.Candle) *ORBScheduler {
		return &ORBScheduler{
			marketData: &qMarketData{candles: emaCandles},
			forexCfg:   &config.ForexORBConfig{Enabled: true, TrendTimeframe: "1h", TrendEMAPeriod: 20},
			now:        func() time.Time { return time.Date(2026, 4, 13, 9, 0, 0, 0, time.UTC) },
		}
	}
	// 20 hourly bars climbing 1.1000→1.1019: EMA < last close → uptrend → long only.
	up := make([]data.Candle, 0, 21)
	base := time.Date(2026, 4, 12, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 21; i++ {
		px := 1.1000 + float64(i)*0.0001
		up = append(up, data.Candle{Timestamp: base.Add(time.Duration(i) * time.Hour), Open: px, High: px, Low: px, Close: px})
	}
	longOK, shortOK, ok := s(up).directionEligibility(context.Background(), "EUR/USD")
	if !ok || !longOK || shortOK {
		t.Errorf("uptrend: got long=%v short=%v ok=%v; want long-only", longOK, shortOK, ok)
	}
	// Reverse for a downtrend → short only.
	down := make([]data.Candle, 0, 21)
	for i := 0; i < 21; i++ {
		px := 1.1020 - float64(i)*0.0001
		down = append(down, data.Candle{Timestamp: base.Add(time.Duration(i) * time.Hour), Open: px, High: px, Low: px, Close: px})
	}
	longOK, shortOK, ok = s(down).directionEligibility(context.Background(), "EUR/USD")
	if !ok || longOK || !shortOK {
		t.Errorf("downtrend: got long=%v short=%v ok=%v; want short-only", longOK, shortOK, ok)
	}
	// Thin data → fail open (ok=false).
	if _, _, ok := s(up[:5]).directionEligibility(context.Background(), "EUR/USD"); ok {
		t.Error("thin data should report ok=false (fail open)")
	}
}

func TestDirectionDecision_Latched(t *testing.T) {
	// 21 climbing hourly bars → uptrend → long-only on the first call.
	up := make([]data.Candle, 0, 21)
	base := time.Date(2026, 4, 12, 12, 0, 0, 0, time.UTC)
	for i := range 21 {
		px := 1.1000 + float64(i)*0.0001
		up = append(up, data.Candle{Timestamp: base.Add(time.Duration(i) * time.Hour), Open: px, High: px, Low: px, Close: px})
	}
	md := &qMarketData{candles: up}
	s := &ORBScheduler{
		marketData:         md,
		forexCfg:           &config.ForexORBConfig{Enabled: true, TrendTimeframe: "1h", TrendEMAPeriod: 20},
		now:                func() time.Time { return time.Date(2026, 4, 13, 9, 0, 0, 0, time.UTC) },
		directionDecisions: make(map[string][2]bool),
	}
	long1, short1 := s.directionDecision(context.Background(), "EUR/USD")
	if !long1 || short1 {
		t.Fatalf("first decision: long=%v short=%v; want long-only (uptrend)", long1, short1)
	}

	// Flip the market to a downtrend — the latched decision must NOT change.
	down := make([]data.Candle, 0, 21)
	for i := range 21 {
		px := 1.1040 - float64(i)*0.0001
		down = append(down, data.Candle{Timestamp: base.Add(time.Duration(i) * time.Hour), Open: px, High: px, Low: px, Close: px})
	}
	md.candles = down
	long2, short2 := s.directionDecision(context.Background(), "EUR/USD")
	if long2 != long1 || short2 != short1 {
		t.Errorf("decision drifted after the market flipped: was (%v,%v), now (%v,%v) — must be latched", long1, short1, long2, short2)
	}
}

func TestRetestEntry(t *testing.T) {
	// Range 1.1000-1.1010, buffer 0.0002 → buy trigger 1.1012. rangeWidth 0.0010, tol 0.00025.
	r := &OpeningRange{Ticker: "EUR/USD", High: 1.1010, Low: 1.1000,
		DefinedAt: time.Date(2026, 4, 13, 8, 14, 0, 0, time.UTC)}
	buy := BracketParams{Side: "buy", EntryStop: 1.1012, StopLoss: 1.0998, Qty: 5000}
	sell := BracketParams{Side: "sell", EntryStop: 1.0998, StopLoss: 1.1012, Qty: 5000}
	cfg := config.ForexORBConfig{RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 0}
	inst := config.Instrument{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 1, PipBuffer: 0.0002, BreakoutConfirmation: "retest_range"}
	now := func() time.Time { return time.Date(2026, 4, 13, 8, 30, 0, 0, time.UTC) }

	// breakout (close ≥ 1.1012) → pullback (low within tol, close inside not breached) → bounce
	// (high ≥ retestHigh+buffer = 1.1014+0.0002 = 1.1016). Entry at 1.1016.
	bounce := []data.Candle{
		confBar(15, 1.1011, 1.1015, 1.1010, 1.1014), // breakout
		confBar(16, 1.1014, 1.1014, 1.1013, 1.1013), // pullback: low 1.1013 ≤ 1.10145, retestHigh 1.1014
		confBar(17, 1.1013, 1.1017, 1.1013, 1.1016), // bounce: high 1.1017 ≥ 1.1016
	}

	t.Run("bounce triggers a retest_range buy at the bounce level", func(t *testing.T) {
		s := &ORBScheduler{marketData: &qMarketData{candles: bounce}, now: now}
		rb, side, ok := s.retestEntry(context.Background(), "EUR/USD", r, buy, sell, cfg, inst, 100000)
		if !ok || side != "buy" {
			t.Fatalf("retestEntry ok=%v side=%q; want true/buy", ok, side)
		}
		if math.Abs(rb.EntryStop-1.1016) > 1e-9 {
			t.Errorf("entry = %.5f; want 1.1016 (retest bounce level)", rb.EntryStop)
		}
		if math.Abs(rb.StopLoss-1.0998) > 1e-9 { // retest_range → range-opposite stop
			t.Errorf("stop = %.5f; want 1.0998 (range stop)", rb.StopLoss)
		}
		// target = entry + risk×RR = 1.1016 + 0.0018×2 = 1.1052.
		if math.Abs(rb.TakeProfit-1.1052) > 1e-9 {
			t.Errorf("target = %.5f; want 1.1052", rb.TakeProfit)
		}
	})

	t.Run("no bounce yet → not entered", func(t *testing.T) {
		s := &ORBScheduler{marketData: &qMarketData{candles: bounce[:2]}, now: now} // breakout+pullback, no bounce
		if _, _, ok := s.retestEntry(context.Background(), "EUR/USD", r, buy, sell, cfg, inst, 100000); ok {
			t.Error("retestEntry ok=true; want false (bounce not yet printed)")
		}
	})

	t.Run("nil market data → not entered (never fail-open)", func(t *testing.T) {
		s := &ORBScheduler{now: now}
		if _, _, ok := s.retestEntry(context.Background(), "EUR/USD", r, buy, sell, cfg, inst, 100000); ok {
			t.Error("retestEntry ok=true with no data; want false")
		}
	})
}

func TestComputeRetestBracket_SnapsFuturesTick(t *testing.T) {
	// Futures inst: entry/stop/target must land on the 0.25 tick.
	inst := config.Instrument{Symbol: "MNQ", SecType: "FUT", MinLotSize: 1, PipBuffer: 0.5, PointValue: 2, TickSize: 0.25}
	cfg := config.ForexORBConfig{RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 0}
	// Off-tick entry 5004.9 and stop 4984.1 (as a candle extreme ± buffer could produce).
	rb := ComputeRetestBracket("MNQ", "buy", 5004.9, 4984.1, 100000, cfg, inst)
	for _, p := range []float64{rb.EntryStop, rb.StopLoss, rb.TakeProfit} {
		if rem := math.Abs(p/0.25 - math.Round(p/0.25)); rem > 1e-6 {
			t.Errorf("price %.4f not a 0.25-tick multiple", p)
		}
	}
	// Target must be RR × the SUBMITTED (snapped) stop distance, not the raw one: entry
	// 5004.9→5005.00, stop 4984.1→4984.00 → risk 21.00 → target 5005 + 21×2 = 5047.00
	// (NOT 5046.50 from the unsnapped risk of 20.80).
	if math.Abs(rb.EntryStop-5005.0) > 1e-6 || math.Abs(rb.StopLoss-4984.0) > 1e-6 {
		t.Fatalf("snapped entry/stop = %.2f/%.2f; want 5005.00/4984.00", rb.EntryStop, rb.StopLoss)
	}
	if math.Abs(rb.TakeProfit-5047.0) > 1e-6 {
		t.Errorf("target = %.2f; want 5047.00 (RR × submitted stop distance)", rb.TakeProfit)
	}
}
