package scanner

import (
	"context"
	"log/slog"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/engine"
)

type weeklyLeaderSessionJob interface {
	RunSession(context.Context, engine.USMarketSession, time.Time) error
}

// WeeklyLeaderScheduler uses one-shot timers so a DST transition, weekend, or
// holiday cannot drift a fixed-duration ticker away from the exchange close.
type WeeklyLeaderScheduler struct {
	Cfg   config.WeeklyLeaderConfig
	Job   weeklyLeaderSessionJob
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
}

func (s *WeeklyLeaderScheduler) Run(ctx context.Context) error {
	if !s.Cfg.Enabled || s.Job == nil {
		return nil
	}
	delay := time.Duration(s.Cfg.AfterCloseDelayMinutes) * time.Minute
	if delay < 0 {
		delay = 0
	}
	now := s.now()
	session := engine.MostRecentCompletedMarketSession(now, delay)
	decisionAt := session.Close.Add(delay)
	for {
		err := s.Job.RunSession(ctx, session, decisionAt)
		if ctx.Err() != nil {
			return nil
		}
		var waitUntil time.Time
		if err != nil {
			slog.Error("weekly leader: session failed; scheduling retry",
				"session_date", session.Date.Format("2006-01-02"), "err", err)
			session, decisionAt, waitUntil = nextWeeklyLeaderAttempt(
				s.now(), session, delay,
			)
		} else {
			session, waitUntil = engine.NextMarketCloseRun(s.now(), delay)
			decisionAt = waitUntil
		}
		wait := waitUntil.Sub(s.now())
		if wait < 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.after(wait):
		}
	}
}

func nextWeeklyLeaderAttempt(
	now time.Time, failed engine.USMarketSession, delay time.Duration,
) (engine.USMarketSession, time.Time, time.Time) {
	latest := engine.MostRecentCompletedMarketSession(now, delay)
	if latest.Date.After(failed.Date) {
		decisionAt := latest.Close.Add(delay)
		return latest, decisionAt, now
	}
	nextSession, nextRun := engine.NextMarketCloseRun(now, delay)
	retryAt := now.Add(5 * time.Minute)
	if nextRun.Before(retryAt) {
		return nextSession, nextRun, nextRun
	}
	return failed, failed.Close.Add(delay), retryAt
}

func (s *WeeklyLeaderScheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *WeeklyLeaderScheduler) after(wait time.Duration) <-chan time.Time {
	if s.After != nil {
		return s.After(wait)
	}
	return time.After(wait)
}
