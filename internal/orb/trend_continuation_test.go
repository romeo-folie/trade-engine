package orb

import (
	"math"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

func TestParseRangeBands(t *testing.T) {
	bands, err := ParseRangeBands("32:60, 80:120")
	if err != nil {
		t.Fatalf("ParseRangeBands returned error: %v", err)
	}
	if len(bands) != 2 {
		t.Fatalf("bands = %d, want 2", len(bands))
	}
	if bands[0] != (RangeBand{Min: 32, Max: 60}) {
		t.Fatalf("first band = %+v, want 32:60", bands[0])
	}
	if bands[1] != (RangeBand{Min: 80, Max: 120}) {
		t.Fatalf("second band = %+v, want 80:120", bands[1])
	}

	for _, input := range []string{"32", "60:32", "-1:10", "10:0", "x:10", "10:y", "10:20,"} {
		if _, err := ParseRangeBands(input); err == nil {
			t.Fatalf("ParseRangeBands(%q) returned nil error", input)
		}
	}
}

func TestRangeExcluded(t *testing.T) {
	bands := []RangeBand{{Min: 32, Max: 60}}
	tests := []struct {
		rangePips float64
		want      bool
	}{
		{rangePips: 31.99, want: false},
		{rangePips: 32.00, want: true},
		{rangePips: 59.99, want: true},
		{rangePips: 60.00, want: false},
	}
	for _, tt := range tests {
		if got := RangeExcluded(tt.rangePips, bands); got != tt.want {
			t.Fatalf("RangeExcluded(%v) = %v, want %v", tt.rangePips, got, tt.want)
		}
	}
}

func TestEvaluateTrendContinuationBuyContrarian(t *testing.T) {
	sessionOpen := time.Date(2026, 1, 5, 13, 30, 0, 0, time.UTC)
	cfg := config.ForexORBConfig{
		RangeMinutes:   30,
		RiskReward:     2.5,
		MaxRiskPct:     1,
		MaxPositionPct: 100,
	}
	inst := config.Instrument{
		Symbol:     "XAG/USD",
		SecType:    "CFD",
		MinLotSize: 1,
		PipBuffer:  0.01,
	}
	candles := make([]data.Candle, 0, 70)
	candles = append(candles,
		data.Candle{Timestamp: sessionOpen.Add(-60 * time.Minute), Open: 25.50, High: 25.55, Low: 25.45, Close: 25.50},
		data.Candle{Timestamp: sessionOpen.Add(-1 * time.Minute), Open: 25.20, High: 25.25, Low: 25.15, Close: 25.20},
	)
	for i := 0; i < 30; i++ {
		ts := sessionOpen.Add(time.Duration(i) * time.Minute)
		candles = append(candles, data.Candle{Timestamp: ts, Open: 25.00, High: 25.20, Low: 25.00, Close: 25.15})
	}
	rangeEnd := sessionOpen.Add(30 * time.Minute)
	for i := 0; i < 40; i++ {
		ts := rangeEnd.Add(time.Duration(i) * time.Minute)
		close := 25.26
		if i >= 26 && i <= 30 {
			close = 25.24
		}
		open := close
		if i == 31 {
			open = 25.31
		}
		candles = append(candles, data.Candle{Timestamp: ts, Open: open, High: open + 0.03, Low: open - 0.03, Close: close})
	}
	r := &OpeningRange{Ticker: "XAG/USD", High: 25.20, Low: 25.00, DefinedAt: rangeEnd.Add(-time.Minute)}
	sig, ok := EvaluateTrendContinuation(candles, r, sessionOpen, rangeEnd.Add(39*time.Minute), inst, cfg, TrendContinuationParams{
		DelayMinutes:        30,
		ConfirmBars:         5,
		DepthMult:           3,
		TrendFilter:         TrendFilterContrarian,
		TrendLookbackMinute: 60,
		MinRangePips:        14,
	}, 100000)
	if !ok {
		t.Fatal("EvaluateTrendContinuation did not produce a signal")
	}
	if sig.Side != "buy" {
		t.Fatalf("side = %s, want buy", sig.Side)
	}
	if sig.EntryIndex != 31 {
		t.Fatalf("entry index = %d, want 31", sig.EntryIndex)
	}
	if math.Abs(sig.Bracket.EntryStop-25.31) > 0.000001 {
		t.Fatalf("entry = %v, want 25.31", sig.Bracket.EntryStop)
	}
	if math.Abs(sig.Bracket.StopLoss-24.99) > 0.000001 {
		t.Fatalf("stop = %v, want 24.99", sig.Bracket.StopLoss)
	}
}

func TestEvaluateTrendContinuationRangeExcluded(t *testing.T) {
	sessionOpen := time.Date(2026, 1, 5, 13, 30, 0, 0, time.UTC)
	r := &OpeningRange{Ticker: "XAG/USD", High: 25.20, Low: 25.00}
	_, ok := EvaluateTrendContinuation(nil, r, sessionOpen, sessionOpen.Add(time.Hour), config.Instrument{
		Symbol: "XAG/USD", PipBuffer: 0.01,
	}, config.ForexORBConfig{RangeMinutes: 30}, TrendContinuationParams{
		DelayMinutes: 30,
		ConfirmBars:  5,
		DepthMult:    3,
		ExcludeRange: []RangeBand{{Min: 19, Max: 21}},
	}, 100000)
	if ok {
		t.Fatal("EvaluateTrendContinuation produced a signal for an excluded range")
	}
}
