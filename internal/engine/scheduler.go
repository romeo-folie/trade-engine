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

// USMarketSession is an official regular-hours US equity session. Date, Open,
// and Close are expressed in America/New_York and therefore remain DST-safe.
type USMarketSession struct {
	Date  time.Time
	Open  time.Time
	Close time.Time
}

// MarketSessionForDate returns the regular session for the Eastern calendar
// date containing t. Weekends and exchange holidays return ok=false.
func MarketSessionForDate(t time.Time) (session USMarketSession, ok bool) {
	et := t.In(nyLoc)
	date := time.Date(et.Year(), et.Month(), et.Day(), 0, 0, 0, 0, nyLoc)
	if !IsTradingDay(date) {
		return USMarketSession{}, false
	}
	closeHour := 16
	if isEarlyClose(date) {
		closeHour = 13
	}
	return USMarketSession{
		Date:  date,
		Open:  time.Date(et.Year(), et.Month(), et.Day(), 9, 30, 0, 0, nyLoc),
		Close: time.Date(et.Year(), et.Month(), et.Day(), closeHour, 0, 0, 0, nyLoc),
	}, true
}

// IsTradingDay reports whether t's Eastern calendar date is an exchange
// session, independently of the time of day.
func IsTradingDay(t time.Time) bool {
	et := t.In(nyLoc)
	return et.Weekday() != time.Saturday && et.Weekday() != time.Sunday &&
		!isUSHoliday(et) && !isExceptionalNYSEClosure(et)
}

// IsMarketOpen returns true if the stock market is open at the given time.
// The close is 13:00 on recognized early-close sessions and 16:00 otherwise.
func IsMarketOpen(t time.Time) bool {
	session, ok := MarketSessionForDate(t)
	if !ok {
		return false
	}
	return !t.Before(session.Open) && t.Before(session.Close)
}

// MostRecentCompletedMarketSession returns the newest session whose
// close+delay is at or before now. It supports durable startup catch-up.
func MostRecentCompletedMarketSession(now time.Time, delay time.Duration) USMarketSession {
	et := now.In(nyLoc)
	for offset := 0; ; offset++ {
		candidate := et.AddDate(0, 0, -offset)
		session, ok := MarketSessionForDate(candidate)
		if ok && !now.Before(session.Close.Add(delay)) {
			return session
		}
	}
}

// NextMarketCloseRun returns the first future session close plus delay.
func NextMarketCloseRun(now time.Time, delay time.Duration) (USMarketSession, time.Time) {
	et := now.In(nyLoc)
	for offset := 0; ; offset++ {
		candidate := et.AddDate(0, 0, offset)
		session, ok := MarketSessionForDate(candidate)
		if !ok {
			continue
		}
		runAt := session.Close.Add(delay)
		if runAt.After(now) {
			return session, runAt
		}
	}
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
		if IsTradingDay(candidate) {
			return candidate
		}
		candidate = candidate.AddDate(0, 0, 1)
	}
}

// isEarlyClose covers the recurring NYSE 13:00 closes used by the scanner:
// the day after Thanksgiving, Christmas Eve when it is a trading day, and the
// exchange's Independence Day early-close session.
func isEarlyClose(t time.Time) bool {
	et := t.In(nyLoc)
	year := et.Year()
	thanksgiving := nthWeekday(year, time.November, time.Thursday, 4)
	if sameDate(et, thanksgiving.AddDate(0, 0, 1)) {
		return true
	}
	if et.Month() == time.December && et.Day() == 24 {
		return true
	}
	julyFourth := time.Date(year, time.July, 4, 0, 0, 0, 0, nyLoc)
	switch julyFourth.Weekday() {
	case time.Tuesday, time.Wednesday, time.Thursday, time.Friday:
		return et.Month() == time.July && et.Day() == 3
	default:
		return false
	}
}

func sameDate(left, right time.Time) bool {
	ly, lm, ld := left.In(nyLoc).Date()
	ry, rm, rd := right.In(nyLoc).Date()
	return ly == ry && lm == rm && ld == rd
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

	// A Monday observed holiday for a Sunday New Year falls on 01-02. We check
	// adjacent years for observed shifts, but NYSE does not close the preceding
	// Friday when New Year's Day itself falls on Saturday.
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

// isExceptionalNYSEClosure covers full-day NYSE closures that cannot be
// derived from the recurring holiday rules. Keep these explicit: unlike
// exchange holidays, national days of mourning and emergency closures do not
// follow a calendar rule that can safely predict future dates.
//
// The list covers the modern electronic-market era, including the September
// 11 attacks, presidential days of mourning, Hurricane Sandy, and the closures
// for Presidents George H. W. Bush and Jimmy Carter.
func isExceptionalNYSEClosure(t time.Time) bool {
	et := t.In(nyLoc)
	dateKey := et.Year()*10_000 + int(et.Month())*100 + et.Day()
	switch dateKey {
	case 20010911, // September 11 attacks
		20010912,
		20010913,
		20010914,
		20040611, // National Day of Mourning for President Ronald Reagan
		20070102, // National Day of Mourning for President Gerald Ford
		20121029, // Hurricane Sandy
		20121030,
		20181205, // National Day of Mourning for President George H. W. Bush
		20250109: // National Day of Mourning for President Jimmy Carter
		return true
	default:
		return false
	}
}

// usHolidays returns the observed NYSE holiday dates for a given year.
func usHolidays(year int) []time.Time {
	h := []time.Time{
		observeNewYearsDay(year),                                              // New Year's Day
		nthWeekday(year, time.January, time.Monday, 3),                        // MLK Day (3rd Mon Jan)
		nthWeekday(year, time.February, time.Monday, 3),                       // Presidents' Day (3rd Mon Feb)
		goodFriday(year),                                                      // Good Friday
		lastWeekday(year, time.May, time.Monday),                              // Memorial Day (last Mon May)
		observeHoliday(time.Date(year, time.June, 19, 0, 0, 0, 0, nyLoc)),     // Juneteenth
		observeHoliday(time.Date(year, time.July, 4, 0, 0, 0, 0, nyLoc)),      // Independence Day
		nthWeekday(year, time.September, time.Monday, 1),                      // Labor Day (1st Mon Sep)
		nthWeekday(year, time.November, time.Thursday, 4),                     // Thanksgiving (4th Thu Nov)
		observeHoliday(time.Date(year, time.December, 25, 0, 0, 0, 0, nyLoc)), // Christmas
	}
	return h
}

func observeNewYearsDay(year int) time.Time {
	day := time.Date(year, time.January, 1, 0, 0, 0, 0, nyLoc)
	if day.Weekday() == time.Sunday {
		return day.AddDate(0, 0, 1)
	}
	// A Saturday is already closed as a weekend. Unlike the generic federal
	// observation rule, NYSE remains open on the preceding Friday.
	return day
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
