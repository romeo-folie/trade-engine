package cost

import (
	"context"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
)

// DegradeNotifier is implemented by any subsystem that can send an immediate
// (batcher-bypassing) alert on profile degrade.
// notify.TelegramBot satisfies this interface via its SendImmediate method.
type DegradeNotifier interface {
	SendImmediate(ctx context.Context, message string) error
}

// profileOrder defines the degrade step sequence (most expensive → cheapest).
// Unexported to prevent external mutation that would make guardrail behaviour
// unpredictable. Use nextProfile to traverse the sequence.
var profileOrder = []string{"pro", "balanced", "budget"}

// Guardrail checks token spend against configured budgets and triggers
// auto-degrade when thresholds are crossed. Safe for concurrent use.
//
// Degrade fires at most once per budget period: the exceeded flags are set on
// first threshold crossing and cleared when rollPeriods resets the counters,
// preventing repeated step-downs on every tick while the budget stays over cap.
type Guardrail struct {
	mu       sync.Mutex
	cfg      *config.CostConfig
	tracker  *Tracker
	notifier DegradeNotifier // may be nil
	profile  string          // active profile, may differ from cfg.Profile after degrade

	// transition-guard: prevents re-firing degrade on every tick while spend
	// stays above the threshold. Cleared by rollPeriods when counters reset.
	dailyBudgetExceeded  bool
	weeklyBudgetExceeded bool
}

// NewGuardrail creates a guardrail wired to tracker. notifier may be nil.
func NewGuardrail(cfg *config.CostConfig, tracker *Tracker, notifier DegradeNotifier) *Guardrail {
	return &Guardrail{
		cfg:      cfg,
		tracker:  tracker,
		notifier: notifier,
		profile:  cfg.Profile,
	}
}

// ActiveProfile returns the currently active profile (may differ from the
// configured profile if auto-degrade has fired).
func (g *Guardrail) ActiveProfile() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.profile
}

// SetNotifier replaces the DegradeNotifier used for auto-degrade alerts.
// Safe to call after construction — used to wire in TelegramBot once it starts.
func (g *Guardrail) SetNotifier(n DegradeNotifier) {
	g.mu.Lock()
	g.notifier = n
	g.mu.Unlock()
}

// Check evaluates current token spend against the configured budgets.
// Call once per tick. Fires auto-degrade if a threshold is crossed.
func (g *Guardrail) Check(ctx context.Context, now time.Time) {
	g.mu.Lock()
	g.rollPeriods(now)

	if !g.cfg.AutoDegradeOnBudgetHit {
		g.mu.Unlock()
		return
	}

	daily := g.tracker.DailyTokens()
	weekly := g.tracker.WeeklyTokens()

	// Only degrade on first threshold crossing — not on every subsequent tick
	// while spend remains above cap. Flags are cleared by rollPeriods when
	// counters reset at the next day/week boundary.
	var reason string
	switch {
	case g.cfg.DailyTokenBudget > 0 && daily >= int64(g.cfg.DailyTokenBudget) && !g.dailyBudgetExceeded:
		reason = "daily token budget exceeded"
		g.dailyBudgetExceeded = true
	case g.cfg.WeeklyTokenBudget > 0 && weekly >= int64(g.cfg.WeeklyTokenBudget) && !g.weeklyBudgetExceeded:
		reason = "weekly token budget exceeded"
		g.weeklyBudgetExceeded = true
	default:
		g.mu.Unlock()
		return
	}

	// Capture degrade event under lock, then release before I/O.
	prev, next := g.profile, nextProfile(g.profile)
	if next != prev {
		g.profile = next
	}
	notifier := g.notifier
	g.mu.Unlock()

	if next == prev {
		logger.Warn("cost guardrail: already at minimum profile, cannot degrade further",
			"profile", prev, "reason", reason)
		return
	}

	logger.Warn("cost guardrail: auto-degrade triggered",
		"from", prev, "to", next, "reason", reason)

	// Notify outside the critical section — SendImmediate may block on I/O.
	if notifier != nil {
		msg := "⚠️ *Cost guardrail*: " + reason + "\nProfile degraded " + prev + " → " + next
		if err := notifier.SendImmediate(ctx, msg); err != nil {
			logger.Warn("cost guardrail: failed to send degrade notification", "err", err.Error())
		}
	}
}

// rollPeriods resets counters when the day or week has rolled over.
// The exceeded flags are cleared alongside the counters so the next
// threshold crossing in the new period triggers a fresh degrade step.
func (g *Guardrail) rollPeriods(now time.Time) {
	if truncateDay(now).After(g.tracker.DayStart()) {
		g.tracker.ResetDaily(now)
		g.dailyBudgetExceeded = false
	}
	if truncateWeek(now).After(g.tracker.WeekStart()) {
		g.tracker.ResetWeekly(now)
		g.weeklyBudgetExceeded = false
	}
}

// nextProfile returns the next cheaper profile, or the same profile when
// already at minimum.
func nextProfile(current string) string {
	for i, p := range profileOrder {
		if p == current && i+1 < len(profileOrder) {
			return profileOrder[i+1]
		}
	}
	return current // already at "budget"
}
