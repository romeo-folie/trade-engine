package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
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

func TestPrepareDisclosureSyncConfigAppliesBudgetBeforeValidation(t *testing.T) {
	for _, test := range []struct {
		name             string
		source           string
		limit            int
		wantHouse        bool
		wantSenate       bool
		wantOGE          bool
		wantValidationOK bool
	}{
		{name: "all sources", source: "all", limit: 3, wantHouse: true, wantSenate: true, wantOGE: true, wantValidationOK: true},
		{name: "all sources insufficient", source: "all", limit: 2, wantHouse: true, wantSenate: true, wantOGE: true},
		{name: "only OGE", source: "oge", limit: 1, wantOGE: true, wantValidationOK: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := prepareDisclosureSyncConfig(config.Defaults().Disclosures, test.source, test.limit)
			if cfg.MaxReportsPerSync != test.limit || cfg.House.Enabled != test.wantHouse ||
				cfg.Senate.Enabled != test.wantSenate || cfg.OGE.Enabled != test.wantOGE ||
				!cfg.Enabled || !cfg.AcknowledgeUseRestrictions ||
				cfg.Senate.AcknowledgeUseRestrictions != test.wantSenate {
				t.Fatalf("prepared config = %+v", cfg)
			}
			valid := len(config.ValidateDisclosuresConfig(cfg)) == 0
			if valid != test.wantValidationOK {
				t.Fatalf("validation=%v, want valid=%v", config.ValidateDisclosuresConfig(cfg), test.wantValidationOK)
			}
		})
	}
}
