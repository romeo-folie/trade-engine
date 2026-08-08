package universe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/store"
)

const (
	universeFailureRetryBase  = 5 * time.Minute
	universeFailureRetryLimit = 3
)

type sessionUniverseBuilder interface {
	RunScheduledSession(context.Context, engine.USMarketSession) (*store.SwingUniverseVersion, error)
}

// ApprovedUniverseJob is the deliberately small extension seam for scanners
// whose decision must follow publication of the session's approved universe.
type ApprovedUniverseJob interface {
	Name() string
	RunSessionWithUniverse(
		context.Context, engine.USMarketSession, *store.SwingUniverseVersion, time.Time,
	) error
}

type SessionScheduler struct {
	Cfg     config.UniverseConfig
	Builder sessionUniverseBuilder
	Jobs    []ApprovedUniverseJob
	Now     func() time.Time
	After   func(time.Duration) <-chan time.Time
}

func (s *SessionScheduler) Run(ctx context.Context) error {
	if !s.Cfg.Enabled || s.Builder == nil {
		return nil
	}
	delay := time.Duration(s.Cfg.AfterCloseDelayMinutes) * time.Minute
	if delay < 0 {
		delay = 0
	}
	session := engine.MostRecentCompletedMarketSession(s.now(), delay)
	failureCount := 0
	for {
		universe, err := s.Builder.RunScheduledSession(ctx, session)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil && universe == nil {
			err = fmt.Errorf("universe builder returned no approved version")
		}
		if err == nil {
			decisionAt := s.now().UTC()
			if universe.CompletedAt != nil && decisionAt.Before(*universe.CompletedAt) {
				decisionAt = *universe.CompletedAt
			}
			err = s.runJobs(ctx, session, universe, decisionAt)
		}
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			failureCount = 0
			var waitUntil time.Time
			session, waitUntil = engine.NextMarketCloseRun(s.now(), delay)
			if err := s.wait(ctx, waitUntil); err != nil {
				return nil
			}
			continue
		}

		failureCount++
		slog.Error("swing universe pipeline: session failed; scheduling retry",
			"session_date", session.Date.Format("2006-01-02"),
			"failure_count", failureCount, "err", err)
		waitUntil := s.now().Add(universeFailureRetryBase << (failureCount - 1))
		var historical *HistoricalSessionUnavailableError
		if errors.As(err, &historical) {
			failureCount = 0
			session, waitUntil = engine.NextMarketCloseRun(s.now(), delay)
		}
		if retryAt := latestRetryAt(err); !retryAt.IsZero() {
			failureCount = 0
			if retryAt.After(waitUntil) {
				waitUntil = retryAt
			}
		}
		if failureCount > universeFailureRetryLimit {
			failureCount = 0
			session, waitUntil = engine.NextMarketCloseRun(s.now(), delay)
		}
		if err := s.wait(ctx, waitUntil); err != nil {
			return nil
		}
		latest := engine.MostRecentCompletedMarketSession(s.now(), delay)
		if latest.Date.After(session.Date) {
			session = latest
			failureCount = 0
		}
	}
}

type retryAtError interface {
	RetryAtTime() time.Time
}

func latestRetryAt(err error) time.Time {
	if err == nil {
		return time.Time{}
	}
	var latest time.Time
	if retryable, ok := err.(retryAtError); ok {
		latest = retryable.RetryAtTime()
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if candidate := latestRetryAt(child); candidate.After(latest) {
				latest = candidate
			}
		}
		return latest
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if candidate := latestRetryAt(wrapped.Unwrap()); candidate.After(latest) {
			latest = candidate
		}
	}
	return latest
}

func (s *SessionScheduler) runJobs(
	ctx context.Context, session engine.USMarketSession,
	universe *store.SwingUniverseVersion, decisionAt time.Time,
) error {
	var wg sync.WaitGroup
	errCh := make(chan error, len(s.Jobs))
	for _, job := range s.Jobs {
		if job == nil {
			continue
		}
		job := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := job.RunSessionWithUniverse(ctx, session, universe, decisionAt); err != nil {
				errCh <- fmt.Errorf("%s: %w", job.Name(), err)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *SessionScheduler) wait(ctx context.Context, until time.Time) error {
	wait := until.Sub(s.now())
	if wait < 0 {
		wait = 0
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.after(wait):
		return nil
	}
}

func (s *SessionScheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *SessionScheduler) after(wait time.Duration) <-chan time.Time {
	if s.After != nil {
		return s.After(wait)
	}
	return time.After(wait)
}
