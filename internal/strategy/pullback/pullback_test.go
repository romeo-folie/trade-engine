package pullback

import (
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
)

func validSetupCandles() []data.Candle {
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	out := make([]data.Candle, 0, 141)
	// Established momentum leg.
	for i := 0; i < 100; i++ {
		closePrice := 50 + float64(i)*0.58
		out = append(out, data.Candle{
			Timeframe: data.Timeframe1Day,
			Timestamp: start.AddDate(0, 0, i),
			Open:      closePrice - 0.2,
			High:      closePrice + 1.5,
			Low:       closePrice - 1.5,
			Close:     closePrice,
			Volume:    1_000_000,
		})
	}
	// Eight-week rising, higher-low consolidation whose range contracts.
	for i := 0; i < 40; i++ {
		closePrice := 107.5 + float64(i)*0.30
		width := 2.4 - float64(i)*0.035
		lowBump := 0.0
		if i >= 20 {
			lowBump = 1.5
		}
		out = append(out, data.Candle{
			Timeframe: data.Timeframe1Day,
			Timestamp: start.AddDate(0, 0, 100+i),
			Open:      closePrice - 0.15,
			High:      closePrice + width,
			Low:       closePrice - width + lowBump,
			Close:     closePrice,
			Volume:    1_000_000,
		})
	}
	// Signal session: flush through stacked support, turn positive and reclaim.
	out = append(out, data.Candle{
		Timeframe: data.Timeframe1Day,
		Timestamp: start.AddDate(0, 0, 140),
		Open:      119.0,
		High:      120.5,
		Low:       117.0,
		Close:     120.0,
		Volume:    1_000_000,
	})
	return out
}

func TestEvaluateCompleteSetup(t *testing.T) {
	got := Evaluate(validSetupCandles(), 120, Config{})
	if !got.Eligible {
		t.Fatalf("valid setup rejected: %s", got.Reason)
	}
	if got.StopLoss != 117 {
		t.Fatalf("stop = %.2f, want session low 117", got.StopLoss)
	}
	if got.AVWAPHigh <= 0 || got.AVWAPLow <= 0 || got.EMA9 <= 0 || got.EMA21 <= 0 {
		t.Fatalf("indicators not populated: %+v", got)
	}
}

func TestEvaluateRequiresStrengthAndReclaim(t *testing.T) {
	t.Run("weak momentum", func(t *testing.T) {
		candles := validSetupCandles()
		for i := range candles {
			candles[i].Open /= 2
			candles[i].High /= 2
			candles[i].Low /= 2
			candles[i].Close /= 2
		}
		// Flatten the entire history and session while retaining valid ranges.
		for i := range candles {
			candles[i].Open = 100
			candles[i].High = 101
			candles[i].Low = 99
			candles[i].Close = 100
		}
		got := Evaluate(candles, 100, Config{})
		if got.Eligible || got.FailedCheck != "momentum" || !strings.Contains(got.Reason, "momentum below") {
			t.Fatalf("weak setup result: %+v", got)
		}
	})

	t.Run("no positive reclaim", func(t *testing.T) {
		candles := validSetupCandles()
		last := &candles[len(candles)-1]
		last.Open = 120
		last.Close = 118
		got := Evaluate(candles, 118, Config{})
		if got.Eligible || got.FailedCheck != "reclaim" || !strings.Contains(got.Reason, "positive reclaim") {
			t.Fatalf("failed reclaim result: %+v", got)
		}
	})
}

func TestEvaluateFailsClosedWithoutSixMonthHistory(t *testing.T) {
	got := Evaluate(validSetupCandles()[:100], 108, Config{})
	if got.Eligible || got.FailedCheck != "history" || !strings.Contains(got.Reason, "insufficient daily history") {
		t.Fatalf("thin-history result: %+v", got)
	}
}
