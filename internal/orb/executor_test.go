package orb

import (
	"context"
	"math"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/execution"
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

func TestPlaceBracket_OrderShapes(t *testing.T) {
	// Stop-touch entry: a STOP bracket at the breakout level.
	stopExec := &flattenExec{placeResult: execution.OrderResult{BrokerOrderID: "o1"}}
	_, _, _, err := PlaceBracket(context.Background(), stopExec, BracketParams{
		Ticker: "EUR/USD", Side: "buy", Qty: 5000, EntryStop: 1.1012, StopLoss: 1.0998, TakeProfit: 1.1040,
	})
	if err != nil {
		t.Fatalf("PlaceBracket: %v", err)
	}
	if len(stopExec.placed) != 1 {
		t.Fatalf("placed %d; want 1", len(stopExec.placed))
	}
	if got := stopExec.placed[0]; got.Type != execution.OrderTypeStop || got.StopPrice != 1.1012 || got.OrderClass != "bracket" {
		t.Errorf("stop bracket = type %q stop %.4f class %q; want stop/1.1012/bracket", got.Type, got.StopPrice, got.OrderClass)
	}

	// Confirmation entry: a MARKET bracket — no resting stop at the breakout level (which
	// would re-fill on a pullback wick, defeating confirmation).
	mktExec := &flattenExec{placeResult: execution.OrderResult{BrokerOrderID: "o2"}}
	_, _, _, err = PlaceMarketBracket(context.Background(), mktExec, BracketParams{
		Ticker: "EUR/USD", Side: "buy", Qty: 5000, EntryStop: 1.1012, StopLoss: 1.0998, TakeProfit: 1.1040,
	})
	if err != nil {
		t.Fatalf("PlaceMarketBracket: %v", err)
	}
	if got := mktExec.placed[0]; got.Type != execution.OrderTypeMarket || got.StopPrice != 0 || got.OrderClass != "bracket" {
		t.Errorf("market bracket = type %q stop %.4f class %q; want market/0/bracket", got.Type, got.StopPrice, got.OrderClass)
	}
	// The protective TP/SL legs are still set on the market bracket.
	if mktExec.placed[0].StopLossPrice != 1.0998 || mktExec.placed[0].TakeProfitPrice != 1.1040 {
		t.Errorf("market bracket legs = sl %.4f tp %.4f; want 1.0998/1.1040", mktExec.placed[0].StopLossPrice, mktExec.placed[0].TakeProfitPrice)
	}
}

func TestComputeForexBracket_RiskMultiplier(t *testing.T) {
	r := &OpeningRange{Ticker: "EUR/USD", High: 1.1010, Low: 1.1000}
	cfg := config.ForexORBConfig{RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 0} // no notional cap so risk sizing binds
	base := config.Instrument{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 1, PipBuffer: 0.0002}

	full := ComputeForexBracket(r, "buy", 100000, cfg, base)
	half := base
	half.RiskMultiplier = 0.5
	scaled := ComputeForexBracket(r, "buy", 100000, cfg, half)

	if scaled.Qty == 0 || full.Qty == 0 {
		t.Fatalf("unexpected zero qty: full=%d scaled=%d", full.Qty, scaled.Qty)
	}
	// 0.5× multiplier ≈ half the size (allow ±1 lot for floor rounding).
	if diff := full.Qty/2 - scaled.Qty; diff < -1 || diff > 1 {
		t.Errorf("risk_multiplier 0.5: scaled qty %d; want ≈ half of %d", scaled.Qty, full.Qty)
	}
}

func TestComputeForexBracket_MarginRate(t *testing.T) {
	// Range 1.1000-1.1010, pip 0.0002 → buy entry 1.1012, stop 1.0998, risk 0.0014.
	// Risk sizing targets $1000 (1% of 100k) → ~714k units, so the notional cap binds.
	r := &OpeningRange{Ticker: "EUR/USD", High: 1.1010, Low: 1.1000}
	cfg := config.ForexORBConfig{RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 10.0}
	base := config.Instrument{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002}

	t.Run("unset margin_rate keeps the full-notional cap", func(t *testing.T) {
		// 100k × 10% / 1.1012 = 9081 → lot-floored 5000.
		got := ComputeForexBracket(r, "buy", 100000, cfg, base)
		if got.Qty != 5000 {
			t.Errorf("qty = %d; want 5000 (full-notional cap, min lot)", got.Qty)
		}
	})
	t.Run("margin_rate caps margin consumed, not notional", func(t *testing.T) {
		// 100k × 10% / (1.1012 × 0.033) = 275,177 → lot-floored 275,000.
		inst := base
		inst.MarginRate = 0.033
		got := ComputeForexBracket(r, "buy", 100000, cfg, inst)
		if got.Qty != 275000 {
			t.Errorf("qty = %d; want 275000 (margin-based cap at 30:1)", got.Qty)
		}
	})
	t.Run("risk sizing still binds when it is the smaller of the two", func(t *testing.T) {
		// margin cap 275,177 > risk-target 714,285? No — risk target is larger here, so
		// widen the stop instead: risk 0.0140 → target 71,428 < margin cap → risk binds.
		wide := &OpeningRange{Ticker: "EUR/USD", High: 1.1104, Low: 1.0968} // risk 0.0140 after ±pip
		inst := base
		inst.MarginRate = 0.033
		got := ComputeForexBracket(wide, "buy", 100000, cfg, inst)
		if got.Qty != 70000 {
			t.Errorf("qty = %d; want 70000 (risk-sized 71,428 lot-floored)", got.Qty)
		}
	})
	t.Run("margin_rate above 1 is ignored", func(t *testing.T) {
		inst := base
		inst.MarginRate = 33 // leverage ratio typo — validation rejects it; sizing must not amplify
		got := ComputeForexBracket(r, "buy", 100000, cfg, inst)
		if got.Qty != 5000 {
			t.Errorf("qty = %d; want 5000 (invalid rate falls back to full notional)", got.Qty)
		}
	})
}

func TestComputeForexBracket_MaxNotionalUSD(t *testing.T) {
	// Silver-shaped scenario: a tight ORB stop on a ~$62 instrument lets the margin cap
	// size a small-dollar-risk trade into a >$100k notional that trips IBKR's precautionary
	// order-value limit. Range 62.50–62.70, pip 0.05 → buy entry 62.75, stop 62.45.
	r := &OpeningRange{Ticker: "XAG/USD", High: 62.70, Low: 62.50}
	cfg := config.ForexORBConfig{RiskReward: 2.0, MaxRiskPct: 0.5, MaxPositionPct: 25.0}
	base := config.Instrument{Symbol: "XAG/USD", SecType: "CFD", MinLotSize: 50, PipBuffer: 0.05, MarginRate: 0.10}

	t.Run("no cap → margin sizing produces a >$100k notional", func(t *testing.T) {
		got := ComputeForexBracket(r, "buy", 100000, cfg, base)
		notional := float64(got.Qty) * got.EntryStop
		if notional <= 100000 {
			t.Fatalf("expected an uncapped notional over $100k to reproduce the bug; got qty=%d notional=%.0f", got.Qty, notional)
		}
	})

	t.Run("cap bounds order value under the limit", func(t *testing.T) {
		inst := base
		inst.MaxNotionalUSD = 95000
		got := ComputeForexBracket(r, "buy", 100000, cfg, inst)
		notional := float64(got.Qty) * got.EntryStop
		if got.Qty == 0 {
			t.Fatalf("cap zeroed a tradeable size; qty=%d", got.Qty)
		}
		if notional > 95000 {
			t.Errorf("notional %.0f exceeds the $95k cap (qty=%d, entry=%.2f)", notional, got.Qty, got.EntryStop)
		}
		if got.Qty%inst.MinLotSize != 0 {
			t.Errorf("capped qty %d is not a multiple of min lot %d", got.Qty, inst.MinLotSize)
		}
	})

	t.Run("cap larger than the sized notional is a no-op", func(t *testing.T) {
		inst := base
		uncapped := ComputeForexBracket(r, "buy", 100000, cfg, base)
		inst.MaxNotionalUSD = 10_000_000 // far above any sized notional
		got := ComputeForexBracket(r, "buy", 100000, cfg, inst)
		if got.Qty != uncapped.Qty {
			t.Errorf("qty = %d; want %d (a loose cap must not change sizing)", got.Qty, uncapped.Qty)
		}
	})
}
