package engine

import (
	"testing"
	"time"
)

func TestIsMarketOpen(t *testing.T) {
	tests := []struct {
		name string
		time string // RFC3339
		want bool
	}{
		{
			name: "Weekday Morning (Open)",
			time: "2024-03-11T10:00:00-04:00", // Monday 10:00 AM EDT
			want: true,
		},
		{
			name: "Weekday Afternoon (Open)",
			time: "2024-03-11T15:59:59-04:00", // Monday 3:59 PM EDT
			want: true,
		},
		{
			name: "Weekday Night (Closed)",
			time: "2024-03-11T20:00:00-04:00", // Monday 8:00 PM EDT
			want: false,
		},
		{
			name: "Early Morning (Closed)",
			time: "2024-03-11T09:00:00-04:00", // Monday 9:00 AM EDT
			want: false,
		},
		{
			name: "Saturday (Closed)",
			time: "2024-03-16T12:00:00-04:00", // Saturday
			want: false,
		},
		{
			name: "Sunday (Closed)",
			time: "2024-03-17T12:00:00-04:00", // Sunday
			want: false,
		},
		{
			name: "New Year's Day (Closed)",
			time: "2024-01-01T12:00:00-05:00", // Holiday
			want: false,
		},
		{
			name: "Christmas Day (Closed)",
			time: "2024-12-25T12:00:00-05:00", // Holiday
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, _ := time.Parse(time.RFC3339, tt.time)
			if got := IsMarketOpen(parsed); got != tt.want {
				t.Errorf("IsMarketOpen(%v) = %v, want %v", parsed, got, tt.want)
			}
		})
	}
}

func TestNextTickTime(t *testing.T) {
	interval := 5 * time.Minute

	t.Run("During Market Hours", func(t *testing.T) {
		now, _ := time.Parse(time.RFC3339, "2024-03-11T10:00:00-04:00") // Monday 10:00 AM EDT
		got := NextTickTime(now, interval)
		want := now.Add(interval)
		if !got.Equal(want) {
			t.Errorf("NextTickTime = %v, want %v", got, want)
		}
	})

	t.Run("After Market Close", func(t *testing.T) {
		now, _ := time.Parse(time.RFC3339, "2024-03-11T16:05:00-04:00") // Monday 4:05 PM EDT
		got := NextTickTime(now, interval)
		// Should be Tuesday 9:30 AM EDT
		want, _ := time.Parse(time.RFC3339, "2024-03-12T09:30:00-04:00")
		if !got.Equal(want) {
			t.Errorf("NextTickTime = %v, want %v", got, want)
		}
	})

	t.Run("Friday After Close", func(t *testing.T) {
		now, _ := time.Parse(time.RFC3339, "2024-03-15T16:05:00-04:00") // Friday 4:05 PM EDT
		got := NextTickTime(now, interval)
		// Should be Monday 9:30 AM EDT
		want, _ := time.Parse(time.RFC3339, "2024-03-18T09:30:00-04:00")
		if !got.Equal(want) {
			t.Errorf("NextTickTime = %v, want %v", got, want)
		}
	})
}

func TestNthWeekday(t *testing.T) {
	// Thanksgiving 2024 is Nov 28 (4th Thursday)
	got := nthWeekday(2024, time.November, time.Thursday, 4)
	want := time.Date(2024, time.November, 28, 0, 0, 0, 0, nyLoc)
	if !got.Equal(want) {
		t.Errorf("nthWeekday = %v, want %v", got, want)
	}
}

func TestLastWeekday(t *testing.T) {
	// Memorial Day 2024 is May 27 (last Monday)
	got := lastWeekday(2024, time.May, time.Monday)
	want := time.Date(2024, time.May, 27, 0, 0, 0, 0, nyLoc)
	if !got.Equal(want) {
		t.Errorf("lastWeekday = %v, want %v", got, want)
	}
}
