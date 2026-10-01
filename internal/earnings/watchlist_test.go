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
	"github.com/romeo-folie/trade-engine/internal/events"
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

func TestWatchlistFeederPublishesScheduledEarningsPromotionOutcome(t *testing.T) {
	bus := events.New()
	sub := bus.Subscribe(events.ScannerPromotionOutcome)
	defer bus.Unsubscribe(sub)
	feeder := WatchlistFeeder{Bus: bus}
	feeder.publishPromotionOutcome("run-1", store.EarningsWatchlistCandidate{
		Ticker: "NVDA", LiquidityRank: 2, MedianDollarVolume: 1_250_000,
	}, "not_admitted", "queue_capacity", "source reservation and overflow unavailable")
	select {
	case event := <-sub.C:
		payload, ok := event.Payload.(events.ScannerPromotionOutcomePayload)
		if !ok {
			t.Fatalf("payload type = %T", event.Payload)
		}
		if payload.Outcome != "not_admitted" || payload.ReasonCode != "queue_capacity" ||
			len(payload.Sources) != 1 || payload.Sources[0].Source != "earnings_scheduled" || payload.Sources[0].Rank != 2 {
			t.Fatalf("promotion outcome = %+v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduled earnings promotion outcome was not published")
	}
	feeder.publishPromotionOutcome("run-1", store.EarningsWatchlistCandidate{
		Ticker: "AMD", LiquidityRank: 1, MedianDollarVolume: 2_000_000,
	}, "rediscovered", "", "existing research membership retained")
	select {
	case event := <-sub.C:
		payload, ok := event.Payload.(events.ScannerPromotionOutcomePayload)
		if !ok || payload.Outcome != "rediscovered" || payload.Ticker != "AMD" {
			t.Fatalf("rediscovery outcome = %+v", event.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduled earnings rediscovery outcome was not published")
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
	}), FindRetryableTarget: func(context.Context, time.Time) (time.Time, bool, error) {
		return time.Time{}, false, nil
	}}
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

func TestWatchlistSchedulerKeepsPersistedCohortAcrossTargetOpen(t *testing.T) {
	completed, ok := engine.MarketSessionForDate(time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("expected completed Friday session")
	}
	target := engine.NextMarketSession(completed.Date)
	now := completed.Close.Add(5 * time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	s := WatchlistScheduler{
		Cfg: config.EarningsWatchlistConfig{Enabled: true, AfterCloseDelayMinutes: 5},
		Now: func() time.Time { return now },
		After: func(time.Duration) <-chan time.Time {
			now = target.Open.Add(time.Minute)
			ch := make(chan time.Time, 1)
			ch <- now
			return ch
		},
		FindRetryableTarget: func(context.Context, time.Time) (time.Time, bool, error) {
			return target.Date, true, nil
		},
		Job: schedulerJob(func(_ context.Context, session engine.USMarketSession, _ time.Time) error {
			calls++
			if !session.Date.Equal(completed.Date) {
				t.Fatalf("retry session=%s, want original completed session %s", session.Date, completed.Date)
			}
			if calls == 1 {
				return errors.New("capacity unavailable")
			}
			cancel()
			return nil
		}),
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("run attempts=%d, want a retry after target open", calls)
	}
}

func TestWatchlistSchedulerProcessesLatestSessionAfterRetryingOlderCohort(t *testing.T) {
	friday, ok := engine.MarketSessionForDate(time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("expected Friday market session")
	}
	target := engine.NextMarketSession(friday.Date)
	now := target.Close.Add(10 * time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var active bool = true
	calls := 0
	s := WatchlistScheduler{
		Cfg: config.EarningsWatchlistConfig{Enabled: true, AfterCloseDelayMinutes: 5},
		Now: func() time.Time { return now },
		After: func(time.Duration) <-chan time.Time {
			t.Fatal("scheduler waited for another close instead of processing the latest completed session")
			return nil
		},
		FindRetryableTarget: func(context.Context, time.Time) (time.Time, bool, error) {
			return target.Date, active, nil
		},
		Job: schedulerJob(func(_ context.Context, session engine.USMarketSession, _ time.Time) error {
			calls++
			if calls == 1 {
				if !session.Date.Equal(friday.Date) {
					t.Fatalf("first session=%s, want persisted cohort session %s", session.Date, friday.Date)
				}
				active = false
				return nil
			}
			if !session.Date.Equal(target.Date) {
				t.Fatalf("second session=%s, want latest completed session %s", session.Date, target.Date)
			}
			cancel()
			return nil
		}),
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("run attempts=%d, want old retry then latest session", calls)
	}
}

func TestWatchlistSchedulerPreservesUTCDateForPersistedTarget(t *testing.T) {
	ctx := context.Background()
	target := time.Date(2026, time.August, 27, 0, 0, 0, 0, time.UTC) // Thursday DATE from PostgreSQL.
	scheduler := WatchlistScheduler{FindRetryableTarget: func(context.Context, time.Time) (time.Time, bool, error) {
		return target, true, nil
	}}
	session := scheduler.sessionFor(ctx, target.Add(24*time.Hour), 5*time.Minute)
	wednesday, ok := engine.MarketSessionForDate(time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("expected Wednesday market session")
	}
	if !session.Date.Equal(wednesday.Date) {
		t.Fatalf("retry session=%s, want session before Thursday target: %s", session.Date, wednesday.Date)
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

func TestWatchlistFeederRetriesLivePersistedCohortAfterTargetOpen(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("store unavailable; skipping DB-backed earnings watchlist test")
	}
	ctx := context.Background()
	completed, ok := engine.MarketSessionForDate(time.Date(2031, time.January, 7, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("expected completed session")
	}
	target := engine.NextMarketSession(completed.Date)
	if _, err := store.GetDB().Exec(ctx, `DELETE FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target.Date); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.GetDB().Exec(context.Background(), `DELETE FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target.Date)
	})
	attemptID, claimed, err := store.ClaimEarningsWatchlistRun(ctx, target.Date, target.Open.Add(-time.Hour))
	if err != nil || !claimed {
		t.Fatalf("claim setup run: claimed=%v err=%v", claimed, err)
	}
	expiresAt := engine.MarketSessionOffset(target.Date, 2).Close
	candidate := store.EarningsWatchlistCandidate{
		Ticker: "RETRYLIVE", ContextKey: "earnings_scheduled:" + target.Date.Format("2006-01-02"),
		Context:   earningsDiscoveryContext(data.EarningsRelease{Symbol: "RETRYLIVE"}, target, 1, 1),
		ExpiresAt: expiresAt, LiquidityRank: 1,
	}
	if _, err := store.PersistEarningsWatchlistCohort(ctx, target.Date, attemptID, []store.EarningsWatchlistCandidate{candidate}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordEarningsWatchlistCandidate(ctx, target.Date, candidate.Ticker, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.FailEarningsWatchlistRun(ctx, target.Date, attemptID, errors.New("capacity unavailable")); err != nil {
		t.Fatal(err)
	}
	calendar := &watchlistCalendarStub{fetch: func(context.Context) ([]data.EarningsRelease, error) {
		return nil, errors.New("calendar must not be refetched for persisted cohort")
	}}
	feeder := WatchlistFeeder{
		Calendar: calendar, Bars: &watchlistBarsStub{},
		Now: func() time.Time { return target.Open.Add(time.Minute) },
	}
	if err := feeder.RunSession(ctx, completed, completed.Close); err != nil {
		t.Fatalf("retry persisted cohort after target open: %v", err)
	}
	if calendar.calls != 0 {
		t.Fatalf("calendar calls=%d, want persisted cohort reuse", calendar.calls)
	}
	var status string
	if err := store.GetDB().QueryRow(ctx, `SELECT status FROM earnings_watchlist_runs WHERE target_session_date=$1::date`, target.Date).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "completed" {
		t.Fatalf("retry status=%q, want completed", status)
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
