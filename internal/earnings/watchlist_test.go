package earnings

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/engine"
)

func TestNextSessionReleasesUsesFinnhubCalendarDateWithoutTimezoneShift(t *testing.T) {
	for _, date := range []string{"2026-01-06", "2026-08-25"} {
		releaseDate, err := time.Parse("2006-01-02", date)
		if err != nil {
			t.Fatal(err)
		}
		target, ok := engine.MarketSessionForDate(time.Date(releaseDate.Year(), releaseDate.Month(), releaseDate.Day(), 12, 0, 0, 0, time.FixedZone("ET", -5*60*60)))
		if !ok {
			t.Fatalf("target %s is not a session", date)
		}
		got := nextSessionReleases([]data.EarningsRelease{
			{Symbol: " nvda ", Date: releaseDate, Hour: "amc"},
			{Symbol: "NVDA", Date: releaseDate, Hour: "bmo"},
			{Symbol: "OTHER", Date: releaseDate.AddDate(0, 0, 1)},
		}, target.Date)
		if len(got) != 1 || got[0].Symbol != "NVDA" || got[0].Hour != "amc" {
			t.Fatalf("%s: releases = %+v", date, got)
		}
	}
}

func TestRankReleasesUsesMedianDollarVolumeAndDeterministicTie(t *testing.T) {
	releases := []data.EarningsRelease{{Symbol: "A"}, {Symbol: "B"}, {Symbol: "C"}, {Symbol: "D"}}
	now := time.Now()
	ranked := rankReleases(releases, map[string][]data.Candle{
		"A": {{Timestamp: now.Add(-time.Hour), Close: 10, Volume: 1000}, {Timestamp: now, Close: 5, Volume: 1000}, {Timestamp: now.Add(time.Hour), Close: 5, Volume: 1000}},
		"B": {{Timestamp: now, Close: 10, Volume: 1000}, {Timestamp: now.Add(time.Hour), Close: 10, Volume: 1000}},
		"D": {{Timestamp: now, Close: 5, Volume: 1000}},
	})
	got := []string{ranked[0].release.Symbol, ranked[1].release.Symbol, ranked[2].release.Symbol}
	if !slices.Equal(got, []string{"B", "A", "D"}) {
		t.Fatalf("rank = %v", got)
	}
}

type schedulerJob func(context.Context, engine.USMarketSession, time.Time) error

func (f schedulerJob) RunSession(ctx context.Context, s engine.USMarketSession, at time.Time) error {
	return f(ctx, s, at)
}

func TestWatchlistSchedulerRetriesTransientFailure(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-08-21T20:05:00Z")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	s := WatchlistScheduler{Cfg: config.EarningsWatchlistConfig{Enabled: true, AfterCloseDelayMinutes: 5}, Now: func() time.Time { return now }, After: func(wait time.Duration) <-chan time.Time {
		now = now.Add(wait)
		ch := make(chan time.Time, 1)
		ch <- now
		return ch
	}, Job: schedulerJob(func(context.Context, engine.USMarketSession, time.Time) error {
		calls++
		if calls == 1 {
			return errors.New("temporary")
		}
		cancel()
		return nil
	})}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want retry", calls)
	}
}
