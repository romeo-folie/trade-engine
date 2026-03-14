package cost

import (
	"context"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
)

// DegradeNotifier is implemented by any subsystem that can send an immediate
// (batcher-bypassing) alert on profile degrade. TelegramBot satisfies this.
type DegradeNotifier interface {
	SendImmediate(ctx context.Context, message string) error
}

// ProfileOrder defines the degrade step sequence (most expensive → cheapest).
var ProfileOrder = []string{"pro", "balanced", "budget"}

// Guardrail checks token spend against configured budgets and triggers
// auto-degrade when thresholds are crossed. Safe for concurrent use.
type Guardrail struct {
	mu       sync.Mutex
	cfg      *config.CostConfig
	tracker  *Tracker
	notifier DegradeNotifier // may be nil
	profile  string          // active profile, may differ from cfg.Profile after degrade
}

// NewGuardrail creates a guardrail wired to tracker. notifier may be nil.
func NewGuardrail(cfg *config.CostConfig, tracker *Tracker, notifier DegradeNotifier) *Guardrail {
	return &Guardrail{
		cfg:     cfg,
		tracker: tracker,
		notifier: notifier,
		profile: cfg.Profile,
	}
}

// ActiveProfile returns the currently active profile (may differ from the
// configured profile if auto-degrade has fired).
func (g *Guardrail) ActiveProfile() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.profile
}

// Check evaluates current token spend against the configured budgets.
// Call once per tick. Fires auto-degrade if a threshold is crossed.
func (g *Guardrail) Check(ctx context.Context, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.rollPeriods(now)

	if !g.cfg.AutoDegradeOnBudgetHit {
		return
	}

	daily := g.tracker.DailyTokens()
	weekly := g.tracker.WeeklyTokens()

	var reason string
	switch {
	case g.cfg.DailyTokenBudget > 0 && daily >= int64(g.cfg.DailyTokenBudget):
		reason = "daily token budget exceeded"
	case g.cfg.WeeklyTokenBudget > 0 && weekly >= int64(g.cfg.WeeklyTokenBudget):
		reason = "weekly token budget exceeded"
	default:
		return
	}

	g.degrade(ctx, reason)
}

// degrade steps the active profile down one tier and notifies. Must be called
// with g.mu held.
func (g *Guardrail) degrade(ctx context.Context, reason string) {
	next := nextProfile(g.profile)
	if next == g.profile {
		logger.Warn("cost guardrail: already at minimum profile, cannot degrade further",
			"profile", g.profile, "reason", reason)
		return
	}

	prev := g.profile
	g.profile = next

	logger.Warn("cost guardrail: auto-degrade triggered",
		"from", prev, "to", next, "reason", reason)

	if g.notifier != nil {
		msg := "⚠️ *Cost guardrail*: " + reason + "\nProfile degraded " + prev + " → " + next
		if err := g.notifier.SendImmediate(ctx, msg); err != nil {
			logger.Warn("cost guardrail: failed to send degrade notification", "err", err.Error())
		}
	}
}

// rollPeriods resets counters when the day or week has rolled over.
func (g *Guardrail) rollPeriods(now time.Time) {
	if truncateDay(now).After(g.tracker.DayStart()) {
		g.tracker.ResetDaily(now)
	}
	if truncateWeek(now).After(g.tracker.WeekStart()) {
		g.tracker.ResetWeekly(now)
	}
}

// nextProfile returns the next cheaper profile, or the same profile when
// already at minimum.
func nextProfile(current string) string {
	for i, p := range ProfileOrder {
		if p == current && i+1 < len(ProfileOrder) {
			return ProfileOrder[i+1]
		}
	}
	return current // already at "budget"
}
