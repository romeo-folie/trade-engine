package scanner

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/engine"
)

type weeklyLeaderJobFunc func(context.Context, engine.USMarketSession, time.Time) error

func (f weeklyLeaderJobFunc) RunSession(
	ctx context.Context, session engine.USMarketSession, decisionAt time.Time,
) error {
	return f(ctx, session, decisionAt)
}

func TestWeeklyLeaderSchedulerDefaultOffDoesNoWork(t *testing.T) {
	called := false
	scheduler := WeeklyLeaderScheduler{
		Cfg: config.WeeklyLeaderConfig{},
		Job: weeklyLeaderJobFunc(func(
			context.Context, engine.USMarketSession, time.Time,
		) error {
			called = true
			return nil
		}),
	}
	if err := scheduler.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("default-off scheduler ran a job")
	}
}

func TestWeeklyLeaderSchedulerCatchesUpLatestCompletedSession(t *testing.T) {
	now, err := time.Parse(
		time.RFC3339, "2024-03-17T14:00:00Z", // Sunday 10:00 ET
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var gotSession engine.USMarketSession
	var gotDecision time.Time
	scheduler := WeeklyLeaderScheduler{
		Cfg: config.WeeklyLeaderConfig{
			Enabled: true, AfterCloseDelayMinutes: 15,
		},
		Now: func() time.Time { return now },
		Job: weeklyLeaderJobFunc(func(
			_ context.Context, session engine.USMarketSession, decisionAt time.Time,
		) error {
			gotSession, gotDecision = session, decisionAt
			cancel()
			return nil
		}),
	}
	if err := scheduler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := gotSession.Date.Format("2006-01-02"); got != "2024-03-15" {
		t.Fatalf("caught-up session = %s, want 2024-03-15", got)
	}
	if !gotDecision.Equal(gotSession.Close.Add(15 * time.Minute)) {
		t.Fatalf("decision = %v, want close+15m", gotDecision)
	}
}

func TestWeeklyLeaderRetryCannotStarveANewerSession(t *testing.T) {
	fridayNow, _ := time.Parse(time.RFC3339, "2024-03-15T20:20:00Z")
	failed := engine.MostRecentCompletedMarketSession(fridayNow, 15*time.Minute)
	mondayAfterClose, _ := time.Parse(time.RFC3339, "2024-03-18T20:20:00Z")
	next, decisionAt, waitUntil := nextWeeklyLeaderAttempt(
		mondayAfterClose, failed, 15*time.Minute, 1,
	)
	if got := next.Date.Format("2006-01-02"); got != "2024-03-18" {
		t.Fatalf("retry remained stuck on %s", got)
	}
	if !decisionAt.Equal(next.Close.Add(15*time.Minute)) ||
		!waitUntil.Equal(mondayAfterClose) {
		t.Fatalf("new-session retry timing = decision %v wait %v", decisionAt, waitUntil)
	}
}

func TestWeeklyLeaderFailureRetriesAreBoundedAndExponential(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2024-03-15T20:20:00Z")
	session := engine.MostRecentCompletedMarketSession(now, 15*time.Minute)
	wantDelays := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute}
	for i, wantDelay := range wantDelays {
		gotSession, gotDecision, gotWait := nextWeeklyLeaderAttempt(
			now, session, 15*time.Minute, i+1,
		)
		if !gotSession.Date.Equal(session.Date) ||
			!gotDecision.Equal(session.Close.Add(15*time.Minute)) ||
			!gotWait.Equal(now.Add(wantDelay)) {
			t.Fatalf(
				"failure %d retry = session %v decision %v wait %v, want same session/%v/%v",
				i+1, gotSession.Date, gotDecision, gotWait,
				session.Close.Add(15*time.Minute), now.Add(wantDelay),
			)
		}
	}

	nextSession, nextRun := engine.NextMarketCloseRun(now, 15*time.Minute)
	gotSession, gotDecision, gotWait := nextWeeklyLeaderAttempt(
		now, session, 15*time.Minute, weeklyLeaderFailureRetryLimit+1,
	)
	if !gotSession.Date.Equal(nextSession.Date) ||
		!gotDecision.Equal(nextRun) || !gotWait.Equal(nextRun) {
		t.Fatalf(
			"exhausted retry = session %v decision %v wait %v, want %v/%v/%v",
			gotSession.Date, gotDecision, gotWait,
			nextSession.Date, nextRun, nextRun,
		)
	}
}

func TestWeeklyLeaderSchedulerDefersPersistentWeekendFailure(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2024-03-15T20:20:00Z")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int
	var waits []time.Duration
	scheduler := WeeklyLeaderScheduler{
		Cfg: config.WeeklyLeaderConfig{
			Enabled: true, AfterCloseDelayMinutes: 15,
		},
		Now: func() time.Time { return now },
		After: func(wait time.Duration) <-chan time.Time {
			waits = append(waits, wait)
			if len(waits) == weeklyLeaderFailureRetryLimit+1 {
				cancel()
				return make(chan time.Time)
			}
			now = now.Add(wait)
			ch := make(chan time.Time, 1)
			ch <- now
			return ch
		},
		Job: weeklyLeaderJobFunc(func(
			context.Context, engine.USMarketSession, time.Time,
		) error {
			calls++
			return errors.New("target session unavailable")
		}),
	}
	if err := scheduler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != weeklyLeaderFailureRetryLimit+1 {
		t.Fatalf("persistent failure calls = %d, want %d",
			calls, weeklyLeaderFailureRetryLimit+1)
	}
	if len(waits) != weeklyLeaderFailureRetryLimit+1 {
		t.Fatalf("persistent failure waits = %v", waits)
	}
	if want := []time.Duration{
		5 * time.Minute, 10 * time.Minute, 20 * time.Minute,
		71*time.Hour + 20*time.Minute,
	}; !slices.Equal(waits, want) {
		t.Fatalf("persistent failure waits = %v, want %v", waits, want)
	}
}

func TestWeeklyLeaderLeaseRetryKeepsSessionAndDecisionBoundary(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2024-03-15T20:20:00Z")
	session := engine.MostRecentCompletedMarketSession(now, 15*time.Minute)
	decisionAt := session.Close.Add(15 * time.Minute)
	retryAt := now.Add(45 * time.Minute)
	gotSession, gotDecision, gotWait := nextWeeklyLeaderLeaseAttempt(
		now, session, decisionAt, 15*time.Minute, retryAt,
	)
	if !gotSession.Date.Equal(session.Date) ||
		!gotDecision.Equal(decisionAt) || !gotWait.Equal(retryAt) {
		t.Fatalf("lease retry changed boundary: session=%v decision=%v wait=%v",
			gotSession.Date, gotDecision, gotWait)
	}
}

func TestWeeklyLeaderLeaseRetryRollsToNewerCompletedSession(t *testing.T) {
	fridayNow, _ := time.Parse(time.RFC3339, "2024-03-15T20:20:00Z")
	friday := engine.MostRecentCompletedMarketSession(fridayNow, 15*time.Minute)
	mondayNow, _ := time.Parse(time.RFC3339, "2024-03-18T20:20:00Z")
	next, decisionAt, waitUntil := nextWeeklyLeaderLeaseAttempt(
		mondayNow, friday, friday.Close.Add(15*time.Minute), 15*time.Minute,
		mondayNow.Add(time.Hour),
	)
	if got := next.Date.Format("2006-01-02"); got != "2024-03-18" ||
		!decisionAt.Equal(next.Close.Add(15*time.Minute)) ||
		!waitUntil.Equal(mondayNow) {
		t.Fatalf("lease rollover = %s/%v/%v", got, decisionAt, waitUntil)
	}
}

func TestWeeklyLeaderSchedulerRetriesActiveLeaseAtBoundary(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2024-03-15T20:20:00Z")
	retryAt := now.Add(45 * time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int
	var firstSession, secondSession engine.USMarketSession
	var firstDecision, secondDecision time.Time
	var waits []time.Duration
	scheduler := WeeklyLeaderScheduler{
		Cfg: config.WeeklyLeaderConfig{
			Enabled: true, AfterCloseDelayMinutes: 15,
		},
		Now: func() time.Time { return now },
		After: func(wait time.Duration) <-chan time.Time {
			waits = append(waits, wait)
			now = now.Add(wait)
			ch := make(chan time.Time, 1)
			ch <- now
			return ch
		},
		Job: weeklyLeaderJobFunc(func(
			_ context.Context, session engine.USMarketSession, decisionAt time.Time,
		) error {
			calls++
			if calls == 1 {
				firstSession, firstDecision = session, decisionAt
				return &weeklyLeaderRunLeaseActiveError{
					sessionDate: session.Date, retryAt: retryAt,
				}
			}
			secondSession, secondDecision = session, decisionAt
			cancel()
			return nil
		}),
	}
	if err := scheduler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(waits) != 1 || waits[0] != 45*time.Minute ||
		!firstSession.Date.Equal(secondSession.Date) ||
		!firstDecision.Equal(secondDecision) {
		t.Fatalf(
			"active lease retry = calls %d waits %v sessions %v/%v decisions %v/%v",
			calls, waits, firstSession.Date, secondSession.Date,
			firstDecision, secondDecision,
		)
	}
}
