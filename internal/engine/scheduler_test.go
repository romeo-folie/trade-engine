package engine

import (
	"testing"
	"time"
)

func mustParseRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("failed to parse time %q: %v", s, err)
	}
	return ts
}

func TestIsMarketOpen(t *testing.T) {
	tests := []struct {
		name string
		ts   string
		want bool
	}{
		{name: "weekday open", ts: "2024-03-11T10:00:00-04:00", want: true},
		{name: "opening bell inclusive", ts: "2024-03-11T09:30:00-04:00", want: true},
		{name: "close exclusive", ts: "2024-03-11T16:00:00-04:00", want: false},
		{name: "pre-market", ts: "2024-03-11T09:00:00-04:00", want: false},
		{name: "after-hours", ts: "2024-03-11T20:00:00-04:00", want: false},
		{name: "saturday", ts: "2024-03-16T12:00:00-04:00", want: false},
		{name: "sunday", ts: "2024-03-17T12:00:00-04:00", want: false},
		{name: "holiday new year", ts: "2024-01-01T12:00:00-05:00", want: false},
		{name: "holiday christmas", ts: "2024-12-25T12:00:00-05:00", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsMarketOpen(mustParseRFC3339(t, tt.ts))
			if got != tt.want {
				t.Errorf("IsMarketOpen(%s) = %v, want %v", tt.ts, got, tt.want)
			}
		})
	}
}

func TestNextTickTime(t *testing.T) {
	interval := 5 * time.Minute

	t.Run("during market hours adds interval", func(t *testing.T) {
		now := mustParseRFC3339(t, "2024-03-11T10:00:00-04:00")
		got := NextTickTime(now, interval)
		want := now.Add(interval)
		if !got.Equal(want) {
			t.Errorf("NextTickTime = %v, want %v", got, want)
		}
	})

	t.Run("after close goes to next session open", func(t *testing.T) {
		now := mustParseRFC3339(t, "2024-03-11T16:05:00-04:00")
		got := NextTickTime(now, interval)
		want := mustParseRFC3339(t, "2024-03-12T09:30:00-04:00")
		if !got.Equal(want) {
			t.Errorf("NextTickTime = %v, want %v", got, want)
		}
	})

	t.Run("friday after close skips weekend", func(t *testing.T) {
		now := mustParseRFC3339(t, "2024-03-15T16:05:00-04:00")
		got := NextTickTime(now, interval)
		want := mustParseRFC3339(t, "2024-03-18T09:30:00-04:00")
		if !got.Equal(want) {
			t.Errorf("NextTickTime = %v, want %v", got, want)
		}
	})
}

func TestHolidayHelpers(t *testing.T) {
	t.Run("nthWeekday thanksgiving", func(t *testing.T) {
		got := nthWeekday(2024, time.November, time.Thursday, 4)
		want := time.Date(2024, time.November, 28, 0, 0, 0, 0, nyLoc)
		if !got.Equal(want) {
			t.Errorf("nthWeekday = %v, want %v", got, want)
		}
	})

	t.Run("lastWeekday memorial day", func(t *testing.T) {
		got := lastWeekday(2024, time.May, time.Monday)
		want := time.Date(2024, time.May, 27, 0, 0, 0, 0, nyLoc)
		if !got.Equal(want) {
			t.Errorf("lastWeekday = %v, want %v", got, want)
		}
	})

	t.Run("good friday", func(t *testing.T) {
		got := goodFriday(2024)
		want := time.Date(2024, time.March, 29, 0, 0, 0, 0, nyLoc)
		if !got.Equal(want) {
			t.Errorf("goodFriday = %v, want %v", got, want)
		}
	})
}

func TestUSHolidays_CurrentAndNextYear(t *testing.T) {
	current := time.Now().In(nyLoc).Year()
	years := []int{current, current + 1}

	for _, year := range years {
		t.Run(time.Date(year, 1, 1, 0, 0, 0, 0, nyLoc).Format("2006"), func(t *testing.T) {
			h := usHolidays(year)
			if len(h) != 10 {
				t.Fatalf("expected 10 US holidays for %d, got %d", year, len(h))
			}

			seen := map[string]bool{}
			for _, d := range h {
				key := d.Format("2006-01-02")
				if seen[key] {
					t.Fatalf("duplicate holiday date in %d: %s", year, key)
				}
				seen[key] = true
			}
		})
	}
}
