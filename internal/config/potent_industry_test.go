package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDefaultsPotentIndustryDisabled(t *testing.T) {
	got := Defaults().Scanner.PotentIndustry
	if got.Enabled || got.PromoteCandidates {
		t.Fatal("potent industry discovery and promotion must default off")
	}
	if got.MaxPromotionsPerRun != 20 || got.AfterCloseDelayMinutes != 15 ||
		got.UniverseMaxAgeHours != 36 || got.TaxonomyMaxAgeHours != 168 ||
		got.HistoryCalendarDays != 420 || got.MinHistorySessions != 253 ||
		got.MarketBenchmark != "SPY" || len(got.SectorBenchmarks) != 11 ||
		got.MinFreshTaxonomyCoverage != 1 || got.MinGroupMembers != 5 ||
		got.MinGroupCoverage != .8 || got.MinPotencyScore != .7 ||
		got.MinMemberScore != .65 || got.SelectionPercentile != .8 ||
		got.MinAdvanceShare != .6 ||
		got.MinMedianReturn5D != 0 || got.MaxCapEqualDivergence != .08 ||
		got.MinPersistenceSessions != 3 || got.MaxMembersPerGroup != 3 ||
		got.BatchSize != 200 ||
		got.RunLeaseMinutes != 60 {
		t.Fatalf("unexpected potent industry defaults: %+v", got)
	}
}

func TestValidatePotentIndustry(t *testing.T) {
	valid := Defaults()
	valid.Scanner.Enabled = true
	valid.Scanner.Universe.Enabled = true
	valid.Scanner.PotentIndustry.Enabled = true
	if errs := Validate(valid); containsStr(errs, "scanner.potent_industry") {
		t.Fatalf("valid potent industry config rejected: %v", errs)
	}

	disabled := Defaults()
	disabled.Scanner.PotentIndustry = PotentIndustryConfig{}
	if errs := Validate(disabled); containsStr(errs, "scanner.potent_industry") {
		t.Fatalf("disabled zero value must stay backward compatible: %v", errs)
	}

	tests := []struct {
		field string
		apply func(*Config)
	}{
		{"requires scanner.enabled", func(c *Config) { c.Scanner.Enabled = false }},
		{"requires scanner.universe.enabled", func(c *Config) { c.Scanner.Universe.Enabled = false }},
		{"after_close_delay_minutes", func(c *Config) { c.Scanner.PotentIndustry.AfterCloseDelayMinutes = -1 }},
		{"after_close_delay_minutes", func(c *Config) {
			c.Scanner.PotentIndustry.AfterCloseDelayMinutes =
				int((time.Duration(1<<63-1) / time.Minute) + 1)
		}},
		{"universe_max_age_hours", func(c *Config) { c.Scanner.PotentIndustry.UniverseMaxAgeHours = 0 }},
		{"taxonomy_max_age_hours", func(c *Config) { c.Scanner.PotentIndustry.TaxonomyMaxAgeHours = 0 }},
		{"history_calendar_days", func(c *Config) { c.Scanner.PotentIndustry.HistoryCalendarDays = 399 }},
		{"min_history_sessions", func(c *Config) { c.Scanner.PotentIndustry.MinHistorySessions = 252 }},
		{"market_benchmark", func(c *Config) { c.Scanner.PotentIndustry.MarketBenchmark = " " }},
		{"sector_benchmarks", func(c *Config) { c.Scanner.PotentIndustry.SectorBenchmarks = nil }},
		{"sector_benchmarks", func(c *Config) {
			c.Scanner.PotentIndustry.SectorBenchmarks = map[string]string{" Technology ": "XLK"}
		}},
		{"sector_benchmarks", func(c *Config) {
			c.Scanner.PotentIndustry.SectorBenchmarks = map[string]string{
				"Technology": "XLK", "technology": "QQQ",
			}
		}},
		{"industry_benchmark_overrides", func(c *Config) {
			c.Scanner.PotentIndustry.IndustryBenchmarkOverrides = map[string]string{"Software": " "}
		}},
		{"min_fresh_taxonomy_coverage", func(c *Config) { c.Scanner.PotentIndustry.MinFreshTaxonomyCoverage = 0 }},
		{"min_group_members", func(c *Config) { c.Scanner.PotentIndustry.MinGroupMembers = 0 }},
		{"min_group_coverage", func(c *Config) { c.Scanner.PotentIndustry.MinGroupCoverage = 0 }},
		{"min_potency_score", func(c *Config) { c.Scanner.PotentIndustry.MinPotencyScore = 1.1 }},
		{"min_member_score", func(c *Config) { c.Scanner.PotentIndustry.MinMemberScore = 0 }},
		{"selection_percentile", func(c *Config) { c.Scanner.PotentIndustry.SelectionPercentile = 1 }},
		{"min_advance_share", func(c *Config) { c.Scanner.PotentIndustry.MinAdvanceShare = 0 }},
		{"min_median_return_5d", func(c *Config) { c.Scanner.PotentIndustry.MinMedianReturn5D = -1.1 }},
		{"max_cap_equal_divergence", func(c *Config) { c.Scanner.PotentIndustry.MaxCapEqualDivergence = 0 }},
		{"min_persistence_sessions", func(c *Config) { c.Scanner.PotentIndustry.MinPersistenceSessions = 0 }},
		{"min_persistence_sessions", func(c *Config) { c.Scanner.PotentIndustry.MinPersistenceSessions = 6 }},
		{"max_members_per_group", func(c *Config) { c.Scanner.PotentIndustry.MaxMembersPerGroup = 0 }},
		{"max_promotions_per_run", func(c *Config) { c.Scanner.PotentIndustry.MaxPromotionsPerRun = 0 }},
		{"batch_size", func(c *Config) { c.Scanner.PotentIndustry.BatchSize = 0 }},
		{"run_lease_minutes", func(c *Config) { c.Scanner.PotentIndustry.RunLeaseMinutes = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			cfg := *valid
			tt.apply(&cfg)
			if !containsStr(Validate(&cfg), "scanner.potent_industry."+tt.field) &&
				!containsStr(Validate(&cfg), "scanner.potent_industry "+tt.field) {
				t.Fatalf("expected %s validation error, got %v", tt.field, Validate(&cfg))
			}
		})
	}

	promotion := *valid
	promotion.Scanner.PotentIndustry.PromoteCandidates = true
	if errs := Validate(&promotion); containsStr(errs, "scanner.potent_industry") {
		t.Fatalf("enabled potent industry promotion rejected: %v", errs)
	}
}

func TestPotentIndustryCompatibilityEvidence(t *testing.T) {
	base := Defaults().Scanner.PotentIndustry
	snapshot, baseHash, err := PotentIndustryConfigEvidence(base, "potent-industry-v1")
	if err != nil {
		t.Fatal(err)
	}
	var evidence map[string]any
	if err := json.Unmarshal(snapshot, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence["feature_version"] != "potent-industry-v1" {
		t.Fatalf("snapshot omitted feature version: %s", snapshot)
	}

	operational := base
	operational.Enabled = true
	operational.PromoteCandidates = true
	operational.MaxPromotionsPerRun++
	operational.UniverseMaxAgeHours++
	operational.BatchSize++
	operational.RunLeaseMinutes++
	if _, got, err := PotentIndustryConfigEvidence(
		operational, "potent-industry-v1",
	); err != nil || got != baseHash {
		t.Fatalf("operational settings split compatibility: hash=%s err=%v", got, err)
	}

	timing := base
	timing.AfterCloseDelayMinutes++
	if _, got, err := PotentIndustryConfigEvidence(
		timing, "potent-industry-v1",
	); err != nil || got == baseHash {
		t.Fatalf("decision cutoff did not split compatibility: hash=%s err=%v", got, err)
	}

	scoring := base
	scoring.MinAdvanceShare += .01
	if _, got, err := PotentIndustryConfigEvidence(
		scoring, "potent-industry-v1",
	); err != nil || got == baseHash {
		t.Fatalf("scoring setting did not split compatibility: hash=%s err=%v", got, err)
	}

	mapping := base
	mapping.SectorBenchmarks = map[string]string{"Technology": " qqq "}
	_, normalizedHash, err := PotentIndustryConfigEvidence(
		mapping, "potent-industry-v1",
	)
	if err != nil {
		t.Fatal(err)
	}
	mapping.SectorBenchmarks["Technology"] = strings.ToUpper(
		strings.TrimSpace(mapping.SectorBenchmarks["Technology"]),
	)
	if _, got, err := PotentIndustryConfigEvidence(
		mapping, "potent-industry-v1",
	); err != nil || got != normalizedHash {
		t.Fatalf("benchmark normalization is unstable: hash=%s err=%v", got, err)
	}

	mapping.SectorBenchmarks = map[string]string{" technology ": "QQQ"}
	_, caseHash, err := PotentIndustryConfigEvidence(
		mapping, "potent-industry-v1",
	)
	if err != nil {
		t.Fatal(err)
	}
	mapping.SectorBenchmarks = map[string]string{"TECHNOLOGY": "QQQ"}
	if _, got, err := PotentIndustryConfigEvidence(
		mapping, "potent-industry-v1",
	); err != nil || got != caseHash {
		t.Fatalf("equivalent group labels split compatibility: hash=%s err=%v", got, err)
	}
}
