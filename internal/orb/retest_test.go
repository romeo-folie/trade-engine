package orb

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

// bar builds a 1-minute candle at minute m (OHLC).
func rtBar(m int, o, h, l, c float64) data.Candle {
	return data.Candle{
		Timestamp: time.Date(2025, 5, 1, 8, m, 0, 0, time.UTC),
		Open:      o, High: h, Low: l, Close: c,
	}
}

// A clean long retest: break above the level, pull back to it (without closing inside), then
// bounce beyond the pullback bar → entry on the bounce, swing stop at the pullback low.
func TestDetectRetest_LongBounce(t *testing.T) {
	// range high 1.1000, buyTrigger 1.1002 (high+buffer), rangeLow 1.0980.
	rangeHigh, rangeLow, buyTrig, buffer, tol := 1.1000, 1.0980, 1.1002, 0.0002, 0.30
	candles := []data.Candle{
		rtBar(0, 1.1001, 1.1010, 1.1000, 1.1008), // breakout: closes above buyTrigger
		rtBar(1, 1.1008, 1.1009, 1.1003, 1.1004), // pullback toward the level (low 1.1003, within tol), stays above rangeHigh
		rtBar(2, 1.1004, 1.1015, 1.1004, 1.1014), // bounce: high 1.1015 > retest bar high 1.1009 → ENTRY
	}
	r := DetectRetest(candles, buyTrig, 0, rangeHigh, rangeLow, buffer, tol, 30)
	if !r.OK || r.Side != "buy" {
		t.Fatalf("expected a buy retest entry, got %+v", r)
	}
	if r.EntryIdx != 2 {
		t.Errorf("entry idx = %d; want 2 (the bounce bar)", r.EntryIdx)
	}
	if r.EntryPrice != 1.1009+buffer { // retest bar (idx1) high + buffer
		t.Errorf("entry price = %.5f; want %.5f", r.EntryPrice, 1.1009+buffer)
	}
	if r.SwingLow != 1.1003 { // retest bar low → tighter stop than rangeLow
		t.Errorf("swing low = %.5f; want 1.1003", r.SwingLow)
	}
}

// A close back inside the range after breakout = failed breakout → no entry.
func TestDetectRetest_FailedBreakoutClosesInside(t *testing.T) {
	rangeHigh, rangeLow, buyTrig := 1.1000, 1.0980, 1.1002
	candles := []data.Candle{
		rtBar(0, 1.1001, 1.1010, 1.1000, 1.1008), // breakout
		rtBar(1, 1.1008, 1.1009, 1.0990, 1.0995), // closes back INSIDE the range (1.0995 < 1.1000) → fail
		rtBar(2, 1.0995, 1.1015, 1.0995, 1.1014), // would-be bounce, but breakout already failed
	}
	if r := DetectRetest(candles, buyTrig, 0, rangeHigh, rangeLow, 0.0002, 0.30, 30); r.OK {
		t.Errorf("expected no entry after close back inside range, got %+v", r)
	}
}

// No pullback (price runs away) → no retest entry (this is the missed-runaway trade-off).
func TestDetectRetest_NoPullbackNoEntry(t *testing.T) {
	rangeHigh, rangeLow, buyTrig := 1.1000, 1.0980, 1.1002
	candles := []data.Candle{
		rtBar(0, 1.1001, 1.1010, 1.1005, 1.1008), // breakout, never dips near the level (tol band ~1.1008)
		rtBar(1, 1.1008, 1.1020, 1.1010, 1.1018),
		rtBar(2, 1.1018, 1.1030, 1.1020, 1.1028),
	}
	if r := DetectRetest(candles, buyTrig, 0, rangeHigh, rangeLow, 0.0002, 0.10, 30); r.OK {
		t.Errorf("expected no entry when price never retests, got %+v", r)
	}
}

// Symmetric short retest.
func TestDetectRetest_ShortBounce(t *testing.T) {
	rangeHigh, rangeLow, sellTrig, buffer, tol := 1.1020, 1.1000, 1.0998, 0.0002, 0.30
	candles := []data.Candle{
		rtBar(0, 1.1000, 1.1000, 1.0990, 1.0992), // breakout: closes below sellTrigger
		rtBar(1, 1.0992, 1.0997, 1.0991, 1.0996), // pullback up toward the level (high 1.0997 within tol), stays below rangeLow
		rtBar(2, 1.0996, 1.0996, 1.0985, 1.0986), // bounce down: low 1.0985 < retest bar low 1.0991 → ENTRY
	}
	r := DetectRetest(candles, 0, sellTrig, rangeHigh, rangeLow, buffer, tol, 30)
	if !r.OK || r.Side != "sell" {
		t.Fatalf("expected a sell retest entry, got %+v", r)
	}
	if r.EntryPrice != 1.0991-buffer { // retest bar low − buffer
		t.Errorf("entry price = %.5f; want %.5f", r.EntryPrice, 1.0991-buffer)
	}
	if r.SwingHigh != 1.0997 {
		t.Errorf("swing high = %.5f; want 1.0997", r.SwingHigh)
	}
}

// Entry must occur within timeoutBars of the breakout.
func TestDetectRetest_Timeout(t *testing.T) {
	rangeHigh, rangeLow, buyTrig := 1.1000, 1.0980, 1.1002
	candles := []data.Candle{
		rtBar(0, 1.1001, 1.1010, 1.1000, 1.1008), // breakout
		rtBar(1, 1.1008, 1.1009, 1.1003, 1.1004), // retest reached at idx 1
		rtBar(2, 1.1004, 1.1005, 1.1003, 1.1004), // no bounce
		rtBar(3, 1.1004, 1.1005, 1.1003, 1.1004), // no bounce; timeout=1 bar past breakout
	}
	if r := DetectRetest(candles, buyTrig, 0, rangeHigh, rangeLow, 0.0002, 0.30, 1); r.OK {
		t.Errorf("expected no entry past timeout, got %+v", r)
	}
}

func TestComputeRetestBracket_TighterStopSizesLarger(t *testing.T) {
	cfg := config.ForexORBConfig{RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 0}
	inst := config.Instrument{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 1}
	// A tight retest stop (5 pips) vs a wide range stop (20 pips) at the same entry → the tighter
	// stop risk-sizes to a larger qty and a nearer target.
	tight := ComputeRetestBracket("EUR/USD", "buy", 1.1010, 1.1005, 100000, cfg, inst)
	wide := ComputeRetestBracket("EUR/USD", "buy", 1.1010, 1.0990, 100000, cfg, inst)
	if tight.Qty <= wide.Qty {
		t.Errorf("tighter stop should size larger: tight qty=%d wide qty=%d", tight.Qty, wide.Qty)
	}
	// RR target: entry + 2×risk.
	if got, want := tight.TakeProfit, 1.1010+2*0.0005; absf(got-want) > 1e-9 {
		t.Errorf("take profit = %.5f; want %.5f", got, want)
	}
}

// absf mirrors the engine helper for the test.
func absf(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
