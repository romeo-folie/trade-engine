package orbreplay

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

// fakeProvider returns scripted candles filtered to the requested [from, to) window and
// timeframe. Candles carry their own Timeframe, so a 1-minute session fetch and an hourly
// ATR fetch draw from disjoint fixtures.
type fakeProvider struct{ candles []data.Candle }

func (f *fakeProvider) FetchCandles(_ context.Context, ticker string, tf data.Timeframe, from, to time.Time) ([]data.Candle, error) {
	var out []data.Candle
	for _, c := range f.candles {
		if c.Timeframe != tf {
			continue
		}
		if !c.Timestamp.Before(from) && c.Timestamp.Before(to) {
			c.Ticker = ticker
			out = append(out, c)
		}
	}
	return out, nil
}
func (f *fakeProvider) FetchQuote(context.Context, string) (*data.Quote, error) { return nil, nil }
func (f *fakeProvider) FetchHistory(context.Context, string, data.Timeframe, int) ([]data.Candle, error) {
	return nil, nil
}

var (
	testDay     = time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC) // a Monday
	sessionOpen = testDay.Add(8 * time.Hour)                   // London 08:00 UTC
)

func cdl(min int, o, h, l, c float64) data.Candle {
	return data.Candle{Timeframe: data.Timeframe1Min, Timestamp: sessionOpen.Add(time.Duration(min) * time.Minute), Open: o, High: h, Low: l, Close: c}
}

// h1ATR builds 15 hourly bars (ending before the session) with a constant true range,
// so ComputeATR(_,14) returns that range — a known denominator for range_atr_ratio.
func h1ATR(trueRange float64) []data.Candle {
	var out []data.Candle
	start := sessionOpen.Add(-15 * time.Hour) // all 15 bars fall in sessionATR's 16h lookback
	for i := range 15 {
		hi := 1.1000 + trueRange
		out = append(out, data.Candle{
			Timeframe: data.Timeframe1Hour, Timestamp: start.Add(time.Duration(i) * time.Hour),
			Open: 1.1000, High: hi, Low: 1.1000, Close: 1.1000,
		})
	}
	return out
}

// rangeBars builds 15 one-minute bars (08:00–08:14) whose extremes are High=1.1010,
// Low=1.1000 → buy entry 1.1012 / stop 1.0998 / TP 1.1040 for EUR/USD (pip 0.0002).
func rangeBars() []data.Candle {
	bars := []data.Candle{cdl(0, 1.1005, 1.1010, 1.1000, 1.1005)} // sets the extremes
	for i := 1; i < 15; i++ {
		bars = append(bars, cdl(i, 1.1005, 1.1008, 1.1002, 1.1005)) // strictly inside
	}
	return bars
}

func testEngine(candles []data.Candle, v Variant) ([]SimTrade, error) {
	cfg := config.ForexORBConfig{
		Session: "london", RangeMinutes: 15, RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 10.0,
	}
	inst := config.Instrument{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002}
	e := NewEngine(cfg, &fakeProvider{candles: candles}, nil, 48*time.Hour)
	return e.Run(context.Background(), v, testDay, testDay.Add(23*time.Hour), []config.Instrument{inst}, 100000)
}

func TestReplay_CleanLongToTP(t *testing.T) {
	candles := rangeBars()
	candles = append(candles,
		cdl(15, 1.1009, 1.1015, 1.1008, 1.1014), // High 1.1015 ≥ buy entry 1.1012 → enter buy
		cdl(16, 1.1014, 1.1041, 1.1013, 1.1040), // High 1.1041 ≥ TP 1.1040 → take profit
	)
	trades, err := testEngine(candles, Baseline())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trades) != 1 {
		t.Fatalf("trades = %d; want 1", len(trades))
	}
	tr := trades[0]
	if tr.Side != "buy" || tr.ExitType != "tp" {
		t.Errorf("got side=%s exit=%s; want buy/tp", tr.Side, tr.ExitType)
	}
	if tr.NetPnL <= 0 {
		t.Errorf("NetPnL = %v; want > 0 for a TP win after costs", tr.NetPnL)
	}
	if tr.Qty != 5000 {
		t.Errorf("Qty = %d; want 5000 (notional-capped)", tr.Qty)
	}
}

func TestReplay_StopOut(t *testing.T) {
	candles := rangeBars()
	candles = append(candles,
		cdl(15, 1.1009, 1.1015, 1.1008, 1.1014), // enter buy
		cdl(16, 1.1014, 1.1016, 1.0997, 1.0998), // Low 1.0997 ≤ stop 1.0998 → stop out
	)
	trades, err := testEngine(candles, Baseline())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trades) != 1 || trades[0].ExitType != "stop" {
		t.Fatalf("got %+v; want one stop exit", trades)
	}
	if trades[0].NetPnL >= 0 {
		t.Errorf("NetPnL = %v; want < 0 for a stop loss", trades[0].NetPnL)
	}
}

func TestReplay_TimeExitFlattens(t *testing.T) {
	candles := rangeBars()
	candles = append(candles, cdl(15, 1.1009, 1.1015, 1.1008, 1.1014)) // enter buy at 08:15
	// Bars 16..44 hover between stop and TP (no leg touched) through the 30-min deadline.
	for i := 16; i <= 45; i++ {
		candles = append(candles, cdl(i, 1.1015, 1.1016, 1.1014, 1.1015))
	}
	trades, err := testEngine(candles, TimeExit(30)) // deadline 08:45
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trades) != 1 || trades[0].ExitType != "time_exit" {
		t.Fatalf("got %+v; want one time_exit", trades)
	}
	// Exit at the first bar at/after the deadline (08:45).
	if !trades[0].ExitTime.Equal(sessionOpen.Add(45 * time.Minute)) {
		t.Errorf("ExitTime = %v; want 08:45", trades[0].ExitTime)
	}
}

func TestReplay_EndOfData(t *testing.T) {
	candles := rangeBars()
	candles = append(candles,
		cdl(15, 1.1009, 1.1015, 1.1008, 1.1014), // enter buy
		cdl(16, 1.1015, 1.1016, 1.1014, 1.1015), // no leg touched, data ends
	)
	trades, err := testEngine(candles, Baseline())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trades) != 1 || trades[0].ExitType != "end_of_data" {
		t.Fatalf("got %+v; want one end_of_data", trades)
	}
}

func TestReplay_NoBreakoutNoTrade(t *testing.T) {
	candles := rangeBars()
	// Price stays strictly inside the range — neither stop-entry triggers.
	candles = append(candles,
		cdl(15, 1.1005, 1.1009, 1.1001, 1.1004),
		cdl(16, 1.1004, 1.1008, 1.1002, 1.1006),
	)
	trades, err := testEngine(candles, Baseline())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trades) != 0 {
		t.Fatalf("trades = %d; want 0 (range never broke)", len(trades))
	}
}

func TestReplay_CleanShortToTP(t *testing.T) {
	// Sell entry = Low−pip = 1.0998, stop = High+pip = 1.1012, TP = 1.0998 − 2×0.0014 = 1.0970.
	candles := rangeBars()
	candles = append(candles,
		cdl(15, 1.1001, 1.1002, 1.0997, 1.0998), // Low 1.0997 ≤ sell entry 1.0998 → enter sell
		cdl(16, 1.0998, 1.0999, 1.0969, 1.0970), // Low 1.0969 ≤ TP 1.0970 → take profit
	)
	trades, err := testEngine(candles, Baseline())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trades) != 1 || trades[0].Side != "sell" || trades[0].ExitType != "tp" {
		t.Fatalf("got %+v; want one sell/tp", trades)
	}
	if trades[0].NetPnL <= 0 {
		t.Errorf("NetPnL = %v; want > 0", trades[0].NetPnL)
	}
}

func TestReplay_CommissionDragsLosersBelowGross(t *testing.T) {
	candles := rangeBars()
	candles = append(candles,
		cdl(15, 1.1009, 1.1015, 1.1008, 1.1014),
		cdl(16, 1.1014, 1.1016, 1.0997, 1.0998), // stop out
	)
	trades, _ := testEngine(candles, Baseline())
	tr := trades[0]
	if tr.Commission != 4.0 {
		t.Errorf("Commission = %v; want 4.0 (2 sides × $2 default)", tr.Commission)
	}
	if tr.NetPnL >= tr.GrossPnL {
		t.Errorf("NetPnL %v should be below GrossPnL %v after commission", tr.NetPnL, tr.GrossPnL)
	}
}

func TestReplay_SameBarStopOut(t *testing.T) {
	// A single wide bar both triggers the breakout and spans the stop. Dual-sided
	// geometry makes the buy-stop-loss level coincide with the sell-entry level, so a
	// bearish such bar is taken as a sell that stops on its own bar (conservative).
	candles := rangeBars()
	candles = append(candles,
		cdl(15, 1.1000, 1.1013, 1.0997, 1.0999), // sell entry 1.0998 hit; high 1.1013 ≥ sell stop 1.1012
		cdl(16, 1.0998, 1.0999, 1.0969, 1.0970), // would be a TP later — must NOT be reached
	)
	trades, err := testEngine(candles, Baseline())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trades) != 1 {
		t.Fatalf("trades = %d; want 1", len(trades))
	}
	tr := trades[0]
	if tr.ExitType != "stop" {
		t.Errorf("ExitType = %q; want stop (adverse same-bar)", tr.ExitType)
	}
	if !tr.ExitTime.Equal(sessionOpen.Add(15 * time.Minute)) {
		t.Errorf("ExitTime = %v; want 08:15 (stopped on the entry bar, not a later TP)", tr.ExitTime)
	}
	if tr.NetPnL >= 0 {
		t.Errorf("NetPnL = %v; want < 0", tr.NetPnL)
	}
}

func TestReplay_SessionBothRejected(t *testing.T) {
	cfg := config.ForexORBConfig{Session: "both", RangeMinutes: 15, RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 10.0}
	inst := config.Instrument{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002}
	e := NewEngine(cfg, &fakeProvider{}, nil, 0)
	if _, err := e.Run(context.Background(), Baseline(), testDay, testDay.Add(23*time.Hour), []config.Instrument{inst}, 100000); err == nil {
		t.Error(`session "both" should error (one session per run), got nil`)
	}
}

func TestReplay_EntryWindowClosesAfter60Min(t *testing.T) {
	// A breakout after the 60-min live cancel window (deadline 09:00) must not trade.
	candles := rangeBars()
	candles = append(candles,
		cdl(15, 1.1005, 1.1009, 1.1001, 1.1004), // inside the range — no entry
		cdl(65, 1.1009, 1.1015, 1.1008, 1.1014), // 09:05 breakout — past the entry window
	)
	trades, err := testEngine(candles, Baseline())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trades) != 0 {
		t.Fatalf("trades = %d; want 0 (breakout after the 60-min entry window)", len(trades))
	}
}

func TestReplay_NearFlatRangeSkipped(t *testing.T) {
	// Opening range width 0.0002 < 2×pip (0.0004) → live skips it; the replay must too.
	var candles []data.Candle
	candles = append(candles, cdl(0, 1.1001, 1.1002, 1.1000, 1.1001)) // width 0.0002
	for i := 1; i < 15; i++ {
		candles = append(candles, cdl(i, 1.1001, 1.1002, 1.1000, 1.1001))
	}
	candles = append(candles, cdl(15, 1.1002, 1.1010, 1.1001, 1.1009)) // would break out
	trades, err := testEngine(candles, Baseline())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trades) != 0 {
		t.Fatalf("trades = %d; want 0 (near-flat range filtered)", len(trades))
	}
}

func TestReplay_SkipsSessionWhilePriorTradeOpen(t *testing.T) {
	// A Monday baseline trade that never resolves stays open into Tuesday; the live
	// guard (tickersWithOpenFilledTrades) would suppress Tuesday's bracket, so the
	// replay must produce exactly one trade (Monday's), not stack a Tuesday entry.
	candles := rangeBars() // Mon 08:00–08:14, High 1.1010 / Low 1.1000
	candles = append(candles,
		cdl(15, 1.1009, 1.1015, 1.1008, 1.1014), // Mon 08:15 enter buy (TP 1.1040 / stop 1.0998)
		cdl(16, 1.1015, 1.1016, 1.1014, 1.1015), // Mon inside
	)
	// Tuesday (offset +1440 min) has its OWN wide range + breakout that, in isolation,
	// would trade — but Monday's position is still open, so it must be skipped. All Tue
	// bars stay between Monday's stop and TP, so Monday's trade does not exit on them.
	candles = append(candles, cdl(1440, 1.1025, 1.1030, 1.1020, 1.1025)) // Tue range extremes
	for i := 1441; i < 1455; i++ {
		candles = append(candles, cdl(i, 1.1025, 1.1028, 1.1022, 1.1025))
	}
	candles = append(candles,
		cdl(1455, 1.1029, 1.1033, 1.1028, 1.1032), // Tue 08:15 breakout (buy entry 1.1032)
		cdl(1456, 1.1032, 1.1033, 1.1031, 1.1032),
	)

	cfg := config.ForexORBConfig{Session: "london", RangeMinutes: 15, RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 10.0}
	inst := config.Instrument{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002}
	e := NewEngine(cfg, &fakeProvider{candles: candles}, nil, 48*time.Hour)
	trades, err := e.Run(context.Background(), Baseline(), testDay, testDay.Add(48*time.Hour), []config.Instrument{inst}, 100000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trades) != 1 {
		t.Fatalf("trades = %d; want 1 (Tuesday suppressed while Monday's trade is open)", len(trades))
	}
	if !trades[0].EntryTime.Equal(sessionOpen.Add(15 * time.Minute)) {
		t.Errorf("EntryTime = %v; want Monday 08:15", trades[0].EntryTime)
	}
}

func TestReplay_RangeFilterVariant(t *testing.T) {
	// Opening range width = 0.0010 (rangeBars). ATR = 0.0010 (h1ATR) → range_atr_ratio 1.0.
	build := func(minATR, maxATR float64) ([]SimTrade, error) {
		candles := rangeBars()
		candles = append(candles,
			cdl(15, 1.1009, 1.1015, 1.1008, 1.1014), // enter buy
			cdl(16, 1.1014, 1.1041, 1.1013, 1.1040), // TP
		)
		candles = append(candles, h1ATR(0.0010)...) // ATR = 0.0010
		cfg := config.ForexORBConfig{Session: "london", RangeMinutes: 15, RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 10.0, ATRPeriod: 14, ATRTimeframe: "1h"}
		inst := config.Instrument{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, MinRangeATR: minATR, MaxRangeATR: maxATR}
		e := NewEngine(cfg, &fakeProvider{candles: candles}, nil, 48*time.Hour)
		return e.Run(context.Background(), RangeFilter(0), testDay, testDay.Add(23*time.Hour), []config.Instrument{inst}, 100000)
	}

	t.Run("ratio above max is filtered", func(t *testing.T) {
		trades, err := build(0, 0.90) // ratio 1.0 > 0.90
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(trades) != 0 {
			t.Fatalf("trades = %d; want 0 (range_atr_ratio 1.0 exceeds max 0.90)", len(trades))
		}
	})

	t.Run("ratio in band trades", func(t *testing.T) {
		trades, err := build(0.25, 0) // ratio 1.0 ≥ 0.25, no upper bound
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(trades) != 1 {
			t.Fatalf("trades = %d; want 1 (range_atr_ratio 1.0 passes)", len(trades))
		}
	})
}

func TestReplay_SpreadFilterVariant(t *testing.T) {
	// risk = range width 0.0010 + 2×pip 0.0002 = 0.0014. With spread 0.0002,
	// spread/risk = 0.143 — rejected by max 0.10, allowed by max 0.20.
	build := func(maxSpreadToRisk float64) ([]SimTrade, error) {
		candles := rangeBars()
		candles = append(candles,
			cdl(15, 1.1009, 1.1015, 1.1008, 1.1014), // enter buy
			cdl(16, 1.1014, 1.1041, 1.1013, 1.1040), // TP
		)
		cfg := config.ForexORBConfig{Session: "london", RangeMinutes: 15, RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 10.0}
		inst := config.Instrument{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, MaxSpreadToRisk: maxSpreadToRisk}
		costs := map[string]CostModel{"EUR/USD": {Spread: 0.0002, CommissionPerSide: 2.0}}
		e := NewEngine(cfg, &fakeProvider{candles: candles}, costs, 48*time.Hour)
		return e.Run(context.Background(), RangeFilter(0), testDay, testDay.Add(23*time.Hour), []config.Instrument{inst}, 100000)
	}

	t.Run("spread above max is filtered", func(t *testing.T) {
		trades, err := build(0.10) // 0.143 > 0.10
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(trades) != 0 {
			t.Fatalf("trades = %d; want 0 (spread/risk 0.143 exceeds max 0.10)", len(trades))
		}
	})
	t.Run("spread within max trades", func(t *testing.T) {
		trades, err := build(0.20) // 0.143 ≤ 0.20
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(trades) != 1 {
			t.Fatalf("trades = %d; want 1 (spread/risk 0.143 within max 0.20)", len(trades))
		}
	})
}

func TestReplay_CommissionFilterVariant(t *testing.T) {
	// risk/unit 0.0014; the full-notional cap floors qty to the 5000 min lot → dollar
	// risk $7. Round-trip commission $4 → commission/risk 0.571: a min-commission trade
	// the gate exists to refuse. With margin_rate 0.033 the cap lifts to 275,000 units →
	// dollar risk $385 → ratio 0.0104, and the same max admits the trade.
	build := func(maxCommissionToRisk, marginRate float64) ([]SimTrade, error) {
		candles := rangeBars()
		candles = append(candles,
			cdl(15, 1.1009, 1.1015, 1.1008, 1.1014), // enter buy
			cdl(16, 1.1014, 1.1041, 1.1013, 1.1040), // TP
		)
		cfg := config.ForexORBConfig{Session: "london", RangeMinutes: 15, RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 10.0}
		inst := config.Instrument{
			Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002,
			MaxCommissionToRisk: maxCommissionToRisk, MarginRate: marginRate,
		}
		costs := map[string]CostModel{"EUR/USD": {Spread: 0.0001, CommissionPerSide: 2.0}}
		e := NewEngine(cfg, &fakeProvider{candles: candles}, costs, 48*time.Hour)
		return e.Run(context.Background(), RangeFilter(0), testDay, testDay.Add(23*time.Hour), []config.Instrument{inst}, 100000)
	}

	t.Run("min-lot dollar risk is filtered", func(t *testing.T) {
		trades, err := build(0.05, 0) // 0.571 > 0.05
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(trades) != 0 {
			t.Fatalf("trades = %d; want 0 (commission/risk 0.571 exceeds max 0.05)", len(trades))
		}
	})
	t.Run("disabled gate keeps the min-lot trade", func(t *testing.T) {
		trades, err := build(0, 0)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(trades) != 1 {
			t.Fatalf("trades = %d; want 1 (gate disabled)", len(trades))
		}
	})
	t.Run("margin-sized position passes the same max", func(t *testing.T) {
		trades, err := build(0.05, 0.033) // ratio 0.0104 ≤ 0.05
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(trades) != 1 {
			t.Fatalf("trades = %d; want 1 (margin sizing lifts dollar risk above the commission floor)", len(trades))
		}
		if got := trades[0].Qty; got != 275000 {
			t.Fatalf("qty = %d; want 275000 (margin-based cap at 30:1)", got)
		}
	})
}

func TestReplay_ConfirmedVariant(t *testing.T) {
	cfg := config.ForexORBConfig{Session: "london", RangeMinutes: 15, RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 10.0}
	// close_1m confirmation; entry-quality bounds left at 0 so only confirmation matters.
	inst := config.Instrument{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, BreakoutConfirmation: "close_1m"}
	run := func(candles []data.Candle) ([]SimTrade, error) {
		e := NewEngine(cfg, &fakeProvider{candles: candles}, nil, 48*time.Hour)
		return e.Run(context.Background(), Confirmed(0), testDay, testDay.Add(23*time.Hour), []config.Instrument{inst}, 100000)
	}

	t.Run("wick touch that closes inside does not confirm", func(t *testing.T) {
		candles := rangeBars()
		candles = append(candles,
			cdl(15, 1.1009, 1.1015, 1.1008, 1.1011), // high 1.1015 ≥ entry 1.1012 (a touch) but close 1.1011 inside
			cdl(16, 1.1011, 1.1011, 1.1009, 1.1010), // stays inside
		)
		trades, err := run(candles)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(trades) != 0 {
			t.Fatalf("trades = %d; want 0 (a wick that closes inside must not confirm — stop_touch would have entered)", len(trades))
		}
	})

	t.Run("a close beyond confirms and enters market on the next bar", func(t *testing.T) {
		candles := rangeBars()
		candles = append(candles,
			cdl(15, 1.1009, 1.1016, 1.1008, 1.1014), // close 1.1014 ≥ entry 1.1012 → confirm buy
			cdl(16, 1.1014, 1.1018, 1.1013, 1.1016), // entry bar: market fill at its open (1.1014)
			cdl(17, 1.1016, 1.1041, 1.1015, 1.1040), // TP
		)
		trades, err := run(candles)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(trades) != 1 || trades[0].ExitType != "tp" {
			t.Fatalf("got %+v; want one tp trade", trades)
		}
		// Market-on-confirmation: enters at the next bar's open (1.1014 + half-spread), ABOVE
		// the stop-touch level 1.1012 — the price cost of waiting for confirmation.
		if trades[0].EntryPrice <= 1.1012 {
			t.Errorf("EntryPrice = %v; want > 1.1012 (market fill above the breakout level)", trades[0].EntryPrice)
		}
	})

	t.Run("confirmation on the last bar yields no fill bar", func(t *testing.T) {
		candles := rangeBars()
		candles = append(candles,
			cdl(15, 1.1009, 1.1016, 1.1008, 1.1014), // confirms on the last available candle
		)
		trades, err := run(candles)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(trades) != 0 {
			t.Fatalf("trades = %d; want 0 (no bar after confirmation to fill on)", len(trades))
		}
	})
}

// h1Trend builds 21 climbing (start>0) or falling (start<0 slope) hourly bars ending
// before the session, so ComputeEMA(_,20) sits below/above the last close.
func h1Trend(rising bool) []data.Candle {
	var out []data.Candle
	start := sessionOpen.Add(-21 * time.Hour)
	for i := range 21 {
		var px float64
		if rising {
			px = 1.1000 + float64(i)*0.0002
		} else {
			px = 1.1040 - float64(i)*0.0002
		}
		out = append(out, data.Candle{Timeframe: data.Timeframe1Hour, Timestamp: start.Add(time.Duration(i) * time.Hour), Open: px, High: px, Low: px, Close: px})
	}
	return out
}

func TestReplay_DirectionEligibility(t *testing.T) {
	cfg := config.ForexORBConfig{Session: "london", RangeMinutes: 15, RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 10.0, TrendTimeframe: "1h", TrendEMAPeriod: 20}
	// Only direction eligibility active (no ATR bounds, spread, or confirmation configured).
	inst := config.Instrument{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, DirectionEligibility: true}
	run := func(session, trend []data.Candle) ([]SimTrade, error) {
		all := append([]data.Candle{}, session...)
		all = append(all, trend...)
		e := NewEngine(cfg, &fakeProvider{candles: all}, nil, 48*time.Hour)
		return e.Run(context.Background(), Confirmed(0), testDay, testDay.Add(23*time.Hour), []config.Instrument{inst}, 100000)
	}

	t.Run("uptrend rules out a downside breakout", func(t *testing.T) {
		session := rangeBars()
		session = append(session,
			cdl(15, 1.1001, 1.1002, 1.0997, 1.0998), // sell breakout (low ≤ sell entry 1.0998)
			cdl(16, 1.0998, 1.0999, 1.0969, 1.0970),
		)
		trades, err := run(session, h1Trend(true)) // uptrend → short ineligible
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(trades) != 0 {
			t.Fatalf("trades = %d; want 0 (sell ruled out by an uptrend)", len(trades))
		}
	})

	t.Run("uptrend allows an upside breakout", func(t *testing.T) {
		session := rangeBars()
		session = append(session,
			cdl(15, 1.1009, 1.1015, 1.1008, 1.1014), // buy breakout
			cdl(16, 1.1014, 1.1041, 1.1013, 1.1040), // TP
		)
		trades, err := run(session, h1Trend(true))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(trades) != 1 || trades[0].Side != "buy" {
			t.Fatalf("got %+v; want one buy trade (trend-aligned)", trades)
		}
	})
}

// adaptiveInst is a EUR/USD instrument with only the given adaptive-exit fields set
// (no confirmation/direction/range filters), so the adaptive variant reduces to
// stop_touch entry + adaptive exit management.
func adaptiveInst(set func(*config.Instrument)) config.Instrument {
	inst := config.Instrument{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002}
	set(&inst)
	return inst
}

func runAdaptive(candles []data.Candle, inst config.Instrument) ([]SimTrade, error) {
	cfg := config.ForexORBConfig{Session: "london", RangeMinutes: 15, RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 10.0, ATRTimeframe: "1h", ATRPeriod: 14}
	e := NewEngine(cfg, &fakeProvider{candles: candles}, nil, 48*time.Hour)
	return e.Run(context.Background(), Adaptive(0), testDay, testDay.Add(23*time.Hour), []config.Instrument{inst}, 100000)
}

func TestReplay_AdaptiveBreakEven(t *testing.T) {
	// buy entry 1.1012, stop 1.0998, R=0.0014. +1R = ~1.1026. Reach +1R (stop → entry),
	// then drop back to entry: exit at break-even, NOT the −1R stop.
	candles := rangeBars()
	candles = append(candles,
		cdl(15, 1.1009, 1.1015, 1.1008, 1.1014), // stop_touch buy entry
		cdl(16, 1.1014, 1.1028, 1.1013, 1.1027), // reaches +1R → stop moves to entry (~1.10125)
		cdl(17, 1.1014, 1.1015, 1.1011, 1.1012), // drops to the break-even stop → exit
	)
	trades, err := runAdaptive(candles, adaptiveInst(func(i *config.Instrument) { i.BreakEvenR = 1.0 }))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trades) != 1 || trades[0].ExitType != "stop" {
		t.Fatalf("got %+v; want one stop (break-even) exit", trades)
	}
	// Break-even exit is ~scratch (just costs), far better than a full −1R (~−7 gross).
	if trades[0].GrossPnL < -1.0 {
		t.Errorf("GrossPnL = %v; want ≈ 0 (break-even), not a full −1R loss", trades[0].GrossPnL)
	}
}

func TestReplay_AdaptivePartialTakeProfit(t *testing.T) {
	// Partial 50% at +1R, runner to +2R. Both legs win → 3 fills (entry + partial + runner).
	candles := rangeBars()
	candles = append(candles,
		cdl(15, 1.1009, 1.1015, 1.1008, 1.1014), // entry
		cdl(16, 1.1014, 1.1028, 1.1013, 1.1027), // reaches +1R → partial 50% at ~1.10265
		cdl(17, 1.1027, 1.1041, 1.1026, 1.1040), // reaches +2R → runner TP at 1.1040
	)
	inst := adaptiveInst(func(i *config.Instrument) {
		i.MinLotSize = 1 // so a 50% partial of a single-lot position is still a tradeable size
		i.PartialTakeProfitR = 1.0
		i.PartialFraction = 0.5
		i.RunnerTakeProfitR = 2.0
	})
	trades, err := runAdaptive(candles, inst)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trades) != 1 || trades[0].ExitType != "tp" {
		t.Fatalf("got %+v; want one tp (runner) exit", trades)
	}
	if trades[0].Commission != 6.0 { // (1 entry + 2 exits) × $2
		t.Errorf("Commission = %v; want 6.0 (partial adds a third fill)", trades[0].Commission)
	}
	if trades[0].GrossPnL <= 0 {
		t.Errorf("GrossPnL = %v; want > 0 (partial + runner both win)", trades[0].GrossPnL)
	}
}

func TestReplay_AdaptiveTrailing(t *testing.T) {
	// Trail at hwm − 1×ATR (ATR 0.0010). Price spikes to 1.1030 (stop trails to ~1.1020),
	// then reverses → exit at the trailed stop, locking profit ABOVE entry.
	candles := rangeBars()
	candles = append(candles,
		cdl(15, 1.1009, 1.1015, 1.1008, 1.1014), // entry
		cdl(16, 1.1014, 1.1030, 1.1025, 1.1028), // hwm 1.1030 → trailed stop ~1.1020
		cdl(17, 1.1026, 1.1027, 1.1019, 1.1020), // drops below the trailed stop → exit
	)
	candles = append(candles, h1ATR(0.0010)...) // ATR = 0.0010
	inst := adaptiveInst(func(i *config.Instrument) {
		i.TrailATRMult = 1.0
		i.TrailAfterR = 0
	})
	trades, err := runAdaptive(candles, inst)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trades) != 1 || trades[0].ExitType != "stop" {
		t.Fatalf("got %+v; want one stop (trailed) exit", trades)
	}
	// The trailed stop is above entry, so the exit locks a profit (> 0 gross).
	if trades[0].GrossPnL <= 0 {
		t.Errorf("GrossPnL = %v; want > 0 (trailed stop locked profit above entry)", trades[0].GrossPnL)
	}
}

func TestReplay_AdaptivePartial_SubLotSkipped(t *testing.T) {
	// MinLotSize 5000, qty 5000, 50% partial = 2500 → rounds to 0 lots → no partial.
	// The trade must take a single full exit (2 fills), not a fabricated 2500-unit partial.
	candles := rangeBars()
	candles = append(candles,
		cdl(15, 1.1009, 1.1015, 1.1008, 1.1014), // entry
		cdl(16, 1.1014, 1.1028, 1.1013, 1.1027), // reaches the partial level (1R) — but 2500 < 1 lot
		cdl(17, 1.1027, 1.1041, 1.1026, 1.1040), // TP
	)
	inst := adaptiveInst(func(i *config.Instrument) { // MinLotSize stays 5000
		i.PartialTakeProfitR = 1.0
		i.PartialFraction = 0.5
		i.RunnerTakeProfitR = 2.0
	})
	trades, err := runAdaptive(candles, inst)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trades) != 1 {
		t.Fatalf("trades = %d; want 1", len(trades))
	}
	if trades[0].Commission != 4.0 { // 2 fills (entry + one exit) — NO partial fill
		t.Errorf("Commission = %v; want 4.0 (sub-lot partial must not produce a third fill)", trades[0].Commission)
	}
}

func TestReplay_PerInstrumentMaxHold(t *testing.T) {
	// Variant says 240, but the instrument configures 30 — the per-instrument value must win
	// (mirroring live EffectiveMaxHoldMinutes), so the trade time-exits at 08:45, not 12:15.
	candles := rangeBars()
	candles = append(candles, cdl(15, 1.1009, 1.1015, 1.1008, 1.1014)) // enter buy
	for i := 16; i <= 50; i++ {
		candles = append(candles, cdl(i, 1.1015, 1.1016, 1.1014, 1.1015)) // hover, no TP/SL
	}
	cfg := config.ForexORBConfig{Session: "london", RangeMinutes: 15, RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 10.0}
	inst := config.Instrument{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, MaxHoldMinutes: 30}
	e := NewEngine(cfg, &fakeProvider{candles: candles}, nil, 48*time.Hour)
	trades, err := e.Run(context.Background(), TimeExit(240), testDay, testDay.Add(23*time.Hour), []config.Instrument{inst}, 100000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(trades) != 1 || trades[0].ExitType != "time_exit" {
		t.Fatalf("got %+v; want one time_exit", trades)
	}
	if !trades[0].ExitTime.Equal(sessionOpen.Add(45 * time.Minute)) {
		t.Errorf("ExitTime = %v; want 08:45 (the instrument's 30-min max-hold, not the variant's 240)", trades[0].ExitTime)
	}
}

// testEngineInst runs the replay with a caller-supplied instrument (so a test can set
// per-instrument gates like CloseFracThreshold), otherwise identical to testEngine.
func testEngineInst(candles []data.Candle, v Variant, inst config.Instrument) ([]SimTrade, error) {
	cfg := config.ForexORBConfig{
		Session: "london", RangeMinutes: 15, RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 10.0,
	}
	e := NewEngine(cfg, &fakeProvider{candles: candles}, nil, 48*time.Hour)
	return e.Run(context.Background(), v, testDay, testDay.Add(23*time.Hour), []config.Instrument{inst}, 100000)
}

// topClosingRange builds 15 one-minute bars with the same extremes as rangeBars
// (High=1.1010, Low=1.1000) but whose FINAL bar closes at 1.1009 → CloseFrac 0.9,
// so a close-position guard arms the buy side and disables the sell side.
func topClosingRange() []data.Candle {
	bars := []data.Candle{cdl(0, 1.1005, 1.1010, 1.1000, 1.1005)} // sets the extremes
	for i := 1; i < 14; i++ {
		bars = append(bars, cdl(i, 1.1005, 1.1008, 1.1002, 1.1005)) // strictly inside
	}
	bars = append(bars, cdl(14, 1.1008, 1.1009, 1.1007, 1.1009)) // last close near the high → frac 0.9
	return bars
}

func TestReplay_CloseFractionGuard(t *testing.T) {
	inst := config.Instrument{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, CloseFracThreshold: 0.6}

	t.Run("mid-range close skips the session", func(t *testing.T) {
		// rangeBars closes at 1.1005 → frac 0.5, inside the [0.4,0.6] dead zone → both sides
		// zeroed. Even with a clean upside breakout, no trade is placed.
		candles := rangeBars()
		candles = append(candles,
			cdl(15, 1.1009, 1.1015, 1.1008, 1.1014), // would enter buy without the guard
			cdl(16, 1.1014, 1.1041, 1.1013, 1.1040), // would TP
		)
		trades, err := testEngineInst(candles, RangeFilter(0), inst)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(trades) != 0 {
			t.Fatalf("trades = %d; want 0 (mid-range close should skip)", len(trades))
		}
	})

	t.Run("top close arms buy only", func(t *testing.T) {
		candles := topClosingRange()
		candles = append(candles,
			cdl(15, 1.1009, 1.1015, 1.1008, 1.1014), // High ≥ buy entry 1.1012 → enter buy
			cdl(16, 1.1014, 1.1041, 1.1013, 1.1040), // TP
		)
		trades, err := testEngineInst(candles, RangeFilter(0), inst)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(trades) != 1 || trades[0].Side != "buy" {
			t.Fatalf("got %+v; want exactly one buy trade", trades)
		}
	})

	t.Run("top close disables sell — a downside break does not trade", func(t *testing.T) {
		candles := topClosingRange()
		candles = append(candles,
			cdl(15, 1.1001, 1.1002, 1.0997, 1.0998), // Low ≤ sell entry 1.0998 → would enter sell without guard
			cdl(16, 1.0998, 1.0999, 1.0995, 1.0996),
		)
		trades, err := testEngineInst(candles, RangeFilter(0), inst)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(trades) != 0 {
			t.Fatalf("trades = %d; want 0 (sell disabled by top close)", len(trades))
		}
	})

	t.Run("guard off (threshold 0) trades normally", func(t *testing.T) {
		off := inst
		off.CloseFracThreshold = 0
		candles := rangeBars()
		candles = append(candles,
			cdl(15, 1.1009, 1.1015, 1.1008, 1.1014),
			cdl(16, 1.1014, 1.1041, 1.1013, 1.1040),
		)
		trades, err := testEngineInst(candles, RangeFilter(0), off)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(trades) != 1 {
			t.Fatalf("trades = %d; want 1 (guard off should not skip)", len(trades))
		}
	})
}
