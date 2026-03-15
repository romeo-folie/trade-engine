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
	signalsGenerated atomic.Int64
	tradesExecuted   atomic.Int64

	// Rolling daily/weekly totals (tokens), reset by the guardrail on day/week boundary.
	dailyTokens  atomic.Int64
	weeklyTokens atomic.Int64

	// Rolling weekly signal/trade counters — reset with weeklyTokens so that
	// TokensPerSignal/TokensPerTrade remain meaningful after the first week boundary.
	weeklySignals atomic.Int64
	weeklyTrades  atomic.Int64

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

	// Hold the mutex across the map lookup *and* the first increment so that
	// a concurrent TakeSnapshot can never observe a newly added ModelSpend entry
	// with zero tokens.  For existing entries the mutex is released before
	// incrementing; that is safe because the entry is already visible with its
	// accumulated (non-zero) prior values.
	t.mu.Lock()
	ms, ok := t.models[key]
	if !ok {
		ms = &ModelSpend{Provider: provider, Model: model}
		t.models[key] = ms
		ms.InputTokens.Add(int64(inputTokens))
		ms.OutputTokens.Add(int64(outputTokens))
		t.mu.Unlock()
	} else {
		t.mu.Unlock()
		ms.InputTokens.Add(int64(inputTokens))
		ms.OutputTokens.Add(int64(outputTokens))
	}

	// Cast each operand to int64 before addition to avoid int overflow on
	// 32-bit platforms or with unexpectedly large token counts.
	total := int64(inputTokens) + int64(outputTokens)
	t.dailyTokens.Add(total)
	t.weeklyTokens.Add(total)
}

// RecordSignal increments the signals-generated counter (lifetime + weekly).
func (t *Tracker) RecordSignal() {
	t.signalsGenerated.Add(1)
	t.weeklySignals.Add(1)
}

// RecordTrade increments the trades-executed counter (lifetime + weekly).
func (t *Tracker) RecordTrade() {
	t.tradesExecuted.Add(1)
	t.weeklyTrades.Add(1)
}

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
// Weekly signal/trade counters are reset alongside tokens so TokensPerSignal/TokensPerTrade
// remain meaningful after the first week boundary.
func (t *Tracker) ResetWeekly(now time.Time) {
	t.weeklyTokens.Store(0)
	t.weeklySignals.Store(0)
	t.weeklyTrades.Store(0)
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
	DailyTokens      int64
	WeeklyTokens     int64
	SignalsGenerated int64
	TradesExecuted   int64
	TokensPerSignal  float64 // average weekly tokens consumed per signal generated
	TokensPerTrade   float64 // average weekly tokens consumed per trade executed
	ModelBreakdown   []ModelBreakdownEntry
}

// ModelBreakdownEntry is one row in the per-model spend summary.
type ModelBreakdownEntry struct {
	Provider     string
	Model        string
	InputTokens  int64
	OutputTokens int64
	Total        int64
}

// TakeSnapshot returns a best-effort point-in-time telemetry snapshot.
//
// DailyTokens/WeeklyTokens and ModelBreakdown are read while t.mu is held,
// so they reflect the same logical instant relative to each other.
// The lifetime signal/trade counters (SignalsGenerated, TradesExecuted) are
// read outside the lock because they are append-only and only directionally
// associated with token spend; under high concurrency a concurrent Record
// call may complete between the counter reads and the lock acquisition,
// producing a snapshot where counters are slightly ahead of token totals.
//
// This snapshot is suitable for observability (dashboards, reports, logs).
// It is NOT suitable for billing or hard spend enforcement — for that, use
// Tracker.DailyTokens() / Tracker.WeeklyTokens() directly (atomic reads)
// as the guardrail's Check() method does.
func (t *Tracker) TakeSnapshot() Snapshot {
	// Lifetime counters — read outside lock (acceptable slight lag; see above).
	signals := t.signalsGenerated.Load()
	trades := t.tradesExecuted.Load()

	// Hold t.mu for the remainder so that token totals and model breakdown
	// reflect the same instant (no concurrent Record can interleave).
	t.mu.Lock()
	daily := t.dailyTokens.Load()
	weekly := t.weeklyTokens.Load()

	// Use rolling weekly signal/trade counters (not lifetime totals) so
	// cost-per-unit stays meaningful after week boundaries.
	weeklySignals := t.weeklySignals.Load()
	weeklyTrades := t.weeklyTrades.Load()

	breakdown := make([]ModelBreakdownEntry, 0, len(t.models))
	for _, ms := range t.models {
		// Load atomics once into locals so Total is always equal to
		// InputTokens+OutputTokens in the returned entry, even under
		// concurrent Record calls between the two atomic reads.
		inp := ms.InputTokens.Load()
		out := ms.OutputTokens.Load()
		breakdown = append(breakdown, ModelBreakdownEntry{
			Provider:     ms.Provider,
			Model:        ms.Model,
			InputTokens:  inp,
			OutputTokens: out,
			Total:        inp + out,
		})
	}
	t.mu.Unlock()

	var costPerSignal, costPerTrade float64
	if weeklySignals > 0 {
		costPerSignal = float64(weekly) / float64(weeklySignals)
	}
	if weeklyTrades > 0 {
		costPerTrade = float64(weekly) / float64(weeklyTrades)
	}

	return Snapshot{
		DailyTokens:      daily,
		WeeklyTokens:     weekly,
		SignalsGenerated: signals,
		TradesExecuted:   trades,
		TokensPerSignal:  costPerSignal,
		TokensPerTrade:   costPerTrade,
		ModelBreakdown:   breakdown,
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
