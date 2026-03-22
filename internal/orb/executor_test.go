package orb

import (
	"math"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func TestComputeBracketBuy(t *testing.T) {
	r := &OpeningRange{
		Ticker: "AAPL",
		High:   150.0,
		Low:    140.0,
	}
	cfg := config.ORBConfig{
		RiskReward: 2.0,
		MaxRiskPct: 1.0,
	}

	p := ComputeBracket(r, "buy", 10000, cfg)

	if p.Ticker != "AAPL" {
		t.Errorf("Ticker = %q, want AAPL", p.Ticker)
	}
	if p.Side != "buy" {
		t.Errorf("Side = %q, want buy", p.Side)
	}

	// EntryStop = 150 * 1.001 = 150.15
	expectedEntry := 150.0 * 1.001
	if math.Abs(p.EntryStop-expectedEntry) > 0.01 {
		t.Errorf("EntryStop = %f, want %f", p.EntryStop, expectedEntry)
	}

	// StopLoss = 140 * 0.999 = 139.86
	expectedStop := 140.0 * 0.999
	if math.Abs(p.StopLoss-expectedStop) > 0.01 {
		t.Errorf("StopLoss = %f, want %f", p.StopLoss, expectedStop)
	}

	// risk = 150.15 - 139.86 = 10.29
	// TakeProfit = 150.15 + 10.29*2.0 = 170.73
	risk := expectedEntry - expectedStop
	expectedTP := expectedEntry + risk*2.0
	if math.Abs(p.TakeProfit-expectedTP) > 0.01 {
		t.Errorf("TakeProfit = %f, want %f", p.TakeProfit, expectedTP)
	}

	// qty = floor(100 / 10.29) = 9
	maxRisk := 10000 * 1.0 / 100.0
	expectedQty := int(math.Floor(maxRisk / risk))
	if p.Qty != expectedQty {
		t.Errorf("Qty = %d, want %d", p.Qty, expectedQty)
	}
}

func TestComputeBracketSell(t *testing.T) {
	r := &OpeningRange{
		Ticker: "TSLA",
		High:   200.0,
		Low:    190.0,
	}
	cfg := config.ORBConfig{
		RiskReward: 2.0,
		MaxRiskPct: 2.0,
	}

	p := ComputeBracket(r, "sell", 50000, cfg)

	if p.Side != "sell" {
		t.Errorf("Side = %q, want sell", p.Side)
	}

	// EntryStop = 190 * 0.999 = 189.81
	expectedEntry := 190.0 * 0.999
	if math.Abs(p.EntryStop-expectedEntry) > 0.01 {
		t.Errorf("EntryStop = %f, want %f", p.EntryStop, expectedEntry)
	}

	// StopLoss = 200 * 1.001 = 200.20
	expectedStop := 200.0 * 1.001
	if math.Abs(p.StopLoss-expectedStop) > 0.01 {
		t.Errorf("StopLoss = %f, want %f", p.StopLoss, expectedStop)
	}

	// risk = 200.20 - 189.81 = 10.39
	risk := expectedStop - expectedEntry
	expectedTP := expectedEntry - risk*2.0
	if math.Abs(p.TakeProfit-expectedTP) > 0.01 {
		t.Errorf("TakeProfit = %f, want %f", p.TakeProfit, expectedTP)
	}

	// qty = floor(1000 / 10.39) = 96
	maxRisk := 50000 * 2.0 / 100.0
	expectedQty := int(math.Floor(maxRisk / risk))
	if p.Qty != expectedQty {
		t.Errorf("Qty = %d, want %d", p.Qty, expectedQty)
	}
}

func TestComputeBracketMinQty(t *testing.T) {
	r := &OpeningRange{
		Ticker: "BRK.A",
		High:   500000.0,
		Low:    490000.0,
	}
	cfg := config.ORBConfig{
		RiskReward: 2.0,
		MaxRiskPct: 0.01, // very low risk
	}

	p := ComputeBracket(r, "buy", 1000, cfg)

	// Risk per share is huge, max risk is tiny → qty floors to 0 but min is 1
	if p.Qty != 1 {
		t.Errorf("Qty = %d, want 1 (minimum)", p.Qty)
	}
}
