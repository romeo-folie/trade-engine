package engine

import (
	"testing"
	"time"
)

func mustParseNY(t *testing.T, s string) time.Time {
	t.Helper()
	// Parse without zone, then attach nyLoc.
	ts, err := time.ParseInLocation("2006-01-02T15:04:05", s, nyLoc)
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
		{name: "weekday open", ts: "2024-03-11T10:00:00", want: true},
		{name: "opening bell inclusive", ts: "2024-03-11T09:30:00", want: true},
		{name: "close exclusive", ts: "2024-03-11T16:00:00", want: false},
		{name: "pre-market", ts: "2024-03-11T09:00:00", want: false},
		{name: "after-hours", ts: "2024-03-11T20:00:00", want: false},
		{name: "saturday", ts: "2024-03-16T12:00:00", want: false},
		{name: "sunday", ts: "2024-03-17T12:00:00", want: false},
		{name: "holiday new year", ts: "2024-01-01T12:00:00", want: false},
		{name: "holiday christmas", ts: "2024-12-25T12:00:00", want: false},
		{name: "holiday new year observed prior year", ts: "2021-12-31T12:00:00", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsMarketOpen(mustParseNY(t, tt.ts))
			if got != tt.want {
				t.Errorf("IsMarketOpen(%s) = %v, want %v", tt.ts, got, tt.want)
			}
		})
	}
}

func TestNextTickTime(t *testing.T) {
	interval := 5 * time.Minute

	t.Run("during market hours adds interval", func(t *testing.T) {
		now := mustParseNY(t, "2024-03-11T10:00:00")
		got := NextTickTime(now, interval)
		want := now.Add(interval)
		if !got.Equal(want) {
			t.Errorf("NextTickTime = %v, want %v", got, want)
		}
	})

	t.Run("after close goes to next session open", func(t *testing.T) {
		now := mustParseNY(t, "2024-03-11T16:05:00")
		got := NextTickTime(now, interval)
		want := mustParseNY(t, "2024-03-12T09:30:00")
		if !got.Equal(want) {
			t.Errorf("NextTickTime = %v, want %v", got, want)
		}
	})

	t.Run("friday after close skips weekend", func(t *testing.T) {
		now := mustParseNY(t, "2024-03-15T16:05:00")
		got := NextTickTime(now, interval)
		want := mustParseNY(t, "2024-03-18T09:30:00")
		if !got.Equal(want) {
			t.Errorf("NextTickTime = %v, want %v", got, want)
		}
	})

	t.Run("saturday mid-day skips to monday open", func(t *testing.T) {
		// Regression: prior loop used time.NewTicker (fixed interval) so a
		// tick scheduled during market hours would fire on Saturday if the
		// interval bridged the weekend. NextTickTime must jump to Monday 09:30.
		now := mustParseNY(t, "2024-03-16T12:00:00") // Saturday
		got := NextTickTime(now, interval)
		want := mustParseNY(t, "2024-03-18T09:30:00") // Monday
		if !got.Equal(want) {
			t.Errorf("NextTickTime(saturday) = %v, want %v", got, want)
		}
	})

	t.Run("sunday mid-day skips to monday open", func(t *testing.T) {
		now := mustParseNY(t, "2024-03-17T10:00:00") // Sunday
		got := NextTickTime(now, interval)
		want := mustParseNY(t, "2024-03-18T09:30:00") // Monday
		if !got.Equal(want) {
			t.Errorf("NextTickTime(sunday) = %v, want %v", got, want)
		}
	})

	t.Run("holiday skips to next trading day", func(t *testing.T) {
		// 2024-01-15 is MLK Day (observed). Timer should jump to 2024-01-16 09:30.
		now := mustParseNY(t, "2024-01-15T11:00:00")
		got := NextTickTime(now, interval)
		want := mustParseNY(t, "2024-01-16T09:30:00")
		if !got.Equal(want) {
			t.Errorf("NextTickTime(holiday) = %v, want %v", got, want)
		}
	})

	t.Run("pre-market with large interval overshoots open", func(t *testing.T) {
		// Regression: NextTickTime(09:00, 1h) = 10:00 (IsMarketOpen(10:00)=true).
		// This is the correct NextTickTime result — the function does not snap to
		// the opening bell. The caller (nextEngineWake) is responsible for choosing
		// nextMarketOpen when no tick was run.
		now := mustParseNY(t, "2024-03-11T09:00:00") // pre-market Monday
		got := NextTickTime(now, time.Hour)
		want := mustParseNY(t, "2024-03-11T10:00:00") // now+1h is open — correct
		if !got.Equal(want) {
			t.Errorf("NextTickTime(pre-market, 1h) = %v, want %v", got, want)
		}
	})
}

// TestNextEngineWake verifies that nextEngineWake selects the right strategy:
// - after a tick (ranTick=true):  now+interval, routed to next open if needed
// - after a skip (ranTick=false): nextMarketOpen(now), ignoring interval
func TestNextEngineWake(t *testing.T) {
	oneHour := time.Hour

	t.Run("after tick during market hours uses interval", func(t *testing.T) {
		now := mustParseNY(t, "2024-03-11T10:00:00")
		got := nextEngineWake(now, oneHour, true)
		want := now.Add(oneHour) // 11:00 — still open
		if !got.Equal(want) {
			t.Errorf("nextEngineWake(ranTick=true) = %v, want %v", got, want)
		}
	})

	t.Run("after tick near close routes to next open", func(t *testing.T) {
		now := mustParseNY(t, "2024-03-11T15:30:00")
		got := nextEngineWake(now, oneHour, true)
		want := mustParseNY(t, "2024-03-12T09:30:00") // 16:30 is after close
		if !got.Equal(want) {
			t.Errorf("nextEngineWake(ranTick=true, near-close) = %v, want %v", got, want)
		}
	})

	t.Run("after skip pre-market wakes at opening bell not now+interval", func(t *testing.T) {
		// Key regression: 09:00 + 1h = 10:00 via NextTickTime, but after a skip
		// the engine must wake at 09:30 so it doesn't miss the opening bell.
		now := mustParseNY(t, "2024-03-11T09:00:00")
		got := nextEngineWake(now, oneHour, false)
		want := mustParseNY(t, "2024-03-11T09:30:00")
		if !got.Equal(want) {
			t.Errorf("nextEngineWake(ranTick=false, pre-market) = %v, want %v", got, want)
		}
	})

	t.Run("after skip on saturday wakes monday open", func(t *testing.T) {
		now := mustParseNY(t, "2024-03-16T12:00:00") // Saturday
		got := nextEngineWake(now, oneHour, false)
		want := mustParseNY(t, "2024-03-18T09:30:00") // Monday
		if !got.Equal(want) {
			t.Errorf("nextEngineWake(ranTick=false, saturday) = %v, want %v", got, want)
		}
	})
}

func ptr(t time.Time) *time.Time { return &t }

// TestStartupSkipDecision covers the pure skip-decision and next-tick scheduling
// logic introduced in the skip_recent_tick feature.
func TestStartupSkipDecision(t *testing.T) {
	interval := time.Hour

	tests := []struct {
		name         string
		now          string
		lastAnalysis *time.Time  // nil → no prior analyses
		wantSkip     bool
		wantNextTick string // empty string → zero value (don't check)
	}{
		{
			name:         "no prior analyses — do not skip",
			now:          "2024-03-11T10:00:00",
			lastAnalysis: nil,
			wantSkip:     false,
		},
		{
			name:         "last analysis older than interval — do not skip",
			now:          "2024-03-11T10:00:00",
			lastAnalysis: ptr(mustParseNY(t, "2024-03-11T08:30:00")), // 90 min ago
			wantSkip:     false,
		},
		{
			name:         "last analysis exactly at interval boundary — do not skip",
			now:          "2024-03-11T10:00:00",
			lastAnalysis: ptr(mustParseNY(t, "2024-03-11T09:00:00")), // exactly 1h ago
			wantSkip:     false,
		},
		{
			name:         "last analysis recent — skip and schedule from last analysis",
			now:          "2024-03-11T10:00:00",
			lastAnalysis: ptr(mustParseNY(t, "2024-03-11T09:45:00")), // 15 min ago
			wantSkip:     true,
			wantNextTick: "2024-03-11T10:45:00", // lastAnalysis + 1h
		},
		{
			name:         "recent analysis, next tick crosses market close — routes to next open",
			now:          "2024-03-11T15:45:00",
			lastAnalysis: ptr(mustParseNY(t, "2024-03-11T15:30:00")), // 15 min ago
			wantSkip:     true,
			wantNextTick: "2024-03-12T09:30:00", // 15:30+1h=16:30 is after close
		},
		{
			name:         "recent analysis on friday, next tick falls on weekend — routes to monday",
			now:          "2024-03-15T15:45:00", // Friday
			lastAnalysis: ptr(mustParseNY(t, "2024-03-15T15:30:00")), // 15 min ago
			wantSkip:     true,
			wantNextTick: "2024-03-18T09:30:00", // 15:30+1h=16:30 Friday → Monday open
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := mustParseNY(t, tt.now)
			skip, nextTick := startupSkipDecision(now, tt.lastAnalysis, interval)

			if skip != tt.wantSkip {
				t.Errorf("skip = %v, want %v", skip, tt.wantSkip)
			}
			if tt.wantNextTick != "" {
				want := mustParseNY(t, tt.wantNextTick)
				if !nextTick.Equal(want) {
					t.Errorf("nextTick = %v, want %v", nextTick, want)
				}
			}
			if !tt.wantSkip && !nextTick.IsZero() {
				t.Errorf("expected zero nextTick when skip=false, got %v", nextTick)
			}
		})
	}
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
