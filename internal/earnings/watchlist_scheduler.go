package earnings

import (
	"context"
	"log/slog"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type watchlistSessionJob interface {
	RunSession(context.Context, engine.USMarketSession, time.Time) error
}

// WatchlistScheduler is an exchange-calendar-safe, one-shot after-close
// scheduler. Startup catches up the most recently completed session once.
type WatchlistScheduler struct {
	Cfg                 config.EarningsWatchlistConfig
	Job                 watchlistSessionJob
	Now                 func() time.Time
	After               func(time.Duration) <-chan time.Time
	FindRetryableTarget func(context.Context, time.Time) (time.Time, bool, error)
}

const watchlistFailureRetry = 5 * time.Minute

func (s *WatchlistScheduler) Run(ctx context.Context) error {
	if !s.Cfg.Enabled || s.Job == nil {
		return nil
	}
	delay := time.Duration(s.Cfg.AfterCloseDelayMinutes) * time.Minute
	now := s.now()
	session := s.sessionFor(ctx, now, delay)
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
			// A live persisted cohort wins over a newer session, so deferred
			// admissions keep retrying until their stored leases expire. If the
			// outage happened before selection was persisted, the latest completed
			// session still wins and prevents stale calendar work from blocking.
			latest := s.sessionFor(ctx, wokeAt, delay)
			if !latest.Date.Equal(session.Date) {
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
		latest := s.sessionFor(ctx, waitingAt, delay)
		if !latest.Date.Equal(session.Date) {
			// A recovered older cohort must not strand the latest completed
			// session until its next close. Process it immediately.
			session = latest
			continue
		}
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
		session = s.sessionFor(ctx, wokeAt, delay)
	}
}

func (s *WatchlistScheduler) sessionFor(ctx context.Context, now time.Time, delay time.Duration) engine.USMarketSession {
	latest := engine.MostRecentCompletedMarketSession(now, delay)
	findTarget := s.FindRetryableTarget
	if findTarget == nil {
		findTarget = store.OldestLiveEarningsWatchlistTarget
	}
	target, found, err := findTarget(ctx, now)
	if err != nil {
		loggerWarnRetryableTarget(err)
		return latest
	}
	if !found {
		return latest
	}
	// PostgreSQL DATE values arrive as midnight UTC. Keep the date components
	// intact before passing them to the Eastern-time market calendar; interpreting
	// UTC midnight directly can shift the target to the preceding ET date.
	targetDate := time.Date(target.Year(), target.Month(), target.Day(), 12, 0, 0, 0, time.UTC)
	for day := 1; day <= 14; day++ {
		candidate := targetDate.AddDate(0, 0, -day)
		if previous, ok := engine.MarketSessionForDate(candidate); ok {
			return previous
		}
	}
	return latest
}

func loggerWarnRetryableTarget(err error) {
	slog.Warn("earnings watchlist scheduler could not locate persisted retry cohort", "err", err)
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
