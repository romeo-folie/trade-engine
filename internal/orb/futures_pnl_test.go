package orb

import (
	"math"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestExitPnLF_PointValues locks the futures-multiplier fix: exit P&L must scale by the
// contract's dollar-per-point, including FRACTIONAL values (MYM = $0.50/pt) that the old
// integer exitPnLM could not represent. Forex/CFD units use pointValue 1.
func TestExitPnLF_PointValues(t *testing.T) {
	cases := []struct {
		name             string
		side             string
		entry, exit      float64
		qty              int
		pointValue, want float64
	}{
		{"MES long loss ($5/pt)", "buy", 7600, 7590, 5, 5.0, -250},      // (7590-7600)*5*5
		{"MYM long loss ($0.50/pt)", "buy", 53000, 52900, 4, 0.5, -200}, // (52900-53000)*4*0.5
		{"forex short ($1/pt)", "sell", 58, 59, 1000, 1.0, -1000},       // (58-59)*1000*1
		{"zero point value defaults to 1", "buy", 100, 110, 10, 0, 100},
	}
	for _, c := range cases {
		if got := exitPnLF(c.side, c.entry, c.exit, c.qty, c.pointValue); math.Abs(got-c.want) > 1e-6 {
			t.Errorf("%s: exitPnLF = %.4f; want %.4f", c.name, got, c.want)
		}
	}
}

func TestPointValueForTrade_FuturesRequiresDurableFractionalValue(t *testing.T) {
	s := &ORBScheduler{}
	got, err := s.pointValueForTrade(store.ORBTrade{ID: "mym", Ticker: "MYM", AssetClass: "futures", OwnerScope: "futures_index_orb", PointValue: 0.5})
	if err != nil || got != 0.5 {
		t.Fatalf("pointValueForTrade(MYM) = %v, %v; want 0.5, nil", got, err)
	}
	if _, err := s.pointValueForTrade(store.ORBTrade{ID: "unknown", Ticker: "UNKNOWN", AssetClass: "futures", OwnerScope: "futures_index_orb"}); err == nil {
		t.Fatal("missing futures point_value must fail loudly, never fall back to 1")
	}
	got, err = s.pointValueForTrade(store.ORBTrade{Ticker: "XAG/USD", AssetClass: "forex", OwnerScope: "forex_orb"})
	if err != nil || got != 1 {
		t.Fatalf("forex point value = %v, %v; want 1, nil", got, err)
	}
}

// TestExitPnLM_UnchangedForForex guards the delegation: the integer path still returns the
// exact per-unit result for the existing shares/forex callers (multiplier 1).
func TestExitPnLM_UnchangedForForex(t *testing.T) {
	if got := exitPnL("sell", 58.0, 58.02, 1000); math.Abs(got-(-20)) > 1e-6 {
		t.Errorf("forex exitPnL = %.4f; want -20", got)
	}
}

// TestPointValueForTicker: TickValue/TickSize for configured futures (MES $5, MYM $0.50),
// else 1.0 for forex or when futures mode is off — so forex exits are never rescaled.
func TestPointValueForTicker(t *testing.T) {
	s := &ORBScheduler{futuresCfg: &config.FuturesORBConfig{
		Enabled: true,
		Instruments: []config.FuturesInstrument{
			{Symbol: "MES", TickSize: 0.25, TickValue: 1.25},
			{Symbol: "MYM", TickSize: 1.00, TickValue: 0.50},
		},
	}}
	for _, tc := range []struct {
		ticker string
		want   float64
	}{
		{"MES", 5.0}, {"MYM", 0.5}, {"XAG/USD", 1.0}, {"UNKNOWN", 1.0},
	} {
		if got := s.pointValueForTicker(tc.ticker); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("pointValueForTicker(%q) = %v; want %v", tc.ticker, got, tc.want)
		}
	}
	// No futures config at all (forex-only scheduler) → always 1.
	s2 := &ORBScheduler{}
	if got := s2.pointValueForTicker("MES"); got != 1.0 {
		t.Errorf("nil futuresCfg pointValueForTicker = %v; want 1", got)
	}
}
