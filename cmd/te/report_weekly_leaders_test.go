package main

import (
	"strings"
	"testing"
	"time"
)

func TestWeeklyLeaderReportDateRangeDefaultsToExchangeCalendarDates(t *testing.T) {
	// This instant is already July 27 in Tokyo, but still July 26 in New York.
	now := time.Date(2026, time.July, 26, 22, 30, 0, 0, time.UTC)

	from, to, err := weeklyLeaderReportDateRange(now, "", "")
	if err != nil {
		t.Fatal(err)
	}
	assertReportDate(t, from, "2026-06-26")
	assertReportDate(t, to, "2026-07-26")
}

func TestWeeklyLeaderReportDateRangeIsInclusive(t *testing.T) {
	from, to, err := weeklyLeaderReportDateRange(
		time.Time{}, "2026-07-20", "2026-07-20",
	)
	if err != nil {
		t.Fatal(err)
	}
	assertReportDate(t, from, "2026-07-20")
	assertReportDate(t, to, "2026-07-20")
}

func TestWeeklyLeaderReportDateRangeRejectsReversedRange(t *testing.T) {
	_, _, err := weeklyLeaderReportDateRange(
		time.Time{}, "2026-07-21", "2026-07-20",
	)
	if err == nil || !strings.Contains(err.Error(), "--from must be on or before --to") {
		t.Fatalf("error = %v, want reversed-range error", err)
	}
}

func assertReportDate(t *testing.T, got time.Time, want string) {
	t.Helper()
	if got.Format("2006-01-02") != want {
		t.Fatalf("date = %s, want %s", got.Format("2006-01-02"), want)
	}
	if got.Location() != time.UTC || got.Hour() != 0 || got.Minute() != 0 ||
		got.Second() != 0 || got.Nanosecond() != 0 {
		t.Fatalf("date is not normalized UTC midnight: %s", got)
	}
}
