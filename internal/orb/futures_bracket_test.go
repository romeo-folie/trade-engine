package orb

import (
	"math"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// esInst is a standard E-mini S&P 500 instrument config used across tests.
var esInst = config.FuturesInstrument{
	Symbol:     "ES",
	Exchange:   "CME",
	AssetClass: "equity_index",
	ExpiryCycle: "quarterly",
	TickSize:   0.25,
	TickValue:  12.50,
	TickBuffer: 2,
}

// nqInst is a standard E-mini Nasdaq-100 instrument config.
var nqInst = config.FuturesInstrument{
	Symbol:     "NQ",
	Exchange:   "CME",
	AssetClass: "equity_index",
	ExpiryCycle: "quarterly",
	TickSize:   0.25,
	TickValue:  5.00,
	TickBuffer: 2,
}

// ymInst is a standard E-mini Dow Jones instrument config (whole-tick size).
var ymInst = config.FuturesInstrument{
	Symbol:     "YM",
	Exchange:   "CBOT",
	AssetClass: "equity_index",
	ExpiryCycle: "quarterly",
	TickSize:   1.00,
	TickValue:  5.00,
	TickBuffer: 4,
}

var stdFuturesCfg = config.FuturesORBConfig{
	Enabled:        true,
	RangeMinutes:   15,
	RiskReward:     2.0,
	MaxRiskPct:     1.0,
	MaxPositionPct: 0, // 0 disables the notional cap — isolated in dedicated cap tests below
}

func TestComputeFuturesBracket_ES_Buy(t *testing.T) {
	// Range: High=5100, Low=5080. Buffer = 2 ticks × 0.25 = 0.50
	// entryStop = 5100.50, stopLoss = 5079.50
	// riskTicks = 21 / 0.25 = 84 ticks; riskPerContract = 84 × $12.50 = $1050
	// portfolioValue=$250k, maxRiskDollars=1%=$2500, qty=floor(2500/1050)=2
	r := &OpeningRange{Ticker: "ES", High: 5100, Low: 5080}
	params := ComputeFuturesBracket(r, "buy", 250_000, stdFuturesCfg, esInst)

	assertClose(t, "EntryStop", params.EntryStop, 5100.50)
	assertClose(t, "StopLoss", params.StopLoss, 5079.50)
	expectedTP := 5100.50 + (5100.50-5079.50)*2.0
	assertClose(t, "TakeProfit", params.TakeProfit, expectedTP)
	if params.Qty != 2 {
		t.Errorf("Qty: want 2, got %d", params.Qty)
	}
	if params.SecType != "FUT" {
		t.Errorf("SecType: want FUT, got %q", params.SecType)
	}
	if params.Side != "buy" {
		t.Errorf("Side: want buy, got %q", params.Side)
	}
}

func TestComputeFuturesBracket_ES_Sell(t *testing.T) {
	// Sell-side symmetry: entryStop below range low, stopLoss above range high.
	r := &OpeningRange{Ticker: "ES", High: 5100, Low: 5080}
	params := ComputeFuturesBracket(r, "sell", 250_000, stdFuturesCfg, esInst)

	assertClose(t, "EntryStop", params.EntryStop, 5079.50) // Low - buf
	assertClose(t, "StopLoss", params.StopLoss, 5100.50)   // High + buf
	if params.TakeProfit >= params.EntryStop {
		t.Errorf("sell TakeProfit (%v) should be below EntryStop (%v)", params.TakeProfit, params.EntryStop)
	}
	if params.SecType != "FUT" {
		t.Errorf("SecType: want FUT, got %q", params.SecType)
	}
}

func TestComputeFuturesBracket_NQ_Sell(t *testing.T) {
	// NQ: TickValue=$5, same tick size. At $100k portfolio, qty should be smaller than ES.
	r := &OpeningRange{Ticker: "NQ", High: 19000, Low: 18950}
	params := ComputeFuturesBracket(r, "sell", 100_000, stdFuturesCfg, nqInst)

	if params.SecType != "FUT" {
		t.Errorf("SecType: want FUT, got %q", params.SecType)
	}
	if params.Qty < 1 {
		t.Errorf("Qty must be >= 1 for a valid range")
	}
	// entryStop = 18950 - 0.50 = 18949.50; stopLoss = 19000 + 0.50 = 19000.50
	assertClose(t, "EntryStop", params.EntryStop, 18949.50)
	assertClose(t, "StopLoss", params.StopLoss, 19000.50)
}

func TestComputeFuturesBracket_YM_WholeTickSize(t *testing.T) {
	// YM: TickSize=1.0, TickBuffer=4 → buffer=4 points.
	// No fractional tick should appear in entry/stop prices.
	r := &OpeningRange{Ticker: "YM", High: 42000, Low: 41950}
	params := ComputeFuturesBracket(r, "buy", 200_000, stdFuturesCfg, ymInst)

	// entryStop must be a whole number (TickSize=1.0, TickBuffer=4 → +4 pts)
	if params.EntryStop != math.Trunc(params.EntryStop) {
		t.Errorf("YM EntryStop has fractional ticks: %v", params.EntryStop)
	}
	if params.StopLoss != math.Trunc(params.StopLoss) {
		t.Errorf("YM StopLoss has fractional ticks: %v", params.StopLoss)
	}
	assertClose(t, "EntryStop", params.EntryStop, 42004)
	assertClose(t, "StopLoss", params.StopLoss, 41946)
}

func TestComputeFuturesBracket_NarrowRange_MinOneTick(t *testing.T) {
	// A 1-tick-wide range should still produce a valid bracket with qty >= 1.
	r := &OpeningRange{Ticker: "ES", High: 5000.25, Low: 5000.00} // 1-tick range
	params := ComputeFuturesBracket(r, "buy", 500_000, stdFuturesCfg, esInst)

	if params.Qty < 1 {
		t.Errorf("Qty should be >= 1 even for a 1-tick range, got %d", params.Qty)
	}
	if params.SecType != "FUT" {
		t.Errorf("SecType: want FUT, got %q", params.SecType)
	}
}

func TestComputeFuturesBracket_NotionalCap_ZeroQty(t *testing.T) {
	// A small portfolio with a tight MaxPositionPct should return qty=0
	// when even 1 contract exceeds the cap.
	// ES at 5000: contractMultiplier = $12.50/0.25 = $50/pt
	// notionalPerContract = 5000 × $50 = $250,000
	// portfolioValue=$50k, MaxPositionPct=10% → maxNotional=$5k < $250k → qty=0
	r := &OpeningRange{Ticker: "ES", High: 5000, Low: 4980}
	cfg := stdFuturesCfg
	cfg.MaxPositionPct = 10.0
	params := ComputeFuturesBracket(r, "buy", 50_000, cfg, esInst)

	if params.Qty != 0 {
		t.Errorf("expected qty=0 (account too small for 1 ES contract), got %d", params.Qty)
	}
	if params.SecType != "FUT" {
		t.Errorf("SecType: want FUT, got %q", params.SecType)
	}
}

func TestComputeFuturesBracket_NotionalCap_Clamps(t *testing.T) {
	// ES at ~5000: contractMultiplier = $12.50/0.25 = $50/pt
	// notionalPerContract = 5000.50 × $50 = $250,025
	// portfolioValue=$1M, MaxPositionPct=50% → maxNotional=$500k → maxQtyByNotional=floor(500k/250025)=1
	// MaxRiskPct=10%, 1-tick risk → riskPerContract=$12.50 → risk-based qty=floor(100k/12.50)=8000
	// Notional cap clamps 8000 → 1
	r := &OpeningRange{Ticker: "ES", High: 5000.25, Low: 5000.00} // 1-tick range
	cfg := stdFuturesCfg
	cfg.MaxRiskPct = 10.0
	cfg.MaxPositionPct = 50.0
	params := ComputeFuturesBracket(r, "buy", 1_000_000, cfg, esInst)

	// maxQtyByNotional = floor($500k / $250,025) = 1
	if params.Qty != 1 {
		t.Errorf("notional cap should clamp to 1, got %d", params.Qty)
	}
}

func TestComputeFuturesBracket_SecType_AlwaysFUT(t *testing.T) {
	r := &OpeningRange{Ticker: "RTY", High: 2100, Low: 2090}
	rtyInst := config.FuturesInstrument{
		Symbol: "RTY", Exchange: "CME", AssetClass: "equity_index",
		ExpiryCycle: "quarterly", TickSize: 0.10, TickValue: 5.00, TickBuffer: 4,
	}
	params := ComputeFuturesBracket(r, "buy", 100_000, stdFuturesCfg, rtyInst)
	if params.SecType != "FUT" {
		t.Errorf("SecType must always be FUT, got %q", params.SecType)
	}
}

// assertClose checks two float64 values are within 0.001 of each other.
func assertClose(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.001 {
		t.Errorf("%s: want %.4f, got %.4f", name, want, got)
	}
}
