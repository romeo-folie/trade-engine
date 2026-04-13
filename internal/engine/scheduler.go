package engine

import (
	"time"
)

// nyLoc is the US/Eastern timezone used for all market hours checks.
var nyLoc = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		// Fallback: UTC-5 (EST). DST transitions will be off by 1h, but the engine
		// will still run — just with a slightly shifted window.
		loc = time.FixedZone("EST", -5*60*60)
	}
	return loc
}()

// IsMarketOpen returns true if the stock market is open at the given time.
// Open = Monday–Friday, 09:30–16:00 US/Eastern, excluding US stock exchange holidays.
func IsMarketOpen(t time.Time) bool {
	et := t.In(nyLoc)
	weekday := et.Weekday()
	if weekday == time.Saturday || weekday == time.Sunday {
		return false
	}
	if isUSHoliday(et) {
		return false
	}
	h, m, _ := et.Clock()
	openMinutes := h*60 + m
	return openMinutes >= 9*60+30 && openMinutes < 16*60
}

// NextTickTime returns the next scheduled tick time starting from 'now'.
// If the market is currently open, it returns now + tickInterval.
// If the market is closed, it returns the next market open (09:30 ET next trading day).
func NextTickTime(now time.Time, tickInterval time.Duration) time.Time {
	next := now.Add(tickInterval)
	if IsMarketOpen(next) {
		return next
	}
	return nextMarketOpen(next)
}

// nextMarketOpen returns the next 09:30 ET on a trading day at or after t.
func nextMarketOpen(t time.Time) time.Time {
	et := t.In(nyLoc)
	candidate := time.Date(et.Year(), et.Month(), et.Day(), 9, 30, 0, 0, nyLoc)
	// If we're past 09:30 today, start from tomorrow.
	if !et.Before(candidate) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	for {
		wd := candidate.Weekday()
		if wd != time.Saturday && wd != time.Sunday && !isUSHoliday(candidate) {
			return candidate
		}
		candidate = candidate.AddDate(0, 0, 1)
	}
}

// ── US stock exchange holiday calendar ───────────────────────────────────────
//
// Covers all NYSE/NASDAQ observed holidays. Floating holidays (MLK Day, Presidents'
// Day, Memorial Day, Good Friday, Labor Day, Thanksgiving) are computed from rules.

func isUSHoliday(t time.Time) bool {
	et := t.In(nyLoc)
	year := et.Year()
	month := et.Month()
	day := et.Day()
	d := time.Date(year, month, day, 0, 0, 0, 0, nyLoc)

	// A Friday observed holiday for a Saturday New Year might fall on 12-31 of year-1.
	// A Monday observed holiday for a Sunday New Year might fall on 01-02 of year.
	// We check the target year, previous year, and next year to catch observed shifts.
	for _, y := range []int{year - 1, year, year + 1} {
		holidays := usHolidays(y)
		for _, h := range holidays {
			if h.Equal(d) {
				return true
			}
		}
	}
	return false
}

// usHolidays returns the observed NYSE holiday dates for a given year.
func usHolidays(year int) []time.Time {
	h := []time.Time{
		observeHoliday(time.Date(year, time.January, 1, 0, 0, 0, 0, nyLoc)),   // New Year's Day
		nthWeekday(year, time.January, time.Monday, 3),                          // MLK Day (3rd Mon Jan)
		nthWeekday(year, time.February, time.Monday, 3),                         // Presidents' Day (3rd Mon Feb)
		goodFriday(year),                                                          // Good Friday
		lastWeekday(year, time.May, time.Monday),                                 // Memorial Day (last Mon May)
		observeHoliday(time.Date(year, time.June, 19, 0, 0, 0, 0, nyLoc)),      // Juneteenth
		observeHoliday(time.Date(year, time.July, 4, 0, 0, 0, 0, nyLoc)),       // Independence Day
		nthWeekday(year, time.September, time.Monday, 1),                         // Labor Day (1st Mon Sep)
		nthWeekday(year, time.November, time.Thursday, 4),                        // Thanksgiving (4th Thu Nov)
		observeHoliday(time.Date(year, time.December, 25, 0, 0, 0, 0, nyLoc)), // Christmas
	}
	return h
}

// observeHoliday shifts a holiday to Friday if it falls on Saturday,
// or to Monday if it falls on Sunday.
func observeHoliday(d time.Time) time.Time {
	switch d.Weekday() {
	case time.Saturday:
		return d.AddDate(0, 0, -1) // Friday
	case time.Sunday:
		return d.AddDate(0, 0, 1) // Monday
	}
	return d
}

// nthWeekday returns the date of the nth occurrence of weekday wd in month m of year y.
func nthWeekday(year int, month time.Month, wd time.Weekday, n int) time.Time {
	t := time.Date(year, month, 1, 0, 0, 0, 0, nyLoc)
	count := 0
	for {
		if t.Weekday() == wd {
			count++
			if count == n {
				return t
			}
		}
		t = t.AddDate(0, 0, 1)
	}
}

// lastWeekday returns the last occurrence of weekday wd in month m of year y.
func lastWeekday(year int, month time.Month, wd time.Weekday) time.Time {
	// Start from the first day of the next month and go back.
	t := time.Date(year, month+1, 1, 0, 0, 0, 0, nyLoc).AddDate(0, 0, -1)
	for t.Weekday() != wd {
		t = t.AddDate(0, 0, -1)
	}
	return t
}

// goodFriday returns Good Friday for the given year using the Anonymous Gregorian algorithm.
func goodFriday(year int) time.Time {
	// Easter Sunday via Anonymous Gregorian algorithm.
	a := year % 19
	b := year / 100
	c := year % 100
	d := b / 4
	e := b % 4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i := c / 4
	k := c % 4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := ((h + l - 7*m + 114) % 31) + 1
	easter := time.Date(year, time.Month(month), day, 0, 0, 0, 0, nyLoc)
	return easter.AddDate(0, 0, -2) // Good Friday is 2 days before Easter
}

// startupSkipDecision determines whether the startup tick should be skipped
// and, if so, when the next tick should fire.
//
// skip=true when lastAnalysis is non-nil and its age relative to now is less
// than the tick interval — meaning an analysis already ran recently enough
// that firing again immediately would be redundant.
//
// When skip=true, nextTick is NextTickTime(lastAnalysis, interval), preserving
// the cadence from the last run rather than resetting the clock to now.
// When skip=false, nextTick is the zero value and the caller uses its normal
// scheduling path.
func startupSkipDecision(now time.Time, lastAnalysis *time.Time, interval time.Duration) (skip bool, nextTick time.Time) {
	if lastAnalysis == nil {
		return false, time.Time{}
	}
	if now.Sub(*lastAnalysis) >= interval {
		return false, time.Time{}
	}
	return true, NextTickTime(*lastAnalysis, interval)
}
