package orb

import (
	"math"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

func TestEvaluateSweepReclaimOvernightHighShort(t *testing.T) {
	sessionOpen := time.Date(2026, 1, 5, 14, 30, 0, 0, time.UTC)
	fetchStart := sessionOpen.Add(-16 * time.Hour)
	levelEnd := sessionOpen
	inst := config.Instrument{
		Symbol:     "MYM",
		SecType:    "FUT",
		MinLotSize: 1,
		PipBuffer:  4,
		PointValue: 0.5,
		TickSize:   1,
		RiskReward: 3,
	}
	cfg := config.ForexORBConfig{RiskReward: 3, MaxRiskPct: 1, MaxPositionPct: 100}
	candles := []data.Candle{
		{Timestamp: fetchStart, Open: 40000, High: 40020, Low: 39950, Close: 40010},
		{Timestamp: sessionOpen.Add(0), Open: 40000, High: 40060, Low: 39990, Close: 40010},
		{Timestamp: sessionOpen.Add(time.Minute), Open: 40008, High: 40012, Low: 39990, Close: 40000},
	}

	sig, wait, ok := EvaluateSweepReclaim(candles, fetchStart, levelEnd, sessionOpen, sessionOpen.Add(time.Minute), inst, cfg, ReclaimParams{
		LevelSet:           "overnight",
		SweepDepthMult:     8,
		EntryWindowMinutes: 180,
		SideMode:           "both",
	}, 100000)
	if !ok || wait {
		t.Fatalf("signal ok=%v wait=%v, want ok=true wait=false", ok, wait)
	}
	if sig.Side != "sell" {
		t.Fatalf("side = %s, want sell", sig.Side)
	}
	if math.Abs(sig.Bracket.EntryStop-40008) > 1e-9 {
		t.Fatalf("entry = %v, want 40008", sig.Bracket.EntryStop)
	}
	if math.Abs(sig.Bracket.StopLoss-40064) > 1e-9 {
		t.Fatalf("stop = %v, want 40064", sig.Bracket.StopLoss)
	}
}

func TestEvaluateOpeningShockLongOnly(t *testing.T) {
	sessionOpen := time.Date(2026, 1, 5, 14, 30, 0, 0, time.UTC)
	inst := config.Instrument{
		Symbol:     "MNQ",
		SecType:    "FUT",
		MinLotSize: 1,
		PipBuffer:  0.5,
		PointValue: 2,
		TickSize:   0.25,
		RiskReward: 2.5,
	}
	cfg := config.ForexORBConfig{RiskReward: 2.5, MaxRiskPct: 1, MaxPositionPct: 100}
	var candles []data.Candle
	for i := 0; i < 30; i++ {
		close := 18000.0 - float64(i+1)
		candles = append(candles, data.Candle{
			Timestamp: sessionOpen.Add(time.Duration(i) * time.Minute),
			Open:      18000 - float64(i),
			High:      18001 - float64(i),
			Low:       close - 1,
			Close:     close,
		})
	}
	candles = append(candles, data.Candle{
		Timestamp: sessionOpen.Add(35 * time.Minute),
		Open:      17970,
		High:      17975,
		Low:       17969,
		Close:     17974,
	})

	sig, wait, ok := EvaluateOpeningShock(candles, []float64{10, 15, 20, 25, 28}, sessionOpen, sessionOpen.Add(35*time.Minute), inst, cfg, OpeningShockParams{
		WindowMinutes: 30,
		Lookback:      5,
		Percentile:    70,
		StopMult:      1,
		EntryDelayMin: 5,
		SideMode:      "long",
	}, 100000)
	if !ok || wait {
		t.Fatalf("signal ok=%v wait=%v, want ok=true wait=false", ok, wait)
	}
	if sig.Side != "buy" {
		t.Fatalf("side = %s, want buy", sig.Side)
	}
	if math.Abs(sig.Bracket.EntryStop-17970) > 1e-9 {
		t.Fatalf("entry = %v, want 17970", sig.Bracket.EntryStop)
	}
	if sig.Bracket.StopLoss >= sig.Bracket.EntryStop {
		t.Fatalf("buy stop should be below entry, got entry=%v stop=%v", sig.Bracket.EntryStop, sig.Bracket.StopLoss)
	}
}

func TestEvaluateOpeningShockSideModeBlocksShort(t *testing.T) {
	sessionOpen := time.Date(2026, 1, 5, 14, 30, 0, 0, time.UTC)
	inst := config.Instrument{Symbol: "MNQ", PipBuffer: 0.5, MinLotSize: 1, PointValue: 2, TickSize: 0.25}
	cfg := config.ForexORBConfig{RiskReward: 2.5, MaxRiskPct: 1, MaxPositionPct: 100}
	candles := []data.Candle{
		{Timestamp: sessionOpen, Open: 18000, High: 18020, Low: 17999, Close: 18020},
		{Timestamp: sessionOpen.Add(time.Minute), Open: 18020, High: 18032, Low: 18018, Close: 18030},
		{Timestamp: sessionOpen.Add(2 * time.Minute), Open: 18030, High: 18031, Low: 18025, Close: 18026},
	}
	_, wait, ok := EvaluateOpeningShock(candles, []float64{1, 2, 3}, sessionOpen, sessionOpen.Add(2*time.Minute), inst, cfg, OpeningShockParams{
		WindowMinutes: 2,
		Lookback:      3,
		Percentile:    70,
		StopMult:      1,
		SideMode:      "long",
	}, 100000)
	if ok || wait {
		t.Fatalf("short shock should be blocked by long-only side mode, ok=%v wait=%v", ok, wait)
	}
}
