package cost_test

import (
	"sync"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/cost"
)

func TestTrackerRecord(t *testing.T) {
	tr := cost.NewTracker()
	tr.Record("openai", "gpt-4o", 100, 50)
	tr.Record("openai", "gpt-4o", 200, 100)
	tr.Record("anthropic", "claude", 50, 25)

	if tr.DailyTokens() != 525 {
		t.Errorf("daily tokens: want 525, got %d", tr.DailyTokens())
	}
	if tr.WeeklyTokens() != 525 {
		t.Errorf("weekly tokens: want 525, got %d", tr.WeeklyTokens())
	}
}

func TestTrackerCostPerSignal(t *testing.T) {
	tr := cost.NewTracker()
	tr.Record("openai", "gpt-4o", 1000, 500)
	tr.RecordSignal()
	tr.RecordSignal()

	s := tr.TakeSnapshot()
	// weekly=1500, signals=2 → 750.0
	if s.CostPerSignal != 750.0 {
		t.Errorf("cost_per_signal: want 750, got %.2f", s.CostPerSignal)
	}
}

func TestTrackerCostPerTradeZeroWhenNoTrades(t *testing.T) {
	tr := cost.NewTracker()
	tr.Record("openai", "gpt-4o", 500, 250)
	s := tr.TakeSnapshot()
	if s.CostPerTrade != 0 {
		t.Errorf("cost_per_trade should be 0 when no trades, got %.2f", s.CostPerTrade)
	}
}

func TestTrackerModelBreakdown(t *testing.T) {
	tr := cost.NewTracker()
	tr.Record("openai", "gpt-4o", 100, 50)
	tr.Record("google", "gemini", 200, 100)

	s := tr.TakeSnapshot()
	if len(s.ModelBreakdown) != 2 {
		t.Errorf("expected 2 model breakdown entries, got %d", len(s.ModelBreakdown))
	}
	for _, e := range s.ModelBreakdown {
		if e.Total != e.InputTokens+e.OutputTokens {
			t.Errorf("total mismatch: %d != %d+%d", e.Total, e.InputTokens, e.OutputTokens)
		}
	}
}

func TestTrackerResetDaily(t *testing.T) {
	tr := cost.NewTracker()
	tr.Record("openai", "gpt-4o", 1000, 500)
	tr.ResetDaily(tr.DayStart().AddDate(0, 0, 1))
	if tr.DailyTokens() != 0 {
		t.Errorf("daily tokens should be 0 after reset, got %d", tr.DailyTokens())
	}
	// Weekly not reset.
	if tr.WeeklyTokens() == 0 {
		t.Error("weekly tokens should not be reset by ResetDaily")
	}
}

func TestTrackerConcurrentSafe(t *testing.T) {
	tr := cost.NewTracker()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.Record("openai", "gpt-4o", 10, 5)
			tr.RecordSignal()
		}()
	}
	wg.Wait()

	if tr.DailyTokens() != 1500 {
		t.Errorf("concurrent daily tokens: want 1500, got %d", tr.DailyTokens())
	}
}
