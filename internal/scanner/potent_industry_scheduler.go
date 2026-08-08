package scanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/engine"
)

type potentIndustrySessionJob interface {
	RunSession(context.Context, engine.USMarketSession, time.Time) error
}

type potentIndustryRunLeaseActiveError struct {
	sessionDate time.Time
	retryAt     time.Time
}

const (
	potentIndustryFailureRetryBase  = 5 * time.Minute
	potentIndustryFailureRetryLimit = 3
)

func (e *potentIndustryRunLeaseActiveError) Error() string {
	return fmt.Sprintf("potent industry session %s is leased until %s",
		e.sessionDate.Format("2006-01-02"), e.retryAt.Format(time.RFC3339))
}

func (e *potentIndustryRunLeaseActiveError) RetryAtTime() time.Time { return e.retryAt }

// PotentIndustryScheduler derives every run from the exchange calendar. Its
// one-shot timers cannot drift across DST, holidays, or early closes.
type PotentIndustryScheduler struct {
	Cfg   config.PotentIndustryConfig
	Job   potentIndustrySessionJob
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
}

func (s *PotentIndustryScheduler) Run(ctx context.Context) error {
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
	failureCount := 0
	for {
		err := s.Job.RunSession(ctx, session, decisionAt)
		if ctx.Err() != nil {
			return nil
		}
		var waitUntil time.Time
		var activeLease *potentIndustryRunLeaseActiveError
		if errors.As(err, &activeLease) {
			slog.Info("potent industry: session actively claimed; retrying at lease boundary",
				"session_date", session.Date.Format("2006-01-02"),
				"retry_at", activeLease.retryAt)
			blockedSession := session
			session, decisionAt, waitUntil = nextPotentIndustryLeaseAttempt(
				s.now(), session, decisionAt, delay, activeLease.retryAt,
			)
			if session.Date.After(blockedSession.Date) {
				failureCount = 0
			}
		} else if err != nil {
			failureCount++
			slog.Error("potent industry: session failed; scheduling retry",
				"session_date", session.Date.Format("2006-01-02"),
				"failure_count", failureCount, "err", err)
			failedSession := session
			session, decisionAt, waitUntil = nextPotentIndustryAttempt(
				s.now(), session, delay, failureCount,
			)
			if session.Date.After(failedSession.Date) {
				failureCount = 0
			}
		} else {
			failureCount = 0
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

func nextPotentIndustryLeaseAttempt(
	now time.Time,
	blocked engine.USMarketSession,
	decisionAt time.Time,
	delay time.Duration,
	leaseUntil time.Time,
) (engine.USMarketSession, time.Time, time.Time) {
	latest := engine.MostRecentCompletedMarketSession(now, delay)
	if latest.Date.After(blocked.Date) {
		latestDecision := latest.Close.Add(delay)
		return latest, latestDecision, now
	}
	if leaseUntil.Before(now) {
		leaseUntil = now
	}
	nextSession, nextRun := engine.NextMarketCloseRun(now, delay)
	if !nextRun.After(leaseUntil) {
		return nextSession, nextRun, nextRun
	}
	return blocked, decisionAt, leaseUntil
}

func nextPotentIndustryAttempt(
	now time.Time,
	failed engine.USMarketSession,
	delay time.Duration,
	failureCount int,
) (engine.USMarketSession, time.Time, time.Time) {
	latest := engine.MostRecentCompletedMarketSession(now, delay)
	if latest.Date.After(failed.Date) {
		decisionAt := latest.Close.Add(delay)
		return latest, decisionAt, now
	}
	nextSession, nextRun := engine.NextMarketCloseRun(now, delay)
	if failureCount > potentIndustryFailureRetryLimit {
		return nextSession, nextRun, nextRun
	}
	retryDelay := potentIndustryFailureRetryBase
	for attempt := 1; attempt < failureCount; attempt++ {
		retryDelay *= 2
	}
	retryAt := now.Add(retryDelay)
	if !nextRun.After(retryAt) {
		return nextSession, nextRun, nextRun
	}
	return failed, failed.Close.Add(delay), retryAt
}

func (s *PotentIndustryScheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *PotentIndustryScheduler) after(wait time.Duration) <-chan time.Time {
	if s.After != nil {
		return s.After(wait)
	}
	return time.After(wait)
}
