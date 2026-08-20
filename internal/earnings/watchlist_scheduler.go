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
	for {
		if err := s.Job.RunSession(ctx, session, session.Close.Add(delay)); err != nil && ctx.Err() == nil {
			slog.Warn("earnings watchlist feeder failed; retrying", "session_date", session.Date.Format("2006-01-02"), "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-s.after(watchlistFailureRetry):
			}
			// A newer completed session wins over a stale retry; this prevents an
			// extended outage from holding the feeder on Friday through Monday.
			latest := engine.MostRecentCompletedMarketSession(s.now(), delay)
			if latest.Date.After(session.Date) {
				session = latest
			}
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		_, runAt := engine.NextMarketCloseRun(s.now(), delay)
		wait := runAt.Sub(s.now())
		if wait < 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.after(wait):
		}
		session = engine.MostRecentCompletedMarketSession(s.now(), delay)
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
