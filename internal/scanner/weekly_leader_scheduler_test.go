package scanner

import (
	"context"
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
		mondayAfterClose, failed, 15*time.Minute,
	)
	if got := next.Date.Format("2006-01-02"); got != "2024-03-18" {
		t.Fatalf("retry remained stuck on %s", got)
	}
	if !decisionAt.Equal(next.Close.Add(15*time.Minute)) ||
		!waitUntil.Equal(mondayAfterClose) {
		t.Fatalf("new-session retry timing = decision %v wait %v", decisionAt, waitUntil)
	}
}
