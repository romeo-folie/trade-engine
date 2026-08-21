package earnings

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
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
	var logs bytes.Buffer
	restore := logger.SetForTesting(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer restore()
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
	for _, want := range []string{
		"earnings watchlist scheduler initialized",
		"earnings watchlist feeder failed; retry scheduled",
		"\"failure_count\":1",
		"earnings watchlist scheduler waiting",
		"\"reason\":\"retry\"",
		"earnings watchlist scheduler woke",
		"\"lag_seconds\":0",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("scheduler logs missing %q:\n%s", want, logs.String())
		}
	}
}

type watchlistCalendarStub struct {
	calls int
	fetch func(context.Context) ([]data.EarningsRelease, error)
}

func (f *watchlistCalendarStub) FetchMarketEarningsCalendar(ctx context.Context, _, _ time.Time) ([]data.EarningsRelease, error) {
	f.calls++
	return f.fetch(ctx)
}

type watchlistBarsStub struct {
	calls int
}

func (f *watchlistBarsStub) FetchDailyBars(context.Context, []string, time.Time, time.Time, int) (data.DailyBarsBatchResult, error) {
	f.calls++
	return data.DailyBarsBatchResult{}, nil
}

func TestWatchlistFeederSkipsClaimWhenExpiryCrossesTargetOpen(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("store unavailable; skipping DB-backed earnings watchlist test")
	}
	ctx := context.Background()
	completed, ok := engine.MarketSessionForDate(time.Date(2031, time.January, 2, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("expected completed session")
	}
	target := engine.NextMarketSession(completed.Date)
	if _, err := store.GetDB().Exec(ctx, `DELETE FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target.Date); err != nil {
		t.Fatalf("clear run fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target.Date)
	})

	calendar := &watchlistCalendarStub{fetch: func(context.Context) ([]data.EarningsRelease, error) {
		return nil, nil
	}}
	bars := &watchlistBarsStub{}
	nowCalls := 0
	feeder := WatchlistFeeder{
		Calendar: calendar,
		Bars:     bars,
		Now: func() time.Time {
			nowCalls++
			if nowCalls == 1 {
				return target.Open.Add(-time.Nanosecond)
			}
			return target.Open
		},
	}
	if err := feeder.RunSession(ctx, completed, completed.Close); err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	if calendar.calls != 0 || bars.calls != 0 {
		t.Fatalf("providers called after target opened: calendar=%d bars=%d", calendar.calls, bars.calls)
	}
	var claimed int
	if err := store.GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target.Date).Scan(&claimed); err != nil {
		t.Fatalf("query claimed run: %v", err)
	}
	if claimed != 0 {
		t.Fatalf("claimed runs=%d, want 0", claimed)
	}
}

func TestWatchlistFeederCancellationAfterClaimLeavesRunRunning(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("store unavailable; skipping DB-backed earnings watchlist test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completed, ok := engine.MarketSessionForDate(time.Date(2031, time.January, 3, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("expected completed session")
	}
	target := engine.NextMarketSession(completed.Date)
	if _, err := store.GetDB().Exec(context.Background(), `DELETE FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target.Date); err != nil {
		t.Fatalf("clear run fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target.Date)
	})
	var logs bytes.Buffer
	restore := logger.SetForTesting(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer restore()
	calendar := &watchlistCalendarStub{fetch: func(context.Context) ([]data.EarningsRelease, error) {
		cancel()
		return nil, context.Canceled
	}}
	feeder := WatchlistFeeder{
		Calendar: calendar,
		Bars:     &watchlistBarsStub{},
		Now:      func() time.Time { return target.Open.Add(-time.Minute) },
	}
	err := feeder.RunSession(ctx, completed, completed.Close)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunSession error = %v, want context canceled", err)
	}
	if strings.Contains(logs.String(), "earnings watchlist run failed") {
		t.Fatalf("canceled run logged failure:\n%s", logs.String())
	}
	var status string
	var lastError *string
	if err := store.GetDB().QueryRow(context.Background(), `SELECT status, last_error FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target.Date).Scan(&status, &lastError); err != nil {
		t.Fatalf("query claimed run: %v", err)
	}
	if status != "running" || lastError != nil {
		t.Fatalf("run status=%q last_error=%v, want running with no error", status, lastError)
	}
}

func TestWatchlistFeederProviderCancellationFailsHealthyRun(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("store unavailable; skipping DB-backed earnings watchlist test")
	}
	ctx := context.Background()
	completed, ok := engine.MarketSessionForDate(time.Date(2031, time.January, 6, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("expected completed session")
	}
	target := engine.NextMarketSession(completed.Date)
	if _, err := store.GetDB().Exec(ctx, `DELETE FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target.Date); err != nil {
		t.Fatalf("clear run fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target.Date)
	})
	var logs bytes.Buffer
	restore := logger.SetForTesting(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer restore()
	feeder := WatchlistFeeder{
		Calendar: &watchlistCalendarStub{fetch: func(context.Context) ([]data.EarningsRelease, error) {
			return nil, context.Canceled
		}},
		Bars: &watchlistBarsStub{},
		Now:  func() time.Time { return target.Open.Add(-time.Minute) },
	}
	err := feeder.RunSession(ctx, completed, completed.Close)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunSession error = %v, want context canceled", err)
	}
	if !strings.Contains(logs.String(), "earnings watchlist run failed") {
		t.Fatalf("healthy-context provider cancellation did not log failure:\n%s", logs.String())
	}
	var status string
	if err := store.GetDB().QueryRow(ctx, `SELECT status FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target.Date).Scan(&status); err != nil {
		t.Fatalf("query failed run: %v", err)
	}
	if status != "failed" {
		t.Fatalf("run status=%q, want failed", status)
	}
}
