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

func TestComputeBracketSnapsEquityPricesToAlpacaTick(t *testing.T) {
	r := &OpeningRange{
		Ticker: "ATAI",
		High:   7.2286,    // high * 1.001 would have four decimals above $1.
		Low:    7.1693694, // low * 0.999 ~= 7.1622, rejected by Alpaca.
	}
	cfg := config.ORBConfig{
		RiskReward:     2.0,
		MaxRiskPct:     1.0,
		MaxPositionPct: 10,
	}

	p := ComputeBracket(r, "buy", 100000, cfg)

	for name, price := range map[string]float64{
		"entry":       p.EntryStop,
		"stop_loss":   p.StopLoss,
		"take_profit": p.TakeProfit,
	} {
		if math.Abs(price*100-math.Round(price*100)) > 1e-9 {
			t.Fatalf("%s price %v is not a penny increment", name, price)
		}
	}
	if p.StopLoss != 7.16 {
		t.Fatalf("StopLoss = %v, want 7.16", p.StopLoss)
	}
}

func TestSnapEquityPriceAllowsSubDollarFourDecimals(t *testing.T) {
	if got := snapEquityPrice(0.123456); got != 0.1235 {
		t.Fatalf("snapEquityPrice(0.123456) = %v, want 0.1235", got)
	}
	if got := snapEquityPrice(39.6196); got != 39.62 {
		t.Fatalf("snapEquityPrice(39.6196) = %v, want 39.62", got)
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

func TestComputeForexBracket_PointValue(t *testing.T) {
	// Index-futures-shaped instrument mapped onto config.Instrument: range 4990-5000,
	// buffer 5 → buy entry 5005, stop 4985, risk 20 points. With point_value 5 ($/pt per
	// contract, e.g. MES) a contract risks $100, so 1% of 100k ($1000) sizes 10 contracts;
	// without it the same trade would (wrongly) size 50.
	r := &OpeningRange{Ticker: "MES", High: 5000, Low: 4990}
	cfg := config.ForexORBConfig{RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 0} // no cap: risk sizing binds
	inst := config.Instrument{Symbol: "MES", SecType: "FUT", MinLotSize: 1, PipBuffer: 5, PointValue: 5}

	got := ComputeForexBracket(r, "buy", 100000, cfg, inst)
	if got.Qty != 10 {
		t.Errorf("qty = %d; want 10 ($1000 risk / $100-per-contract risk)", got.Qty)
	}

	t.Run("margin cap measures notional in dollars (price × point_value)", func(t *testing.T) {
		// Notional per contract = 5005 × $5/pt = $25,025; margin at 0.10 = $2,502.50.
		// 20% of 100k = $20k margin budget → 7 contracts (risk sizing alone would give 10).
		capped := cfg
		capped.MaxPositionPct = 20.0
		mInst := inst
		mInst.MarginRate = 0.10
		got := ComputeForexBracket(r, "buy", 100000, capped, mInst)
		if got.Qty != 7 {
			t.Errorf("qty = %d; want 7 ($20k margin / $2502.50 per contract)", got.Qty)
		}
	})

	t.Run("zero point_value keeps forex sizing unchanged", func(t *testing.T) {
		fx := inst
		fx.PointValue = 0
		got := ComputeForexBracket(r, "buy", 100000, cfg, fx)
		if got.Qty != 50 {
			t.Errorf("qty = %d; want 50 ($1000 risk / $20-per-unit risk at $1/pt)", got.Qty)
		}
	})
}

func TestSnapToTick(t *testing.T) {
	cases := []struct{ price, tick, want float64 }{
		{100.03, 0.25, 100.00},  // rounds down
		{100.13, 0.25, 100.25},  // rounds up
		{100.125, 0.25, 100.25}, // half rounds up (math.Round)
		{100.0, 0.25, 100.0},    // already on tick
		{1.23456, 0, 1.23456},   // tick 0 = no-op (forex)
		{1.23456, -1, 1.23456},  // negative = no-op
	}
	for _, c := range cases {
		if got := snapToTick(c.price, c.tick); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("snapToTick(%v, %v) = %v; want %v", c.price, c.tick, got, c.want)
		}
	}
}

func TestComputeForexBracket_SnapsFuturesTakeProfit(t *testing.T) {
	// A futures-shaped instrument with a fractional-tick target: range 4990-5000, buffer 5
	// → entry 5005, stop 4985, risk 20. TP = 5005 + 20×2.1 = 5047.0 (already a 0.25 multiple),
	// so use RR 2.1 with a range that yields an off-tick TP: risk 20 × 2.15 = 43 → TP 5048.0.
	// Pick RR 1.3: TP = 5005 + 20×1.3 = 5031.0 (on-tick). Use a non-multiple: risk 20×0.3=6 →
	// entry+6=5011 on-tick. Force off-tick via tick 0.25 and RR 0.075 → 20×0.075=1.5 → 5006.5
	// (on 0.25). Simplest: assert TP is always a tick multiple for a range of RRs.
	r := &OpeningRange{Ticker: "MNQ", High: 5000, Low: 4993}
	inst := config.Instrument{Symbol: "MNQ", SecType: "FUT", MinLotSize: 1, PipBuffer: 0.5, PointValue: 2, TickSize: 0.25}
	for _, rr := range []float64{1.0, 1.3, 1.5, 2.0, 2.15, 3.33} {
		cfg := config.ForexORBConfig{RiskReward: rr, MaxRiskPct: 1.0, MaxPositionPct: 0}
		for _, side := range []string{"buy", "sell"} {
			b := ComputeForexBracket(r, side, 100000, cfg, inst)
			for _, p := range []float64{b.EntryStop, b.StopLoss, b.TakeProfit} {
				if rem := math.Abs(p/0.25 - math.Round(p/0.25)); rem > 1e-6 {
					t.Errorf("rr=%.2f %s: price %.4f not a 0.25-tick multiple", rr, side, p)
				}
			}
		}
	}
}

func TestComputeForexBracket_ForexUnaffectedByTickSnap(t *testing.T) {
	// TickSize 0 (forex) must leave prices exactly as the raw arithmetic produces them.
	r := &OpeningRange{Ticker: "EUR/USD", High: 1.1010, Low: 1.1000}
	cfg := config.ForexORBConfig{RiskReward: 2.0, MaxRiskPct: 1.0, MaxPositionPct: 0}
	inst := config.Instrument{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 1, PipBuffer: 0.0002} // TickSize unset
	b := ComputeForexBracket(r, "buy", 100000, cfg, inst)
	// entry 1.1012, stop 1.0998, risk 0.0014, TP 1.1012+0.0028 = 1.1040 — exact, unrounded.
	if math.Abs(b.EntryStop-1.1012) > 1e-9 || math.Abs(b.StopLoss-1.0998) > 1e-9 || math.Abs(b.TakeProfit-1.1040) > 1e-9 {
		t.Errorf("forex bracket changed by tick snap: entry=%.6f stop=%.6f tp=%.6f", b.EntryStop, b.StopLoss, b.TakeProfit)
	}
}
