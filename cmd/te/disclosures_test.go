package main

import (
	"testing"
	"time"
)

func TestDisclosureDateRange(t *testing.T) {
	from, to, err := disclosureDateRange(
		time.Date(2026, time.August, 8, 18, 0, 0, 0, time.UTC),
		"2026-06-01", "2026-08-08",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := from.Format("2006-01-02"); got != "2026-06-01" {
		t.Fatalf("from = %s", got)
	}
	if got := to.Format("2006-01-02"); got != "2026-08-08" {
		t.Fatalf("to = %s", got)
	}
	if _, _, err := disclosureDateRange(time.Now(), "2026-08-09", "2026-08-08"); err == nil {
		t.Fatal("expected reversed date range to fail")
	}
}

func TestValidDisclosureStatus(t *testing.T) {
	for _, status := range []string{"pending", "completed", "failed", "no_text"} {
		if !validDisclosureStatus(status) {
			t.Fatalf("status %q rejected", status)
		}
	}
	if validDisclosureStatus("processing") {
		t.Fatal("invalid report status accepted")
	}
}
