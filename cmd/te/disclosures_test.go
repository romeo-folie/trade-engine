package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
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

func TestDisclosureStatusLabelShowsRetryDisposition(t *testing.T) {
	next := time.Date(2026, 8, 17, 18, 0, 0, 0, time.UTC)
	if got, want := disclosureStatusLabel(store.PoliticalDisclosureReportSummary{
		ExtractionStatus: "failed", RetryableFailure: true, RetryAttemptCount: 1, NextRetryAt: &next,
	}), fmt.Sprintf("failed (retry 1/%d after %s)", store.MaxPoliticalDisclosureRetryAttempts, next.Format(time.RFC3339)); got != want {
		t.Fatalf("retry status = %q, want %q", got, want)
	}
	if got, want := disclosureStatusLabel(store.PoliticalDisclosureReportSummary{
		ExtractionStatus: "failed", RetryAttemptCount: store.MaxPoliticalDisclosureRetryAttempts,
	}), fmt.Sprintf("failed (manual retry; %d attempts)", store.MaxPoliticalDisclosureRetryAttempts); got != want {
		t.Fatalf("manual retry status = %q, want %q", got, want)
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
