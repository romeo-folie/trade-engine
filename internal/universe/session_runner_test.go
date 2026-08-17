package universe

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/engine"
)

func TestSessionRunnerAuthorizesConfiguredDelayAcrossMidnight(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 8, 2, 0, 0, 0, location)
	session := engine.MostRecentCompletedMarketSession(now, 10*time.Hour)
	if session.Date.Format("2006-01-02") != "2026-08-07" {
		t.Fatalf("selected session = %s", session.Date.Format("2006-01-02"))
	}
	runner := SessionRunner{Config: config.UniverseConfig{AfterCloseDelayMinutes: 600}}
	if !runner.sessionAuthorized(now, session, true) {
		t.Fatal("configured cross-midnight session was rejected")
	}
}

func TestSessionRunnerManualModeRejectsPriorDate(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 8, 2, 0, 0, 0, location)
	session := engine.MostRecentCompletedMarketSession(now, 10*time.Hour)
	runner := SessionRunner{
		Config: config.UniverseConfig{AfterCloseDelayMinutes: 600},
	}
	if runner.sessionAuthorized(now, session, false) {
		t.Fatal("manual mode authorized a prior-date live-data build")
	}
}

func TestSessionRunnerRejectsSessionOutsideConfiguredDelay(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 11, 2, 0, 0, 0, location)
	stale, ok := engine.MarketSessionForDate(
		time.Date(2026, 8, 7, 12, 0, 0, 0, location),
	)
	if !ok {
		t.Fatal("expected Friday market session")
	}
	runner := SessionRunner{Config: config.UniverseConfig{AfterCloseDelayMinutes: 600}}
	if runner.sessionAuthorized(now, stale, true) {
		t.Fatal("scheduler authority accepted a session outside its configured delay")
	}
}

func TestSessionRunnerRejectsOlderSessionAfterNewerClose(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		now   time.Time
		delay time.Duration
	}{
		{
			name: "ordinary close before delay",
			now:  time.Date(2026, 8, 10, 16, 15, 0, 0, location), delay: 30 * time.Minute,
		},
		{
			name: "cross-midnight delay before due time",
			now:  time.Date(2026, 8, 11, 1, 59, 0, 0, location), delay: 10 * time.Hour,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := engine.MostRecentCompletedMarketSession(tt.now, tt.delay)
			runner := SessionRunner{Config: config.UniverseConfig{
				AfterCloseDelayMinutes: int(tt.delay / time.Minute),
			}}
			if runner.sessionAuthorized(tt.now, session, true) {
				t.Fatalf("authorized stale session %s at %s",
					session.Date.Format("2006-01-02"), tt.now)
			}
		})
	}
}

func TestSessionRunnerAuthorizesWeekdayAfterCrossMidnightDelay(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 11, 2, 0, 0, 0, location)
	session := engine.MostRecentCompletedMarketSession(now, 10*time.Hour)
	runner := SessionRunner{Config: config.UniverseConfig{AfterCloseDelayMinutes: 600}}
	if !runner.sessionAuthorized(now, session, true) {
		t.Fatalf("rejected due session %s at %s", session.Date.Format("2006-01-02"), now)
	}
}

func TestSessionRunnerScheduledModeHonorsSameDayDelay(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	runner := SessionRunner{Config: config.UniverseConfig{AfterCloseDelayMinutes: 30}}
	for _, tt := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{
			name: "before configured delay",
			now:  time.Date(2026, 8, 10, 16, 1, 0, 0, location),
		},
		{
			name: "at configured delay",
			now:  time.Date(2026, 8, 10, 16, 30, 0, 0, location),
			want: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			session, ok := engine.MarketSessionForDate(tt.now)
			if !ok {
				t.Fatal("expected Monday market session")
			}
			if got := runner.sessionAuthorized(tt.now, session, true); got != tt.want {
				t.Fatalf("scheduled authorization = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSessionRunnerWaitsForConsolidatedFeedDelayInManualMode(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	date := time.Date(2026, 8, 10, 12, 0, 0, 0, location)
	session, ok := engine.MarketSessionForDate(date)
	if !ok {
		t.Fatal("expected Monday market session")
	}
	runner := SessionRunner{Config: config.UniverseConfig{
		SnapshotFeed: "delayed_sip", HistoricalFeed: "sip",
	}}
	if runner.sessionAuthorized(session.Close.Add(15*time.Minute), session, false) {
		t.Fatal("manual refresh authorized before consolidated evidence was complete")
	}
	if !runner.sessionAuthorized(
		session.Close.Add(requiredUniverseFeedDelay(runner.Config)), session, false,
	) {
		t.Fatal("manual refresh rejected after consolidated evidence was complete")
	}
}
