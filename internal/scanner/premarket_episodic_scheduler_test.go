package scanner

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/engine"
)

type recordingPremarketJob struct {
	calls int
}

func (j *recordingPremarketJob) RunCheckpoint(
	context.Context, engine.USMarketSession, time.Time, time.Time,
) error {
	j.calls++
	return nil
}

func TestPremarketSchedulerDefaultOffDoesNoWork(t *testing.T) {
	job := &recordingPremarketJob{}
	scheduler := PremarketEpisodicScheduler{
		Cfg: config.PremarketEpisodicConfig{}, Job: job,
	}
	if err := scheduler.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if job.calls != 0 {
		t.Fatalf("default-off scheduler calls = %d", job.calls)
	}
}

func TestNextPremarketScheduleIsDSTAndCalendarAware(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	cfg := config.Defaults().Scanner.PremarketEpisodic
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			"summer",
			time.Date(2026, 7, 27, 7, 30, 0, 0, loc),
			time.Date(2026, 7, 27, 8, 0, 0, 0, loc),
		},
		{
			"winter",
			time.Date(2026, 12, 7, 7, 30, 0, 0, loc),
			time.Date(2026, 12, 7, 8, 0, 0, 0, loc),
		},
		{
			"weekend",
			time.Date(2026, 7, 25, 8, 30, 0, 0, loc),
			time.Date(2026, 7, 27, 8, 0, 0, 0, loc),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nextPremarketSchedule(tc.now, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Checkpoint.Equal(tc.want) {
				t.Fatalf("checkpoint = %s; want %s", got.Checkpoint, tc.want)
			}
		})
	}
}

func TestNextPremarketScheduleCatchesUpAndSkipsAfterCutoff(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	cfg := config.Defaults().Scanner.PremarketEpisodic
	catchupNow := time.Date(2026, 7, 27, 9, 10, 0, 0, loc)
	got, err := nextPremarketSchedule(catchupNow, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 27, 9, 0, 0, 0, loc)
	if !got.Checkpoint.Equal(want) || !got.RunAt.Equal(catchupNow) {
		t.Fatalf("catch-up schedule = %+v", got)
	}

	afterCutoff := time.Date(2026, 7, 27, 9, 26, 0, 0, loc)
	got, err = nextPremarketSchedule(afterCutoff, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want = time.Date(2026, 7, 28, 8, 0, 0, 0, loc)
	if !got.Checkpoint.Equal(want) {
		t.Fatalf("post-cutoff checkpoint = %s; want %s", got.Checkpoint, want)
	}
}

func TestNextPremarketScheduleDoesNotRepeatCompletedCheckpoint(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	cfg := config.Defaults().Scanner.PremarketEpisodic
	now := time.Date(2026, 7, 27, 8, 1, 0, 0, loc)
	completed := time.Date(2026, 7, 27, 8, 0, 0, 0, loc)
	got, err := nextPremarketScheduleAfter(now, cfg, completed)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 27, 9, 0, 0, 0, loc)
	if !got.Checkpoint.Equal(want) {
		t.Fatalf("next checkpoint = %s; want %s", got.Checkpoint, want)
	}
}
