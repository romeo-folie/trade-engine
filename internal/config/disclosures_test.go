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

func TestPoliticalDisclosureTimingSplitFallsBackForLegacyConfigAndValidatesExplicitValues(t *testing.T) {
	legacy := PoliticalDisclosureScannerConfig{TTLDays: 7}
	if legacy.TriggerWindowDays() != 7 || legacy.EvidenceHorizonDays() != 7 || legacy.WatchlistLeaseWindowDays() != 7 {
		t.Fatalf("legacy timing fallback = %+v", legacy)
	}
	if Defaults().Scanner.PoliticalDisclosures.WatchlistLeaseWindowSessions() != 3 {
		t.Fatal("new defaults should use a three-session political-disclosure lease")
	}
	loaded := Defaults()
	loaded.Scanner.PoliticalDisclosures.TTLDays = 9
	migratePoliticalDisclosureTimingConfig(loaded, []byte("scanner:\n  political_disclosures:\n    ttl_days: 9\n"))
	if got := loaded.Scanner.PoliticalDisclosures; got.TriggerLookbackDays != 9 ||
		got.AnalystEvidenceHorizonDays != 9 || got.WatchlistLeaseDays != 9 || got.WatchlistLeaseSessions != 0 {
		t.Fatalf("legacy config migration = %+v, want all timing values 9", got)
	}
	sessionConfig := Defaults()
	sessionConfig.Scanner.PoliticalDisclosures.WatchlistLeaseSessions = 3
	migratePoliticalDisclosureTimingConfig(sessionConfig, []byte("scanner:\n  political_disclosures:\n    watchlist_lease_sessions: 3\n"))
	if got := sessionConfig.Scanner.PoliticalDisclosures; got.WatchlistLeaseWindowSessions() != 3 {
		t.Fatalf("explicit session lease = %+v, want three sessions", got)
	}
	cfg := Defaults()
	cfg.Scanner.Enabled = true
	cfg.Disclosures.Enabled = true
	cfg.Disclosures.AcknowledgeUseRestrictions = true
	cfg.Scanner.PoliticalDisclosures.Enabled = true
	cfg.Scanner.PoliticalDisclosures.TriggerLookbackDays = -1
	cfg.Scanner.PoliticalDisclosures.AnalystEvidenceHorizonDays = -1
	cfg.Scanner.PoliticalDisclosures.WatchlistLeaseDays = -1
	errs := Validate(cfg)
	for _, field := range []string{"trigger_lookback_days", "analyst_evidence_horizon_days", "watchlist_lease_days"} {
		if !containsConfigError(errs, field) {
			t.Fatalf("validation errors %v did not include %s", errs, field)
		}
	}
	explicitZero := Defaults()
	explicitZero.Scanner.Enabled = true
	explicitZero.Disclosures.Enabled = true
	explicitZero.Disclosures.AcknowledgeUseRestrictions = true
	explicitZero.Scanner.PoliticalDisclosures.Enabled = true
	explicitZero.Scanner.PoliticalDisclosures.TTLDays = 7
	explicitZero.Scanner.PoliticalDisclosures.TriggerLookbackDays = 0
	explicitZero.Scanner.PoliticalDisclosures.AnalystEvidenceHorizonDays = 0
	explicitZero.Scanner.PoliticalDisclosures.WatchlistLeaseDays = 0
	migratePoliticalDisclosureTimingConfig(explicitZero, []byte("scanner:\n  political_disclosures:\n    ttl_days: 7\n    trigger_lookback_days: 0\n    analyst_evidence_horizon_days: 0\n    watchlist_lease_days: 0\n"))
	for _, field := range []string{"trigger_lookback_days", "analyst_evidence_horizon_days", "watchlist_lease_days"} {
		if !containsConfigError(Validate(explicitZero), field) {
			t.Fatalf("explicit zero validation errors %v did not include %s", Validate(explicitZero), field)
		}
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
