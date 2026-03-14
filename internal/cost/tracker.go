// Package cost implements token spend tracking, budget guardrails, and
// cost telemetry for the cost-reduction PRD §7.8.
package cost

import (
	"sync"
	"sync/atomic"
	"time"
)

// ModelSpend tracks cumulative token usage for one model provider/name pair.
type ModelSpend struct {
	Provider     string
	Model        string
	InputTokens  atomic.Int64
	OutputTokens atomic.Int64
}

// TotalTokens returns input + output tokens.
func (m *ModelSpend) TotalTokens() int64 {
	return m.InputTokens.Load() + m.OutputTokens.Load()
}

// Tracker accumulates per-model token spend and computes derived cost metrics.
// All methods are safe for concurrent use.
type Tracker struct {
	mu     sync.Mutex
	models map[string]*ModelSpend // key = "provider/model"

	// Lifecycle counters for cost-per-unit calculations.
	signalsGenerated  atomic.Int64
	tradesExecuted    atomic.Int64

	// Rolling daily/weekly totals (tokens), reset by the guardrail on day/week boundary.
	dailyTokens  atomic.Int64
	weeklyTokens atomic.Int64

	// Period boundaries — protected by periodMu (separate from models mu to
	// avoid holding the broad lock during period resets).
	periodMu  sync.RWMutex
	dayStart  time.Time
	weekStart time.Time
}

// NewTracker creates a Tracker with period boundaries anchored to now.
func NewTracker() *Tracker {
	now := time.Now().UTC()
	return &Tracker{
		models:    make(map[string]*ModelSpend),
		dayStart:  truncateDay(now),
		weekStart: truncateWeek(now),
	}
}

// Record adds a model call's token usage to the tracker.
func (t *Tracker) Record(provider, model string, inputTokens, outputTokens int) {
	key := provider + "/" + model

	t.mu.Lock()
	ms, ok := t.models[key]
	if !ok {
		ms = &ModelSpend{Provider: provider, Model: model}
		t.models[key] = ms
	}
	t.mu.Unlock()

	ms.InputTokens.Add(int64(inputTokens))
	ms.OutputTokens.Add(int64(outputTokens))

	total := int64(inputTokens + outputTokens)
	t.dailyTokens.Add(total)
	t.weeklyTokens.Add(total)
}

// RecordSignal increments the signals-generated counter.
func (t *Tracker) RecordSignal() { t.signalsGenerated.Add(1) }

// RecordTrade increments the trades-executed counter.
func (t *Tracker) RecordTrade() { t.tradesExecuted.Add(1) }

// DailyTokens returns total tokens used today.
func (t *Tracker) DailyTokens() int64 { return t.dailyTokens.Load() }

// WeeklyTokens returns total tokens used this week.
func (t *Tracker) WeeklyTokens() int64 { return t.weeklyTokens.Load() }

// ResetDaily resets the daily counter (called at day boundary by the guardrail).
func (t *Tracker) ResetDaily(now time.Time) {
	t.dailyTokens.Store(0)
	t.periodMu.Lock()
	t.dayStart = truncateDay(now)
	t.periodMu.Unlock()
}

// ResetWeekly resets the weekly counter (called at week boundary by the guardrail).
func (t *Tracker) ResetWeekly(now time.Time) {
	t.weeklyTokens.Store(0)
	t.periodMu.Lock()
	t.weekStart = truncateWeek(now)
	t.periodMu.Unlock()
}

// DayStart returns the start of the current tracking day.
func (t *Tracker) DayStart() time.Time {
	t.periodMu.RLock()
	defer t.periodMu.RUnlock()
	return t.dayStart
}

// WeekStart returns the start of the current tracking week.
func (t *Tracker) WeekStart() time.Time {
	t.periodMu.RLock()
	defer t.periodMu.RUnlock()
	return t.weekStart
}

// Snapshot returns a point-in-time telemetry snapshot.
type Snapshot struct {
	DailyTokens        int64
	WeeklyTokens       int64
	SignalsGenerated    int64
	TradesExecuted     int64
	CostPerSignal      float64 // tokens per signal (proxy for cost)
	CostPerTrade       float64 // tokens per executed trade
	ModelBreakdown     []ModelBreakdownEntry
}

// ModelBreakdownEntry is one row in the per-model spend summary.
type ModelBreakdownEntry struct {
	Provider    string
	Model       string
	InputTokens int64
	OutputTokens int64
	Total       int64
}

// TakeSnapshot returns a consistent Snapshot.
func (t *Tracker) TakeSnapshot() Snapshot {
	signals := t.signalsGenerated.Load()
	trades := t.tradesExecuted.Load()
	daily := t.dailyTokens.Load()
	weekly := t.weeklyTokens.Load()

	var costPerSignal, costPerTrade float64
	if signals > 0 {
		costPerSignal = float64(weekly) / float64(signals)
	}
	if trades > 0 {
		costPerTrade = float64(weekly) / float64(trades)
	}

	t.mu.Lock()
	breakdown := make([]ModelBreakdownEntry, 0, len(t.models))
	for _, ms := range t.models {
		breakdown = append(breakdown, ModelBreakdownEntry{
			Provider:     ms.Provider,
			Model:        ms.Model,
			InputTokens:  ms.InputTokens.Load(),
			OutputTokens: ms.OutputTokens.Load(),
			Total:        ms.TotalTokens(),
		})
	}
	t.mu.Unlock()

	return Snapshot{
		DailyTokens:     daily,
		WeeklyTokens:    weekly,
		SignalsGenerated: signals,
		TradesExecuted:  trades,
		CostPerSignal:   costPerSignal,
		CostPerTrade:    costPerTrade,
		ModelBreakdown:  breakdown,
	}
}

func truncateDay(t time.Time) time.Time {
	t = t.UTC() // normalise before extracting components
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func truncateWeek(t time.Time) time.Time {
	t = t.UTC()
	d := t.Weekday()
	if d == time.Sunday {
		d = 7
	}
	return truncateDay(t).AddDate(0, 0, -int(d-time.Monday))
}
