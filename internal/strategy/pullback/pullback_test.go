package pullback

import (
	"math"
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
	// An eight-week rotating base: it can make higher lows, but it must stay in
	// an overlapping price band rather than steadily advancing every day.
	for i := 0; i < 40; i++ {
		progress := float64(i)
		closePrice := 108 + progress*0.06 + math.Sin(progress*math.Pi/4)*(1.1-progress*0.01)
		if i >= 35 {
			closePrice = 109.5 + float64(i-35)*0.20
		}
		width := 2.5 - progress*0.025
		out = append(out, data.Candle{
			Timeframe: data.Timeframe1Day,
			Timestamp: start.AddDate(0, 0, 100+i),
			Open:      closePrice - 0.15,
			High:      closePrice + width,
			Low:       closePrice - width,
			Close:     closePrice,
			Volume:    1_000_000,
		})
	}
	// The static base calculation must be available before the signal session.
	// Use a provisional session to discover the pre-existing support, then make
	// the final session flush and reclaim that support.
	out = append(out, data.Candle{
		Timeframe: data.Timeframe1Day,
		Timestamp: start.AddDate(0, 0, 140),
		Open:      108.5,
		High:      110.0,
		Low:       108.0,
		Close:     109.0,
		Volume:    1_000_000,
	})
	setup := Evaluate(out, out[len(out)-1].Close, Config{})
	if setup.Support <= 0 {
		panic("valid setup fixture did not form static support: " + setup.Reason)
	}
	signal := &out[len(out)-1]
	signal.Open = setup.Support * 1.002
	signal.Low = setup.Support * 0.998
	signal.Close = setup.Support * 1.007
	signal.High = setup.Support * 1.012
	return out
}

func monotonicAdvanceCandles() []data.Candle {
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	out := make([]data.Candle, 0, 141)
	for i := 0; i < 100; i++ {
		closePrice := 50 + float64(i)*0.58
		out = append(out, data.Candle{Timeframe: data.Timeframe1Day, Timestamp: start.AddDate(0, 0, i), Open: closePrice - .2, High: closePrice + 1.5, Low: closePrice - 1.5, Close: closePrice, Volume: 1_000_000})
	}
	for i := 0; i < 40; i++ {
		closePrice := 107.5 + float64(i)*.30
		width := 2.4 - float64(i)*.035
		out = append(out, data.Candle{Timeframe: data.Timeframe1Day, Timestamp: start.AddDate(0, 0, 100+i), Open: closePrice - .15, High: closePrice + width, Low: closePrice - width, Close: closePrice, Volume: 1_000_000})
	}
	out = append(out, data.Candle{Timeframe: data.Timeframe1Day, Timestamp: start.AddDate(0, 0, 140), Open: 119, High: 120.5, Low: 117, Close: 120, Volume: 1_000_000})
	return out
}

func TestEvaluateCompleteSetup(t *testing.T) {
	candles := validSetupCandles()
	got := Evaluate(candles, candles[len(candles)-1].Close, Config{})
	if !got.Eligible {
		t.Fatalf("valid setup rejected: %s", got.Reason)
	}
	if got.StopLoss != candles[len(candles)-1].Low {
		t.Fatalf("stop = %.2f, want session low %.2f", got.StopLoss, candles[len(candles)-1].Low)
	}
	if got.AVWAPHigh <= 0 || got.AVWAPLow <= 0 || got.EMA9 <= 0 || got.EMA21 <= 0 {
		t.Fatalf("indicators not populated: %+v", got)
	}
}

func TestPromptContextPreservesDeterministicSetupFacts(t *testing.T) {
	candles := validSetupCandles()
	setup := Evaluate(candles, candles[len(candles)-1].Close, Config{})
	context := setup.PromptContext()
	if !context.Qualified || context.FailedCheck != "" {
		t.Fatalf("qualified context = %+v", context)
	}
	if context.SignalSession.IsZero() || context.AVWAPHighAnchor.IsZero() || context.AVWAPLowAnchor.IsZero() {
		t.Fatalf("AVWAP signal/anchor times missing: %+v", context)
	}
	if context.AVWAPDefinition == "" || context.StackedAVWAP <= 0 || context.StackedCompanion <= 0 || context.Support <= 0 {
		t.Fatalf("support context missing: %+v", context)
	}
	if !context.ConsolidationHigherLow || !context.ConsolidationRangeNarrower || !context.FlushedIntoSupport || !context.PositiveReclaim {
		t.Fatalf("qualification checks missing: %+v", context)
	}
	if !context.EMAAligned || !context.ConsolidationOverlapping ||
		context.ConsolidationWidthPct <= 0 || context.EMA21HoldPct <= 0 {
		t.Fatalf("tight-base evidence missing: %+v", context)
	}
	if context.StopLoss != candles[len(candles)-1].Low || context.MaxStopWidthPct != 4 || context.ExecutionInvalidation != "" {
		t.Fatalf("stop context = %+v", context)
	}
}

func TestEvaluateRejectsMonotonicAdvanceAsBaseDrift(t *testing.T) {
	got := Evaluate(monotonicAdvanceCandles(), 120, Config{})
	if got.Eligible || got.FailedCheck != "base_drift" {
		t.Fatalf("monotonic advance was accepted as a base: %+v", got)
	}
}

func TestEvaluateRejectsLateExtendedEntryAfterReclaim(t *testing.T) {
	candles := validSetupCandles()
	setup := Evaluate(candles, candles[len(candles)-1].Close, Config{})
	if !setup.Eligible {
		t.Fatalf("baseline setup rejected: %+v", setup)
	}
	lateMark := setup.Support * 1.02
	got := Evaluate(candles, lateMark, Config{})
	if got.Eligible || got.FailedCheck != "reclaim" {
		t.Fatalf("late extended entry was accepted: %+v", got)
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

func TestValidateExecutionMark(t *testing.T) {
	if reason := ValidateExecutionMark(100, 98, 4); reason != "" {
		t.Fatalf("valid mark rejected: %s", reason)
	}
	if reason := ValidateExecutionMark(98, 98, 4); !strings.Contains(reason, "invalidated") {
		t.Fatalf("stop breach accepted: %q", reason)
	}
	if reason := ValidateExecutionMark(110, 100, 4); !strings.Contains(reason, "widened") {
		t.Fatalf("wide delayed entry accepted: %q", reason)
	}
	if reason := ValidateExecutionMark(0, 98, 4); !strings.Contains(reason, "unavailable") {
		t.Fatalf("missing mark accepted: %q", reason)
	}
}

func TestValidateExecutionSetupRequiresFreshQualifiedSetupAndUnchangedStop(t *testing.T) {
	setup := Result{Eligible: true, StopLoss: 98}
	if reason := ValidateExecutionSetup(setup, 98, 98, 100, 4); reason != "" {
		t.Fatalf("valid refreshed setup rejected: %s", reason)
	}
	if reason := ValidateExecutionSetup(Result{Reason: "support lost"}, 98, 98, 100, 4); !strings.Contains(reason, "support lost") {
		t.Fatalf("failed refreshed setup accepted: %q", reason)
	}
	if reason := ValidateExecutionSetup(Result{Eligible: true, StopLoss: 97.5}, 98, 98, 100, 4); !strings.Contains(reason, "low changed") {
		t.Fatalf("changed stop accepted: %q", reason)
	}
}

func TestValidateExecutionSetupAllowsTighterPyramidPolicyStop(t *testing.T) {
	setup := Result{Eligible: true, StopLoss: 98}
	if reason := ValidateExecutionSetup(setup, 98, 99, 100, 4); reason != "" {
		t.Fatalf("tighter pyramid policy stop rejected: %s", reason)
	}
}

func TestSelectStackEvaluatesEveryAnchorSupportPair(t *testing.T) {
	// The first AVWAP is closer to EMA9 but outside tolerance. The second is a
	// valid horizontal stack and must not be discarded by preselection.
	avwap, support, ok := selectStack(
		[]float64{101.6, 95.4},
		[]float64{100, 95},
		0.01,
	)
	if !ok || avwap != 95.4 || support != 95 {
		t.Fatalf("selected (%v, %v, %v), want valid horizontal pair", avwap, support, ok)
	}
}
