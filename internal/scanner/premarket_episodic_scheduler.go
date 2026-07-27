package scanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/engine"
)

type premarketCheckpointJob interface {
	RunCheckpoint(
		context.Context, engine.USMarketSession, time.Time, time.Time,
	) error
}

type PremarketEpisodicScheduler struct {
	Cfg   config.PremarketEpisodicConfig
	Job   premarketCheckpointJob
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
}

type premarketSchedule struct {
	Session    engine.USMarketSession
	Checkpoint time.Time
	Cutoff     time.Time
	RunAt      time.Time
}

func (s *PremarketEpisodicScheduler) Run(ctx context.Context) error {
	if !s.Cfg.Enabled || s.Job == nil {
		return nil
	}
	var lastCompleted time.Time
	for {
		next, err := nextPremarketScheduleAfter(s.now(), s.Cfg, lastCompleted)
		if err != nil {
			return err
		}
		if wait := next.RunAt.Sub(s.now()); wait > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-s.after(wait):
			}
		}
		err = s.Job.RunCheckpoint(
			ctx, next.Session, next.Checkpoint, next.Cutoff,
		)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			var lease *premarketRunLeaseActiveError
			if errors.As(err, &lease) {
				slog.Info("premarket episodic: checkpoint actively claimed",
					"checkpoint", next.Checkpoint, "retry_at", lease.retryAt)
			} else {
				slog.Error("premarket episodic: checkpoint failed",
					"checkpoint", next.Checkpoint, "err", err)
			}
			retryAt := s.now().Add(time.Minute)
			if lease != nil && lease.retryAt.After(retryAt) {
				retryAt = lease.retryAt
			}
			// Never retry across the hard cutoff. A later configured checkpoint,
			// if one exists, becomes the next authoritative observation.
			if retryAt.Before(next.Cutoff) {
				select {
				case <-ctx.Done():
					return nil
				case <-s.after(retryAt.Sub(s.now())):
				}
			}
		} else {
			lastCompleted = next.Checkpoint
		}
	}
}

func nextPremarketSchedule(
	now time.Time, cfg config.PremarketEpisodicConfig,
) (premarketSchedule, error) {
	return nextPremarketScheduleAfter(now, cfg, time.Time{})
}

func nextPremarketScheduleAfter(
	now time.Time, cfg config.PremarketEpisodicConfig, after time.Time,
) (premarketSchedule, error) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return premarketSchedule{}, err
	}
	nowET := now.In(loc)
	checkpointMinutes, err := parsePremarketClocks(cfg.Checkpoints)
	if err != nil {
		return premarketSchedule{}, err
	}
	cutoffMinutes, err := parsePremarketClock(cfg.HardCutoff)
	if err != nil {
		return premarketSchedule{}, err
	}
	for offset := 0; ; offset++ {
		date := nowET.AddDate(0, 0, offset)
		session, ok := engine.MarketSessionForDate(date)
		if !ok {
			continue
		}
		cutoff := exchangeClock(session.Date, cutoffMinutes)
		if offset == 0 && nowET.Before(cutoff) {
			// Every elapsed checkpoint maps to a durable identity; on startup
			// catch up only the newest elapsed checkpoint before scheduling the
			// next future one.
			for i := len(checkpointMinutes) - 1; i >= 0; i-- {
				checkpoint := exchangeClock(session.Date, checkpointMinutes[i])
				if !checkpoint.After(nowET) && checkpoint.Before(cutoff) &&
					(after.IsZero() || checkpoint.After(after)) {
					return premarketSchedule{
						Session: session, Checkpoint: checkpoint,
						Cutoff: cutoff, RunAt: now,
					}, nil
				}
			}
		}
		for _, minutes := range checkpointMinutes {
			checkpoint := exchangeClock(session.Date, minutes)
			if !checkpoint.Before(cutoff) {
				continue
			}
			if !after.IsZero() && !checkpoint.After(after) {
				continue
			}
			if offset == 0 && nowET.After(cutoff) {
				break
			}
			if offset == 0 && !checkpoint.After(nowET) {
				// Startup catch-up runs only the newest elapsed checkpoint.
				continue
			}
			return premarketSchedule{
				Session: session, Checkpoint: checkpoint,
				Cutoff: cutoff, RunAt: checkpoint,
			}, nil
		}
	}
}

func parsePremarketClocks(values []string) ([]int, error) {
	out := make([]int, 0, len(values))
	for _, value := range values {
		minutes, err := parsePremarketClock(value)
		if err != nil {
			return nil, err
		}
		out = append(out, minutes)
	}
	sort.Ints(out)
	return out, nil
}

func parsePremarketClock(value string) (int, error) {
	parsed, err := time.Parse("15:04", value)
	if err != nil {
		return 0, fmt.Errorf("invalid premarket exchange clock %q: %w", value, err)
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
}

func exchangeClock(date time.Time, minutes int) time.Time {
	return time.Date(
		date.Year(), date.Month(), date.Day(), minutes/60, minutes%60, 0, 0,
		date.Location(),
	)
}

func (s *PremarketEpisodicScheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *PremarketEpisodicScheduler) after(wait time.Duration) <-chan time.Time {
	if wait < 0 {
		wait = 0
	}
	if s.After != nil {
		return s.After(wait)
	}
	return time.After(wait)
}
