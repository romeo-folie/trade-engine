package cost_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/cost"
)

type stubNotifier struct {
	count atomic.Int32
	last  string
	mu    sync.Mutex
}

func (s *stubNotifier) SendImmediate(_ context.Context, msg string) error {
	s.count.Add(1)
	s.mu.Lock()
	s.last = msg
	s.mu.Unlock()
	return nil
}

func testCostCfg(daily, weekly int, autodegrade bool) *config.CostConfig {
	return &config.CostConfig{
		Profile:                "pro",
		DailyTokenBudget:       daily,
		WeeklyTokenBudget:      weekly,
		AutoDegradeOnBudgetHit: autodegrade,
	}
}

func TestGuardrailDailyCapTriggersDegrade(t *testing.T) {
	tr := cost.NewTracker()
	notifier := &stubNotifier{}
	g := cost.NewGuardrail(testCostCfg(100, 0, true), tr, notifier)

	tr.Record("openai", "gpt-4o", 60, 50) // 110 tokens > 100 cap
	g.Check(context.Background(), time.Now())

	if g.ActiveProfile() != "balanced" {
		t.Errorf("expected profile=balanced after degrade, got %q", g.ActiveProfile())
	}
	if notifier.count.Load() != 1 {
		t.Errorf("expected 1 notification, got %d", notifier.count.Load())
	}
}

func TestGuardrailWeeklyCapTriggersDegrade(t *testing.T) {
	tr := cost.NewTracker()
	g := cost.NewGuardrail(testCostCfg(0, 200, true), tr, nil)

	tr.Record("openai", "gpt-4o", 150, 100) // 250 > 200 weekly
	g.Check(context.Background(), time.Now())

	if g.ActiveProfile() != "balanced" {
		t.Errorf("expected degrade to balanced, got %q", g.ActiveProfile())
	}
}

func TestGuardrailStepsDownOneAtATime(t *testing.T) {
	tr := cost.NewTracker()
	g := cost.NewGuardrail(testCostCfg(100, 0, true), tr, nil)

	// First breach: pro → balanced
	tr.Record("openai", "gpt-4o", 60, 50)
	g.Check(context.Background(), time.Now())
	if g.ActiveProfile() != "balanced" {
		t.Fatalf("first degrade: want balanced, got %q", g.ActiveProfile())
	}

	// Second breach (daily still exceeded after degrade — no reset in test).
	g.Check(context.Background(), time.Now())
	if g.ActiveProfile() != "budget" {
		t.Errorf("second degrade: want budget, got %q", g.ActiveProfile())
	}

	// Third breach — already at minimum, no further degrade.
	g.Check(context.Background(), time.Now())
	if g.ActiveProfile() != "budget" {
		t.Errorf("at minimum: should stay budget, got %q", g.ActiveProfile())
	}
}

func TestGuardrailNoDegradeWhenDisabled(t *testing.T) {
	tr := cost.NewTracker()
	g := cost.NewGuardrail(testCostCfg(100, 0, false), tr, nil)

	tr.Record("openai", "gpt-4o", 60, 50)
	g.Check(context.Background(), time.Now())

	if g.ActiveProfile() != "pro" {
		t.Errorf("auto-degrade disabled: profile should stay pro, got %q", g.ActiveProfile())
	}
}

func TestGuardrailNoDegradeWhenUnderBudget(t *testing.T) {
	tr := cost.NewTracker()
	g := cost.NewGuardrail(testCostCfg(1000, 0, true), tr, nil)

	tr.Record("openai", "gpt-4o", 10, 5)
	g.Check(context.Background(), time.Now())

	if g.ActiveProfile() != "pro" {
		t.Errorf("under budget: should not degrade, got %q", g.ActiveProfile())
	}
}

func TestGuardrailTelegramBypassesOnDegrade(t *testing.T) {
	tr := cost.NewTracker()
	notifier := &stubNotifier{}
	g := cost.NewGuardrail(testCostCfg(100, 0, true), tr, notifier)

	tr.Record("openai", "gpt-4o", 60, 50)
	g.Check(context.Background(), time.Now())

	if notifier.count.Load() == 0 {
		t.Error("degrade notification should bypass batcher and be sent immediately")
	}
}

func TestNextProfileOrder(t *testing.T) {
	cases := []struct{ in, want string }{
		{"pro", "balanced"},
		{"balanced", "budget"},
		{"budget", "budget"}, // already at minimum
	}
	for _, c := range cases {
		tr := cost.NewTracker()
		g := cost.NewGuardrail(&config.CostConfig{Profile: c.in, DailyTokenBudget: 1, AutoDegradeOnBudgetHit: true}, tr, nil)
		tr.Record("x", "y", 1, 1)
		g.Check(context.Background(), time.Now())
		if g.ActiveProfile() != c.want {
			t.Errorf("from %q: want %q, got %q", c.in, c.want, g.ActiveProfile())
		}
	}
}
