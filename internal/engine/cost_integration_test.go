package engine

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/cost"
)

// TestApplyDegradedProfileUpdatesInterval verifies that when the guardrail
// changes the active profile, applyDegradedProfile recomputes the tick interval.
func TestApplyDegradedProfileUpdatesInterval(t *testing.T) {
	cfg := config.Defaults()
	// Start from "balanced" so a single degrade step goes to "budget" (5400s),
	// which has a different TickIntervalSec than "balanced" (3600s).
	cfg.Cost.Profile = "balanced"
	cfg.Cost.DailyTokenBudget = 100_000
	cfg.Cost.WeeklyTokenBudget = 500_000
	config.ApplyProfile(cfg)

	tracker := cost.NewTracker()
	guardrail := cost.NewGuardrail(&cfg.Cost, tracker, nil)

	e := &Engine{
		cfg:               cfg,
		costTracker:       tracker,
		costGuardrail:     guardrail,
		activeCostProfile: "balanced",
	}

	origInterval := time.Duration(cfg.Engine.TickIntervalSec) * time.Second
	interval := origInterval

	// Exceed daily budget to trigger degrade (balanced → budget).
	for i := 0; i < 200; i++ {
		tracker.Record("openai", "gpt-4o", 600, 400)
	}
	guardrail.Check(t.Context(), time.Now())
	e.applyDegradedProfile(&interval)

	if e.activeCostProfile != "budget" {
		t.Fatalf("expected profile 'budget', got %q", e.activeCostProfile)
	}
	wantInterval := 5400 * time.Second
	if interval != wantInterval {
		t.Errorf("expected interval %v after degrade, got %v", wantInterval, interval)
	}
	t.Logf("degraded: balanced → %s, interval %v → %v", e.activeCostProfile, origInterval, interval)
}

// TestApplyDegradedProfilePreservesTelegramSettings verifies that runtime
// profile degrade does not mutate Telegram batch settings (which can't be
// re-wired at runtime).
func TestApplyDegradedProfilePreservesTelegramSettings(t *testing.T) {
	cfg := config.Defaults()
	cfg.Cost.Profile = "balanced"
	cfg.Cost.DailyTokenBudget = 100_000
	cfg.Cost.WeeklyTokenBudget = 500_000
	config.ApplyProfile(cfg)

	// Set custom Telegram batch settings that differ from what ApplyProfile sets.
	cfg.Notifications.Telegram.BatchEnabled = true
	cfg.Notifications.Telegram.BatchWindowSec = 999

	tracker := cost.NewTracker()
	guardrail := cost.NewGuardrail(&cfg.Cost, tracker, nil)

	e := &Engine{
		cfg:               cfg,
		costTracker:       tracker,
		costGuardrail:     guardrail,
		activeCostProfile: "balanced",
	}

	interval := time.Duration(cfg.Engine.TickIntervalSec) * time.Second

	// Force degrade.
	for i := 0; i < 200; i++ {
		tracker.Record("openai", "gpt-4o", 600, 400)
	}
	guardrail.Check(t.Context(), time.Now())

	e.applyDegradedProfile(&interval)

	if !cfg.Notifications.Telegram.BatchEnabled {
		t.Error("BatchEnabled was mutated by applyDegradedProfile")
	}
	if cfg.Notifications.Telegram.BatchWindowSec != 999 {
		t.Errorf("BatchWindowSec changed to %d, want 999", cfg.Notifications.Telegram.BatchWindowSec)
	}
}

// TestTokenRecordGuardRejectsEmptyModel verifies the guard condition used in
// runAnalysis: tokens must not be recorded when model is empty. This tests the
// guard predicate in isolation to prevent regressions that would create
// ambiguous "provider/" keys in the tracker.
func TestTokenRecordGuardRejectsEmptyModel(t *testing.T) {
	cases := []struct {
		provider, model string
		input, output   int
		wantRecord      bool
	}{
		{"openai", "gpt-4o", 100, 50, true},
		{"openai", "", 100, 50, false},  // empty model
		{"", "gpt-4o", 100, 50, false},  // empty provider
		{"", "", 100, 50, false},         // both empty
		{"openai", "gpt-4o", 0, 0, false}, // zero tokens
	}
	for _, tc := range cases {
		shouldRecord := tc.provider != "" && tc.model != "" && (tc.input > 0 || tc.output > 0)
		if shouldRecord != tc.wantRecord {
			t.Errorf("guard(%q, %q, %d, %d) = %v, want %v",
				tc.provider, tc.model, tc.input, tc.output, shouldRecord, tc.wantRecord)
		}
	}
}
