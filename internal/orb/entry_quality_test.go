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
}
