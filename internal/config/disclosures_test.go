package config

import (
	"strings"
	"testing"
)

func TestDisclosureDefaultsAreObservationOff(t *testing.T) {
	cfg := Defaults()
	if cfg.Disclosures.Enabled || cfg.Scanner.PoliticalDisclosures.Enabled ||
		cfg.Scanner.PoliticalDisclosures.PromoteCandidates {
		t.Fatal("congressional disclosure ingestion and promotion must default off")
	}
	if !cfg.Disclosures.House.Enabled || cfg.Disclosures.Senate.Enabled {
		t.Fatalf("unexpected source defaults: %+v", cfg.Disclosures)
	}
	if cfg.Disclosures.Senate.Transport != "browser" {
		t.Fatalf("Senate transport = %q", cfg.Disclosures.Senate.Transport)
	}
	if !cfg.Disclosures.OGE.Enabled {
		t.Fatal("OGE source should be enabled by default when disclosures are enabled")
	}
}

func TestValidateSenateDisclosureTransport(t *testing.T) {
	cfg := Defaults()
	cfg.Disclosures.Enabled = true
	cfg.Disclosures.AcknowledgeUseRestrictions = true
	cfg.Disclosures.Senate.Enabled = true
	cfg.Disclosures.Senate.AcknowledgeUseRestrictions = true
	cfg.Disclosures.Senate.Transport = "carrier-pigeon"
	if errors := Validate(cfg); !containsConfigError(errors, "senate.transport") {
		t.Fatalf("errors = %v", errors)
	}
}

func TestValidateSenateDisclosureAcknowledgement(t *testing.T) {
	cfg := Defaults()
	cfg.Disclosures.Enabled = true
	cfg.Disclosures.AcknowledgeUseRestrictions = true
	cfg.Disclosures.Senate.Enabled = true
	errors := Validate(cfg)
	if !containsConfigError(errors, "acknowledge_use_restrictions") {
		t.Fatalf("errors = %v", errors)
	}
	cfg.Disclosures.Senate.AcknowledgeUseRestrictions = true
	if errors := Validate(cfg); containsConfigError(errors, "disclosures") {
		t.Fatalf("unexpected disclosure validation errors: %v", errors)
	}
}

func TestValidateDisclosureBudgetCoversEnabledSources(t *testing.T) {
	cfg := Defaults()
	cfg.Disclosures.Enabled = true
	cfg.Disclosures.AcknowledgeUseRestrictions = true
	cfg.Disclosures.MaxReportsPerSync = 1
	if errors := Validate(cfg); !containsConfigError(errors, "max_reports_per_sync") {
		t.Fatalf("errors = %v", errors)
	}
}

func TestValidateDisclosuresConfigReportsInvalidEnabledSettingsDeterministically(t *testing.T) {
	cfg := Defaults().Disclosures
	cfg.Enabled = true
	cfg.AcknowledgeUseRestrictions = false
	cfg.RequestTimeoutSeconds = 0
	cfg.House.BaseURL = ""
	cfg.House.RequestDelayMilliseconds = -1
	got := ValidateDisclosuresConfig(cfg)
	want := []string{
		"disclosures.acknowledge_use_restrictions must be true",
		"disclosures.request_timeout_seconds must be a safe positive duration",
		"disclosures.house.base_url must not be blank",
		"disclosures.house.request_delay_milliseconds must be >= 0",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("errors=%v, want %v", got, want)
	}
}

func TestValidateDisclosuresConfigRejectsOverflowingRequestDelay(t *testing.T) {
	cfg := Defaults().Disclosures
	cfg.Enabled = true
	cfg.AcknowledgeUseRestrictions = true
	cfg.Senate.RequestDelayMilliseconds = int(^uint(0) >> 1)
	got := ValidateDisclosuresConfig(cfg)
	if !containsConfigError(got, "disclosures.senate.request_delay_milliseconds must be a safe nonnegative duration") {
		t.Fatalf("errors=%v", got)
	}
}

func containsConfigError(errors []string, part string) bool {
	for _, err := range errors {
		if strings.Contains(err, part) {
			return true
		}
	}
	return false
}
