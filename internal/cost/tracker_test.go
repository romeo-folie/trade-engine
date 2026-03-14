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
	// weekly=1500, weeklySignals=2 → 750.0
	if s.TokensPerSignal != 750.0 {
		t.Errorf("cost_per_signal: want 750, got %.2f", s.TokensPerSignal)
	}
}

func TestTrackerCostPerSignalAfterWeeklyReset(t *testing.T) {
	tr := cost.NewTracker()
	// Week 1 activity.
	tr.Record("openai", "gpt-4o", 1000, 500)
	tr.RecordSignal()
	tr.RecordSignal()

	// Simulate week boundary.
	tr.ResetWeekly(tr.WeekStart().AddDate(0, 0, 7))

	// Week 2: new tokens but no new signals yet.
	tr.Record("openai", "gpt-4o", 200, 100)

	s := tr.TakeSnapshot()
	// weeklySignals reset to 0 → CostPerSignal must be 0, not near-zero drift.
	if s.TokensPerSignal != 0 {
		t.Errorf("cost_per_signal after reset: want 0 (no weekly signals), got %.2f", s.TokensPerSignal)
	}
	// Lifetime signals still visible.
	if s.SignalsGenerated != 2 {
		t.Errorf("lifetime signals: want 2, got %d", s.SignalsGenerated)
	}
}

func TestTrackerCostPerTradeZeroWhenNoTrades(t *testing.T) {
	tr := cost.NewTracker()
	tr.Record("openai", "gpt-4o", 500, 250)
	s := tr.TakeSnapshot()
	if s.TokensPerTrade != 0 {
		t.Errorf("cost_per_trade should be 0 when no trades, got %.2f", s.TokensPerTrade)
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

// TestTrackerRecordInt64Overflow verifies that token addition does not overflow
// when inputTokens+outputTokens would overflow a 32-bit int.  On 64-bit hosts
// with int=64 bits this would not overflow anyway, but the cast is now explicit
// to be safe on 32-bit platforms.
func TestTrackerRecordInt64Overflow(t *testing.T) {
	tr := cost.NewTracker()
	// Use values that would overflow int32 (> 2^31-1 combined).
	const big = 1<<30 + 1 // 1_073_741_825 each; sum = 2_147_483_650 > MaxInt32
	tr.Record("openai", "gpt-4o", big, big)
	want := int64(big) * 2
	if got := tr.DailyTokens(); got != want {
		t.Errorf("daily tokens: want %d, got %d (possible int32 overflow)", want, got)
	}
}

// TestTrackerNewModelEntryNotVisibleAtZero verifies that a concurrent
// TakeSnapshot cannot observe a brand-new ModelSpend entry with zero tokens.
// The fix holds t.mu through the first increment for new entries, so any
// snapshot taken while Record holds the lock either sees the full tokens or
// does not see the entry at all.
func TestTrackerNewModelEntryNotVisibleAtZero(t *testing.T) {
	const iterations = 2000
	for i := 0; i < iterations; i++ {
		tr := cost.NewTracker()
		done := make(chan struct{})
		go func() {
			defer close(done)
			tr.Record("openai", "gpt-4o", 100, 50)
		}()

		// Spin taking snapshots until the goroutine is done; if we ever see
		// the model entry with total==0 that is the race condition.
		for {
			select {
			case <-done:
				goto next
			default:
				s := tr.TakeSnapshot()
				for _, e := range s.ModelBreakdown {
					if e.Total == 0 {
						t.Fatalf("iteration %d: observed zero-total ModelBreakdownEntry — new entry visible before increment", i)
					}
				}
			}
		}
	next:
	}
}

// TestTakeSnapshotTokensConsistentWithBreakdown verifies that DailyTokens and
// WeeklyTokens in a snapshot are read while t.mu is held alongside the model
// breakdown, preventing a race where token totals and per-model entries
// reflect different instants.
func TestTakeSnapshotTokensConsistentWithBreakdown(t *testing.T) {
	const iterations = 3000
	tr := cost.NewTracker()

	stop := make(chan struct{})
	defer close(stop)

	// Continuous writer goroutine.
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				tr.Record("openai", "gpt-4o", 100, 50)
			}
		}
	}()

	// TakeSnapshot should never return a snapshot where WeeklyTokens is
	// non-zero but ModelBreakdown is empty (or vice-versa), because both are
	// now read under the same mutex.
	for i := 0; i < iterations; i++ {
		s := tr.TakeSnapshot()
		breakdownTotal := int64(0)
		for _, e := range s.ModelBreakdown {
			breakdownTotal += e.Total
		}
		// If weekly > 0 but breakdown is empty the reads were inconsistent.
		if s.WeeklyTokens > 0 && len(s.ModelBreakdown) == 0 {
			t.Fatalf("iteration %d: WeeklyTokens=%d but ModelBreakdown is empty — snapshot inconsistency", i, s.WeeklyTokens)
		}
		// WeeklyTokens must never exceed the sum of all per-model totals by
		// more than one in-flight Record (which adds to totals atomically, but
		// the breakdown entry may lag by one record).  A large discrepancy
		// indicates the reads were taken at different instants.
		if breakdownTotal > 0 && s.WeeklyTokens > breakdownTotal*2 {
			t.Fatalf("iteration %d: WeeklyTokens=%d >> breakdownTotal=%d — likely inconsistent snapshot reads", i, s.WeeklyTokens, breakdownTotal)
		}
	}
}
