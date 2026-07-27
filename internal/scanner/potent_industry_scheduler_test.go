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

type potentIndustryJobFunc func(
	context.Context, engine.USMarketSession, time.Time,
) error

func (f potentIndustryJobFunc) RunSession(
	ctx context.Context, session engine.USMarketSession, decisionAt time.Time,
) error {
	return f(ctx, session, decisionAt)
}

func TestPotentIndustrySchedulerDefaultOffDoesNoWork(t *testing.T) {
	called := false
	scheduler := PotentIndustryScheduler{
		Cfg: config.PotentIndustryConfig{},
		Job: potentIndustryJobFunc(func(
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
		t.Fatal("default-off potent industry scheduler ran a job")
	}
}

func TestPotentIndustrySchedulerCatchesUpAcrossDST(t *testing.T) {
	// Sunday after the US spring transition: the prior Friday closed at 21:00Z.
	now, _ := time.Parse(time.RFC3339, "2024-03-10T18:00:00Z")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var gotSession engine.USMarketSession
	var gotDecision time.Time
	scheduler := PotentIndustryScheduler{
		Cfg: config.PotentIndustryConfig{
			Enabled: true, AfterCloseDelayMinutes: 15,
		},
		Now: func() time.Time { return now },
		Job: potentIndustryJobFunc(func(
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
	if got := gotSession.Date.Format("2006-01-02"); got != "2024-03-08" {
		t.Fatalf("caught-up session = %s, want 2024-03-08", got)
	}
	if gotSession.Close.UTC().Hour() != 21 ||
		!gotDecision.Equal(gotSession.Close.Add(15*time.Minute)) {
		t.Fatalf("DST catch-up boundary = close %v decision %v",
			gotSession.Close, gotDecision)
	}
}

func TestPotentIndustrySchedulerUsesHolidayEarlyClose(t *testing.T) {
	// July 4 is closed; catch-up resolves July 3's 13:00 ET close.
	now, _ := time.Parse(time.RFC3339, "2024-07-04T18:00:00Z")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got engine.USMarketSession
	var decision time.Time
	scheduler := PotentIndustryScheduler{
		Cfg: config.PotentIndustryConfig{
			Enabled: true, AfterCloseDelayMinutes: 15,
		},
		Now: func() time.Time { return now },
		Job: potentIndustryJobFunc(func(
			_ context.Context, session engine.USMarketSession, decisionAt time.Time,
		) error {
			got, decision = session, decisionAt
			cancel()
			return nil
		}),
	}
	if err := scheduler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got.Date.Format("2006-01-02") != "2024-07-03" ||
		got.Close.In(got.Close.Location()).Hour() != 13 ||
		!decision.Equal(got.Close.Add(15*time.Minute)) {
		t.Fatalf("holiday/early-close catch-up = %v close %v decision %v",
			got.Date, got.Close, decision)
	}
}

func TestPotentIndustryFailureRetriesAreBoundedAndExponential(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2024-03-15T20:20:00Z")
	session := engine.MostRecentCompletedMarketSession(now, 15*time.Minute)
	wantDelays := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute}
	for i, wantDelay := range wantDelays {
		gotSession, gotDecision, gotWait := nextPotentIndustryAttempt(
			now, session, 15*time.Minute, i+1,
		)
		if !gotSession.Date.Equal(session.Date) ||
			!gotDecision.Equal(session.Close.Add(15*time.Minute)) ||
			!gotWait.Equal(now.Add(wantDelay)) {
			t.Fatalf("retry %d = %v/%v/%v, want same session/%v/%v",
				i+1, gotSession.Date, gotDecision, gotWait,
				session.Close.Add(15*time.Minute), now.Add(wantDelay))
		}
	}
	nextSession, nextRun := engine.NextMarketCloseRun(now, 15*time.Minute)
	gotSession, gotDecision, gotWait := nextPotentIndustryAttempt(
		now, session, 15*time.Minute, potentIndustryFailureRetryLimit+1,
	)
	if !gotSession.Date.Equal(nextSession.Date) ||
		!gotDecision.Equal(nextRun) || !gotWait.Equal(nextRun) {
		t.Fatalf("exhausted retry = %v/%v/%v, want %v/%v/%v",
			gotSession.Date, gotDecision, gotWait,
			nextSession.Date, nextRun, nextRun)
	}
}

func TestPotentIndustrySchedulerDefersPersistentWeekendFailure(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2024-03-15T20:20:00Z")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int
	var waits []time.Duration
	scheduler := PotentIndustryScheduler{
		Cfg: config.PotentIndustryConfig{
			Enabled: true, AfterCloseDelayMinutes: 15,
		},
		Now: func() time.Time { return now },
		After: func(wait time.Duration) <-chan time.Time {
			waits = append(waits, wait)
			if len(waits) == potentIndustryFailureRetryLimit+1 {
				cancel()
				return make(chan time.Time)
			}
			now = now.Add(wait)
			ch := make(chan time.Time, 1)
			ch <- now
			return ch
		},
		Job: potentIndustryJobFunc(func(
			context.Context, engine.USMarketSession, time.Time,
		) error {
			calls++
			return errors.New("target session unavailable")
		}),
	}
	if err := scheduler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != potentIndustryFailureRetryLimit+1 {
		t.Fatalf("calls = %d, want %d", calls, potentIndustryFailureRetryLimit+1)
	}
	want := []time.Duration{
		5 * time.Minute, 10 * time.Minute, 20 * time.Minute,
		71*time.Hour + 20*time.Minute,
	}
	if !slices.Equal(waits, want) {
		t.Fatalf("persistent failure waits = %v, want %v", waits, want)
	}
}

func TestPotentIndustryLeaseRetryPreservesOrAdvancesBoundary(t *testing.T) {
	fridayNow, _ := time.Parse(time.RFC3339, "2024-03-15T20:20:00Z")
	friday := engine.MostRecentCompletedMarketSession(fridayNow, 15*time.Minute)
	decision := friday.Close.Add(15 * time.Minute)
	retryAt := fridayNow.Add(45 * time.Minute)
	got, gotDecision, wait := nextPotentIndustryLeaseAttempt(
		fridayNow, friday, decision, 15*time.Minute, retryAt,
	)
	if !got.Date.Equal(friday.Date) || !gotDecision.Equal(decision) ||
		!wait.Equal(retryAt) {
		t.Fatalf("lease changed blocked boundary: %v/%v/%v",
			got.Date, gotDecision, wait)
	}

	mondayNow, _ := time.Parse(time.RFC3339, "2024-03-18T20:20:00Z")
	got, gotDecision, wait = nextPotentIndustryLeaseAttempt(
		mondayNow, friday, decision, 15*time.Minute, mondayNow.Add(time.Hour),
	)
	if got.Date.Format("2006-01-02") != "2024-03-18" ||
		!gotDecision.Equal(got.Close.Add(15*time.Minute)) ||
		!wait.Equal(mondayNow) {
		t.Fatalf("lease failed to advance to current session: %v/%v/%v",
			got.Date, gotDecision, wait)
	}
}

func TestPotentIndustryRetryCannotStarveNewerSession(t *testing.T) {
	fridayNow, _ := time.Parse(time.RFC3339, "2024-03-15T20:20:00Z")
	friday := engine.MostRecentCompletedMarketSession(fridayNow, 15*time.Minute)
	mondayNow, _ := time.Parse(time.RFC3339, "2024-03-18T20:20:00Z")
	next, decision, wait := nextPotentIndustryAttempt(
		mondayNow, friday, 15*time.Minute, 1,
	)
	if next.Date.Format("2006-01-02") != "2024-03-18" ||
		!decision.Equal(next.Close.Add(15*time.Minute)) ||
		!wait.Equal(mondayNow) {
		t.Fatalf("retry starved newer session: %v/%v/%v",
			next.Date, decision, wait)
	}
}
