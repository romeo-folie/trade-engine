package main

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func microFuturesCfg() config.FuturesORBConfig {
	return config.FuturesORBConfig{
		Enabled: true, RangeMinutes: 15, RiskReward: 2.0, MaxRiskPct: 0.5, MaxPositionPct: 20.0,
		Instruments: []config.FuturesInstrument{
			{Symbol: "MES", TickSize: 0.25, TickValue: 1.25, TickBuffer: 2, MarginRate: 0.10},
			{Symbol: "MNQ", TickSize: 0.25, TickValue: 0.50, TickBuffer: 2, MarginRate: 0.10},
			{Symbol: "MYM", TickSize: 1.00, TickValue: 0.50, TickBuffer: 4, MarginRate: 0.10},
			{Symbol: "M2K", TickSize: 0.10, TickValue: 0.50, TickBuffer: 4, MarginRate: 0.10},
		},
	}
}

// TestBuildFuturesUniverse_ProviderProxies checks that each provider maps to its own
// index-CFD symbol set: HistData covers MES/MNQ only, Dukascopy covers all four, Exness
// local exports cover MES/MNQ/MYM, and the point value is derived from tick_value/tick_size
// regardless of provider.
func TestBuildFuturesUniverse_ProviderProxies(t *testing.T) {
	btORBInstruments = ""
	defer func() { btORBInstruments = "" }()

	t.Run("histdata covers MES and MNQ only", func(t *testing.T) {
		uni, err := buildFuturesUniverse(microFuturesCfg(), nil, "histdata")
		if err != nil {
			t.Fatal(err)
		}
		if got := symbolSet(uni); !got["MES"] || !got["MNQ"] || got["MYM"] || got["M2K"] {
			t.Errorf("histdata instruments = %v; want MES,MNQ only", keys(got))
		}
	})

	t.Run("dukascopy covers all four micros", func(t *testing.T) {
		uni, err := buildFuturesUniverse(microFuturesCfg(), nil, "dukascopy")
		if err != nil {
			t.Fatal(err)
		}
		got := symbolSet(uni)
		for _, s := range []string{"MES", "MNQ", "MYM", "M2K"} {
			if !got[s] {
				t.Errorf("dukascopy missing %s; got %v", s, keys(got))
			}
		}
		// Point value = tick_value/tick_size (MYM 0.50/1.00 = 0.5).
		for _, inst := range uni.instruments {
			if inst.Symbol == "MYM" && inst.PointValue != 0.5 {
				t.Errorf("MYM point_value = %v; want 0.5", inst.PointValue)
			}
		}
	})

	t.Run("exness covers MES MNQ and MYM", func(t *testing.T) {
		uni, err := buildFuturesUniverse(microFuturesCfg(), nil, "exness")
		if err != nil {
			t.Fatal(err)
		}
		if got := symbolSet(uni); !got["MES"] || !got["MNQ"] || !got["MYM"] || got["M2K"] {
			t.Errorf("exness instruments = %v; want MES,MNQ,MYM only", keys(got))
		}
	})

	t.Run("unknown provider errors", func(t *testing.T) {
		if _, err := buildFuturesUniverse(microFuturesCfg(), nil, "oanda"); err == nil {
			t.Error("want error for provider with no proxy mapping")
		}
	})

	t.Run("--instruments filter respects provider coverage", func(t *testing.T) {
		btORBInstruments = "MYM"
		// MYM has no histdata proxy → all requested filtered out → error.
		if _, err := buildFuturesUniverse(microFuturesCfg(), nil, "histdata"); err == nil {
			t.Error("want error: MYM has no histdata proxy")
		}
		// MYM has a dukascopy proxy → one instrument.
		uni, err := buildFuturesUniverse(microFuturesCfg(), nil, "dukascopy")
		if err != nil {
			t.Fatal(err)
		}
		if len(uni.instruments) != 1 || uni.instruments[0].Symbol != "MYM" {
			t.Errorf("instruments = %v; want [MYM]", keys(symbolSet(uni)))
		}
	})
}

func TestResearchForexInstrumentsKeepsDefaultMajorsWithConfigOverrides(t *testing.T) {
	got := researchForexInstruments([]config.Instrument{
		{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 10000, PipBuffer: 0.0003},
		{Symbol: "XAG/USD", SecType: "CMDTY", MinLotSize: 1, PipBuffer: 0.02},
	})

	bySymbol := make(map[string]config.Instrument, len(got))
	for _, inst := range got {
		bySymbol[inst.Symbol] = inst
	}
	if _, ok := bySymbol["GBP/USD"]; !ok {
		t.Fatalf("default GBP/USD missing from research forex universe: %v", got)
	}
	if usdjpy := bySymbol["USD/JPY"]; usdjpy.PipBuffer != 0.02 || usdjpy.PointValue != 0.007 {
		t.Fatalf("research USD/JPY defaults = %+v, want JPY-scale buffer and USD point-value proxy", usdjpy)
	} else if usdjpy.CommissionBpsPerSide != ibkrSpotFXCommissionBpsPerSide || usdjpy.CommissionMinPerSide != ibkrSpotFXCommissionMinPerSide {
		t.Fatalf("research USD/JPY commission = %+v, want IBKR spot-FX bps+minimum defaults", usdjpy)
	}
	if eur := bySymbol["EUR/USD"]; eur.MinLotSize != 10000 || eur.PipBuffer != 0.0003 {
		t.Fatalf("configured EUR/USD did not override default: %+v", eur)
	}
	if _, ok := bySymbol["XAG/USD"]; !ok {
		t.Fatalf("configured XAG/USD missing from research forex universe: %v", got)
	}
}

func symbolSet(uni *backtestUniverse) map[string]bool {
	m := make(map[string]bool)
	for _, i := range uni.instruments {
		m[i.Symbol] = true
	}
	return m
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
