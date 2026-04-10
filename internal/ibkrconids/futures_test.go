package ibkrconids_test

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/ibkrconids"
)

func TestFrontMonthFor_Quarterly_RollMonths(t *testing.T) {
	tests := []struct {
		name  string
		month time.Month
		want  string
	}{
		{"March 1 is Q1 expiry",     time.March,     "202603"},
		{"June 1 is Q2 expiry",      time.June,      "202606"},
		{"September 1 is Q3 expiry", time.September, "202609"},
		{"December 1 is Q4 expiry",  time.December,  "202612"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tt := time.Date(2026, tc.month, 1, 9, 30, 0, 0, time.UTC)
			got := ibkrconids.FrontMonthFor(tt, "quarterly")
			if got != tc.want {
				t.Errorf("FrontMonthFor(%v, quarterly) = %q, want %q", tt.Format("2006-01"), got, tc.want)
			}
		})
	}
}

func TestFrontMonthFor_Quarterly_NonRollMonths(t *testing.T) {
	tests := []struct {
		name  string
		month time.Month
		want  string
	}{
		{"January maps to March",   time.January,  "202603"},
		{"February maps to March",  time.February, "202603"},
		{"April maps to June",      time.April,    "202606"},
		{"May maps to June",        time.May,      "202606"},
		{"July maps to September",  time.July,     "202609"},
		{"August maps to September",time.August,   "202609"},
		{"October maps to December",time.October,  "202612"},
		{"November maps to December",time.November,"202612"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tt := time.Date(2026, tc.month, 15, 12, 0, 0, 0, time.UTC) // mid-month
			got := ibkrconids.FrontMonthFor(tt, "quarterly")
			if got != tc.want {
				t.Errorf("FrontMonthFor(%v, quarterly) = %q, want %q", tt.Format("2006-01"), got, tc.want)
			}
		})
	}
}

func TestFrontMonthFor_Quarterly_YearRollover(t *testing.T) {
	// December 31 is still Dec expiry; Jan 1 of next year rolls to March.
	dec31 := time.Date(2026, time.December, 31, 23, 59, 0, 0, time.UTC)
	jan1 := time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)

	if got := ibkrconids.FrontMonthFor(dec31, "quarterly"); got != "202612" {
		t.Errorf("Dec 31: want 202612, got %q", got)
	}
	if got := ibkrconids.FrontMonthFor(jan1, "quarterly"); got != "202703" {
		t.Errorf("Jan 1 next year: want 202703, got %q", got)
	}
}

func TestFrontMonthFor_Monthly(t *testing.T) {
	tests := []struct {
		date time.Time
		want string
	}{
		{time.Date(2026, time.April, 9, 10, 0, 0, 0, time.UTC), "202604"},
		{time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC), "202601"},
		{time.Date(2026, time.December, 31, 0, 0, 0, 0, time.UTC), "202612"},
	}
	for _, tc := range tests {
		got := ibkrconids.FrontMonthFor(tc.date, "monthly")
		if got != tc.want {
			t.Errorf("FrontMonthFor(%v, monthly) = %q, want %q", tc.date.Format("2006-01"), got, tc.want)
		}
	}
}

func TestFrontMonthFor_UnknownCycle_FallsBackToMonthly(t *testing.T) {
	tt := time.Date(2026, time.April, 9, 10, 0, 0, 0, time.UTC)
	got := ibkrconids.FrontMonthFor(tt, "weekly") // unknown
	if got != "202604" {
		t.Errorf("unknown cycle: want 202604 (monthly fallback), got %q", got)
	}
}

func TestFrontMonthKey(t *testing.T) {
	tt := time.Date(2026, time.April, 9, 10, 0, 0, 0, time.UTC) // April → Q2 → June
	got := ibkrconids.FrontMonthKey("ES", "quarterly", tt)
	if got != "ES/202606" {
		t.Errorf("FrontMonthKey(ES, quarterly, Apr 2026) = %q, want %q", got, "ES/202606")
	}
}

func TestFuturesConids_CurrentFrontMonthPresent(t *testing.T) {
	// Smoke test: the map must contain entries for the instruments enabled by default.
	instruments := []struct{ symbol, cycle string }{
		{"ES", "quarterly"},
		{"NQ", "quarterly"},
		{"YM", "quarterly"},
		{"RTY", "quarterly"},
	}
	now := time.Now()
	for _, inst := range instruments {
		key := ibkrconids.FrontMonthKey(inst.symbol, inst.cycle, now)
		if _, ok := ibkrconids.FuturesConids[key]; !ok {
			t.Errorf("FuturesConids missing key %q — update the map for the current quarter", key)
		}
	}
}
