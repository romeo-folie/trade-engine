package orb

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

// qMarketData is a fake MarketDataProvider for the entry-quality filters: it returns a
// fixed candle set (for the ATR lookup) and a fixed quote (for the spread check).
type qMarketData struct {
	candles  []data.Candle
	quote    *data.Quote
	quoteErr error
}

func (m *qMarketData) FetchCandles(_ context.Context, _ string, _ data.Timeframe, _, _ time.Time) ([]data.Candle, error) {
	return m.candles, nil
}
func (m *qMarketData) FetchQuote(context.Context, string) (*data.Quote, error) {
	return m.quote, m.quoteErr
}
func (m *qMarketData) FetchHistory(context.Context, string, data.Timeframe, int) ([]data.Candle, error) {
	return nil, nil
}

// atrCandles builds 15 bars with a constant true range of 0.0010, so ComputeATR(_,14)
// returns 0.0010 — a known denominator for range_atr_ratio.
func atrCandles() []data.Candle {
	var out []data.Candle
	base := time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC)
	for i := range 15 {
		out = append(out, data.Candle{
			Timestamp: base.Add(time.Duration(i) * time.Hour),
			High:      1.1010, Low: 1.1000, Open: 1.1005, Close: 1.1005, // H-L = 0.0010
		})
	}
	return out
}

func qScheduler(md data.MarketDataProvider) *ORBScheduler {
	return &ORBScheduler{
		marketData: md,
		forexCfg:   &config.ForexORBConfig{Enabled: true, ATRPeriod: 14, ATRTimeframe: "1h"},
		now:        func() time.Time { return time.Date(2026, 4, 13, 9, 0, 0, 0, time.UTC) },
	}
}

func TestPassesRangeQuality(t *testing.T) {
	s := qScheduler(&qMarketData{candles: atrCandles()})               // ATR = 0.0010
	rng := &OpeningRange{Ticker: "EUR/USD", High: 1.1005, Low: 1.1000} // width 0.0005 → ratio 0.50

	t.Run("disabled when both bounds zero", func(t *testing.T) {
		ok, _, _ := s.passesRangeQuality(context.Background(), "EUR/USD", rng, config.Instrument{})
		if !ok {
			t.Error("filter disabled (0/0) should always pass")
		}
	})
	t.Run("in band", func(t *testing.T) {
		ok, ratio, _ := s.passesRangeQuality(context.Background(), "EUR/USD", rng, config.Instrument{MinRangeATR: 0.25, MaxRangeATR: 0.90})
		if !ok {
			t.Errorf("ratio %.2f should be in [0.25, 0.90]", ratio)
		}
	})
	t.Run("below min", func(t *testing.T) {
		narrow := &OpeningRange{High: 1.1002, Low: 1.1000} // width 0.0002 → ratio 0.20
		ok, _, _ := s.passesRangeQuality(context.Background(), "EUR/USD", narrow, config.Instrument{MinRangeATR: 0.25})
		if ok {
			t.Error("ratio 0.20 should be rejected by min 0.25")
		}
	})
	t.Run("above max", func(t *testing.T) {
		wide := &OpeningRange{High: 1.1015, Low: 1.1000} // width 0.0015 → ratio 1.50
		ok, _, _ := s.passesRangeQuality(context.Background(), "EUR/USD", wide, config.Instrument{MaxRangeATR: 0.90})
		if ok {
			t.Error("ratio 1.50 should be rejected by max 0.90")
		}
	})
	t.Run("fail-open when ATR unavailable", func(t *testing.T) {
		empty := qScheduler(&qMarketData{candles: nil}) // ComputeATR → 0
		ok, _, _ := empty.passesRangeQuality(context.Background(), "EUR/USD", rng, config.Instrument{MinRangeATR: 0.25, MaxRangeATR: 0.90})
		if !ok {
			t.Error("missing ATR must fail open (allow the trade), not block it")
		}
	})
}

func TestPassesSpread(t *testing.T) {
	// entryStop 1.1012, stopLoss 1.0998 → risk 0.0014.
	const entryStop, stopLoss = 1.1012, 1.0998

	t.Run("disabled when zero", func(t *testing.T) {
		s := qScheduler(&qMarketData{quote: &data.Quote{Bid: 1.1000, Ask: 1.1050}})
		ok, _, _ := s.passesSpread(context.Background(), "EUR/USD", entryStop, stopLoss, 0)
		if !ok {
			t.Error("disabled (0) should always pass")
		}
	})
	t.Run("within bound", func(t *testing.T) {
		// spread 0.0002 / risk 0.0014 = 0.143 ≤ 0.20.
		s := qScheduler(&qMarketData{quote: &data.Quote{Bid: 1.1000, Ask: 1.1002}})
		ok, ratio, _ := s.passesSpread(context.Background(), "EUR/USD", entryStop, stopLoss, 0.20)
		if !ok {
			t.Errorf("spread/risk %.3f should pass max 0.20", ratio)
		}
	})
	t.Run("too wide", func(t *testing.T) {
		// 0.143 > 0.10.
		s := qScheduler(&qMarketData{quote: &data.Quote{Bid: 1.1000, Ask: 1.1002}})
		ok, _, _ := s.passesSpread(context.Background(), "EUR/USD", entryStop, stopLoss, 0.10)
		if ok {
			t.Error("spread/risk 0.143 should be rejected by max 0.10")
		}
	})
	t.Run("fail-open when no quote", func(t *testing.T) {
		s := qScheduler(&qMarketData{quote: nil})
		ok, _, _ := s.passesSpread(context.Background(), "EUR/USD", entryStop, stopLoss, 0.10)
		if !ok {
			t.Error("missing quote must fail open, not block the trade")
		}
	})
}

func TestPassesCommission(t *testing.T) {
	// entryStop 1.1012, stopLoss 1.0998 → risk/unit 0.0014; qty 5000 → dollar risk $7.
	// commission_per_side $2 → round trip $4 → ratio 0.571.
	const entryStop, stopLoss = 1.1012, 1.0998
	inst := config.Instrument{Symbol: "EUR/USD", CommissionPerSide: 2.0, MaxCommissionToRisk: 0.05}

	t.Run("disabled when max is zero", func(t *testing.T) {
		off := inst
		off.MaxCommissionToRisk = 0
		if ok, _, _ := passesCommission(5000, entryStop, stopLoss, off); !ok {
			t.Error("disabled (max 0) should always pass")
		}
	})
	t.Run("disabled when commission estimate is zero", func(t *testing.T) {
		off := inst
		off.CommissionPerSide = 0
		if ok, _, _ := passesCommission(5000, entryStop, stopLoss, off); !ok {
			t.Error("no commission estimate should pass (config validation rejects this combo)")
		}
	})
	t.Run("min-lot dollar risk is rejected", func(t *testing.T) {
		ok, ratio, reason := passesCommission(5000, entryStop, stopLoss, inst)
		if ok {
			t.Errorf("commission/risk %.3f should be rejected by max 0.05 (%s)", ratio, reason)
		}
	})
	t.Run("margin-sized qty passes", func(t *testing.T) {
		// qty 275,000 → dollar risk $385 → ratio 0.0104 ≤ 0.05.
		ok, ratio, _ := passesCommission(275000, entryStop, stopLoss, inst)
		if !ok {
			t.Errorf("commission/risk %.4f should pass max 0.05", ratio)
		}
	})
	t.Run("zero qty passes through to the existing cannot-trade path", func(t *testing.T) {
		if ok, _, _ := passesCommission(0, entryStop, stopLoss, inst); !ok {
			t.Error("zero qty has no dollar risk to measure; the qty==0 skip handles it")
		}
	})
	t.Run("split-bracket instrument pays four fills", func(t *testing.T) {
		// qty 70,000 → dollar risk $98. Plain bracket: $4 round trip → 0.041 ≤ 0.05.
		// With a partial configured, ScaleOutBrackets places TWO orders (2 entry + 2 exit
		// fills) → $8 → 0.082 > 0.05: the same size must now be rejected.
		if ok, ratio, _ := passesCommission(70000, entryStop, stopLoss, inst); !ok {
			t.Fatalf("plain bracket ratio %.3f should pass max 0.05", ratio)
		}
		split := inst
		split.PartialTakeProfitR = 1.0
		split.PartialFraction = 0.5
		ok, ratio, _ := passesCommission(70000, entryStop, stopLoss, split)
		if ok {
			t.Errorf("split-bracket ratio %.3f should be rejected by max 0.05 (4 fills, not 2)", ratio)
		}
	})
}

func TestCloseFractionEligibility(t *testing.T) {
	// Threshold 0.6: buy needs CloseFrac >= 0.6, sell needs <= 0.4; [0.4,0.6] is the dead zone.
	inst := config.Instrument{CloseFracThreshold: 0.6}

	cases := []struct {
		name              string
		frac              float64
		wantBuy, wantSell bool
	}{
		{"closed at high arms buy only", 1.0, true, false},
		{"just above threshold arms buy only", 0.6, true, false},
		{"mid-range disables both (dead zone)", 0.5, false, false},
		{"just inside dead zone lower disables both", 0.4, false, true}, // 0.4 <= 1-0.6=0.4 → sell ok
		{"closed at low arms sell only", 0.0, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &OpeningRange{CloseFrac: tc.frac}
			buyOK, sellOK, applied := CloseFractionEligibility(r, inst)
			if !applied {
				t.Fatal("guard should be applied when threshold > 0")
			}
			if buyOK != tc.wantBuy || sellOK != tc.wantSell {
				t.Errorf("frac %.2f: got buy=%v sell=%v, want buy=%v sell=%v",
					tc.frac, buyOK, sellOK, tc.wantBuy, tc.wantSell)
			}
		})
	}

	t.Run("threshold 0 disables the guard (both sides, not-applied)", func(t *testing.T) {
		r := &OpeningRange{CloseFrac: 0.5}
		buyOK, sellOK, applied := CloseFractionEligibility(r, config.Instrument{CloseFracThreshold: 0})
		if applied || !buyOK || !sellOK {
			t.Errorf("threshold 0 should be inert: got buy=%v sell=%v applied=%v", buyOK, sellOK, applied)
		}
	})

	t.Run("nil range is inert", func(t *testing.T) {
		if _, _, applied := CloseFractionEligibility(nil, inst); applied {
			t.Error("nil range should not apply the guard")
		}
	})
}

func TestRangeBufferCloseFrac(t *testing.T) {
	base := time.Date(2026, 4, 13, 9, 30, 0, 0, time.UTC)

	t.Run("close near high yields high frac", func(t *testing.T) {
		buf := NewRangeBuffer("T", 2)
		buf.Add(data.Candle{Timestamp: base, High: 10, Low: 8, Close: 9})
		_, r := buf.Add(data.Candle{Timestamp: base.Add(time.Minute), High: 12, Low: 9, Close: 11.5})
		// range: high=12, low=8, last close=11.5 → (11.5-8)/(12-8) = 0.875
		if r.CloseFrac < 0.87 || r.CloseFrac > 0.88 {
			t.Errorf("CloseFrac = %.4f, want ~0.875", r.CloseFrac)
		}
	})

	t.Run("degenerate zero-width range yields neutral 0.5", func(t *testing.T) {
		buf := NewRangeBuffer("T", 1)
		_, r := buf.Add(data.Candle{Timestamp: base, High: 10, Low: 10, Close: 10})
		if r.CloseFrac != 0.5 {
			t.Errorf("CloseFrac = %.4f, want 0.5 for zero-width range", r.CloseFrac)
		}
	})
}

func TestPassesCommission_PointValue(t *testing.T) {
	// Futures-shaped: entry-stop distance 20, qty 2, commission $0.85/side, gate 0.06.
	// With PointValue 2 (MNQ): dollar risk = 20 × 2 × 2 = $80; round trip $1.70 → 0.021 ≤ 0.06 PASS.
	// Without the point-value multiply, risk would read 20 × 2 = $40 → 0.0425 — still under 0.06
	// here, so use a tighter gate to prove the multiplier changes the verdict:
	inst := config.Instrument{Symbol: "MNQ", PointValue: 2, CommissionPerSide: 0.85, MaxCommissionToRisk: 0.03}
	// entry 5005, stop 4985 → distance 20.
	if ok, ratio, reason := passesCommission(2, 5005, 4985, inst); !ok {
		t.Errorf("MNQ commission/risk %.4f should pass 0.03 with point value applied (%s)", ratio, reason)
	}
	// Same numbers as a $1/point instrument (pv unset) → risk $40 → ratio 0.0425 > 0.03 → REJECT.
	// This is the mis-scaling Codex flagged: forgetting the multiplier wrongly disables the side.
	flat := inst
	flat.PointValue = 0
	if ok, ratio, _ := passesCommission(2, 5005, 4985, flat); ok {
		t.Errorf("without point value the ratio %.4f exceeds 0.03 and must reject (proves pv matters)", ratio)
	}
}

func TestPassesCommission_BpsWithMinimum(t *testing.T) {
	inst := config.Instrument{
		Symbol: "XAG/USD", CommissionBpsPerSide: 3.0, CommissionMinPerSide: 2.0,
		MaxCommissionToRisk: 0.05,
	}
	// 1600 oz × $58.125 × 3 bps = $27.90/side; round trip ~= $55.80.
	// Planned risk: 0.80 × 1600 = $1280, ratio ~= 0.0436, so it passes 5%.
	ok, ratio, reason := passesCommission(1600, 58.125, 57.325, inst)
	if !ok {
		t.Fatalf("commission/risk %.4f should pass (%s)", ratio, reason)
	}
	if ratio < 0.043 || ratio > 0.044 {
		t.Fatalf("ratio = %.6f, want about 0.0436", ratio)
	}

	// Same notional but a tight stop leaves too little planned risk for IBKR's bps cost.
	ok, ratio, _ = passesCommission(1600, 58.125, 57.825, inst)
	if ok {
		t.Fatalf("commission/risk %.4f should fail the 5%% cap", ratio)
	}
}
