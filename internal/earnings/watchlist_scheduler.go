package earnings

import (
	"context"
	"log/slog"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/engine"
)

type watchlistSessionJob interface {
	RunSession(context.Context, engine.USMarketSession, time.Time) error
}

// WatchlistScheduler is an exchange-calendar-safe, one-shot after-close
// scheduler. Startup catches up the most recently completed session once.
type WatchlistScheduler struct {
	Cfg   config.EarningsWatchlistConfig
	Job   watchlistSessionJob
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
}

const watchlistFailureRetry = 5 * time.Minute

func (s *WatchlistScheduler) Run(ctx context.Context) error {
	if !s.Cfg.Enabled || s.Job == nil {
		return nil
	}
	delay := time.Duration(s.Cfg.AfterCloseDelayMinutes) * time.Minute
	now := s.now()
	session := engine.MostRecentCompletedMarketSession(now, delay)
	slog.Info("earnings watchlist scheduler initialized",
		"completed_session_date", session.Date.Format("2006-01-02"),
		"scheduled_for", session.Close.Add(delay),
		"now", now,
		"delay", delay,
	)
	failureCount := 0
	for {
		if err := s.Job.RunSession(ctx, session, session.Close.Add(delay)); err != nil && ctx.Err() == nil {
			failureCount++
			waitingAt := s.now()
			retryAt := waitingAt.Add(watchlistFailureRetry)
			slog.Warn("earnings watchlist feeder failed; retry scheduled",
				"completed_session_date", session.Date.Format("2006-01-02"),
				"failure_count", failureCount,
				"retry_at", retryAt,
				"err", err,
			)
			slog.Info("earnings watchlist scheduler waiting",
				"reason", "retry",
				"completed_session_date", session.Date.Format("2006-01-02"),
				"wake_at", retryAt,
				"wait_seconds", watchlistFailureRetry.Seconds(),
			)
			select {
			case <-ctx.Done():
				return nil
			case <-s.after(watchlistFailureRetry):
			}
			wokeAt := s.now()
			slog.Info("earnings watchlist scheduler woke",
				"reason", "retry",
				"completed_session_date", session.Date.Format("2006-01-02"),
				"wake_at", retryAt,
				"woke_at", wokeAt,
				"lag_seconds", wokeAt.Sub(retryAt).Seconds(),
			)
			// A newer completed session wins over a stale retry; this prevents an
			// extended outage from holding the feeder on Friday through Monday.
			latest := engine.MostRecentCompletedMarketSession(wokeAt, delay)
			if latest.Date.After(session.Date) {
				session = latest
				failureCount = 0
			}
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		failureCount = 0
		waitingAt := s.now()
		_, runAt := engine.NextMarketCloseRun(waitingAt, delay)
		wait := runAt.Sub(waitingAt)
		if wait < 0 {
			wait = 0
		}
		slog.Info("earnings watchlist scheduler waiting",
			"reason", "next_close",
			"completed_session_date", session.Date.Format("2006-01-02"),
			"wake_at", runAt,
			"wait_seconds", wait.Seconds(),
		)
		select {
		case <-ctx.Done():
			return nil
		case <-s.after(wait):
		}
		wokeAt := s.now()
		slog.Info("earnings watchlist scheduler woke",
			"reason", "next_close",
			"completed_session_date", session.Date.Format("2006-01-02"),
			"wake_at", runAt,
			"woke_at", wokeAt,
			"lag_seconds", wokeAt.Sub(runAt).Seconds(),
		)
		session = engine.MostRecentCompletedMarketSession(wokeAt, delay)
	}
}

func (s *WatchlistScheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
func (s *WatchlistScheduler) after(wait time.Duration) <-chan time.Time {
	if s.After != nil {
		return s.After(wait)
	}
	return time.After(wait)
}
