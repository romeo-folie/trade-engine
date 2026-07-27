package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// PotentIndustryConfigEvidence returns the complete operator-visible snapshot
// and a compatibility hash containing only fields that can change evaluation
// output. Operational rollout, batching, promotion, and lease controls
// deliberately do not split transition/persistence history. The after-close
// delay does split history because it changes the taxonomy decision cutoff.
func PotentIndustryConfigEvidence(
	cfg PotentIndustryConfig, featureVersion string,
) (json.RawMessage, string, error) {
	evidence := struct {
		FeatureVersion string               `json:"feature_version"`
		Config         PotentIndustryConfig `json:"config"`
	}{FeatureVersion: featureVersion, Config: cfg}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return nil, "", fmt.Errorf("marshal potent industry config: %w", err)
	}

	compatibility := struct {
		FeatureVersion             string            `json:"feature_version"`
		AfterCloseDelayMinutes     int               `json:"after_close_delay_minutes"`
		TaxonomyMaxAgeHours        int               `json:"taxonomy_max_age_hours"`
		HistoryCalendarDays        int               `json:"history_calendar_days"`
		MinHistorySessions         int               `json:"min_history_sessions"`
		MarketBenchmark            string            `json:"market_benchmark"`
		SectorBenchmarks           map[string]string `json:"sector_benchmarks"`
		IndustryBenchmarkOverrides map[string]string `json:"industry_benchmark_overrides"`
		MinFreshTaxonomyCoverage   float64           `json:"min_fresh_taxonomy_coverage"`
		MinGroupMembers            int               `json:"min_group_members"`
		MinGroupCoverage           float64           `json:"min_group_coverage"`
		MinPotencyScore            float64           `json:"min_potency_score"`
		MinMemberScore             float64           `json:"min_member_score"`
		SelectionPercentile        float64           `json:"selection_percentile"`
		MinAdvanceShare            float64           `json:"min_advance_share"`
		MinMedianReturn5D          float64           `json:"min_median_return_5d"`
		MaxCapEqualDivergence      float64           `json:"max_cap_equal_divergence"`
		MinPersistenceSessions     int               `json:"min_persistence_sessions"`
		MaxMembersPerGroup         int               `json:"max_members_per_group"`
	}{
		FeatureVersion:             strings.TrimSpace(featureVersion),
		AfterCloseDelayMinutes:     cfg.AfterCloseDelayMinutes,
		TaxonomyMaxAgeHours:        cfg.TaxonomyMaxAgeHours,
		HistoryCalendarDays:        cfg.HistoryCalendarDays,
		MinHistorySessions:         cfg.MinHistorySessions,
		MarketBenchmark:            strings.ToUpper(strings.TrimSpace(cfg.MarketBenchmark)),
		SectorBenchmarks:           normalizedBenchmarkMap(cfg.SectorBenchmarks),
		IndustryBenchmarkOverrides: normalizedBenchmarkMap(cfg.IndustryBenchmarkOverrides),
		MinFreshTaxonomyCoverage:   cfg.MinFreshTaxonomyCoverage,
		MinGroupMembers:            cfg.MinGroupMembers,
		MinGroupCoverage:           cfg.MinGroupCoverage,
		MinPotencyScore:            cfg.MinPotencyScore,
		MinMemberScore:             cfg.MinMemberScore,
		SelectionPercentile:        cfg.SelectionPercentile,
		MinAdvanceShare:            cfg.MinAdvanceShare,
		MinMedianReturn5D:          cfg.MinMedianReturn5D,
		MaxCapEqualDivergence:      cfg.MaxCapEqualDivergence,
		MinPersistenceSessions:     cfg.MinPersistenceSessions,
		MaxMembersPerGroup:         cfg.MaxMembersPerGroup,
	}
	compatibilityJSON, err := json.Marshal(compatibility)
	if err != nil {
		return nil, "", fmt.Errorf("marshal potent industry compatibility config: %w", err)
	}
	sum := sha256.Sum256(compatibilityJSON)
	return encoded, hex.EncodeToString(sum[:]), nil
}

func normalizedBenchmarkMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for group, benchmark := range values {
		out[normalizedBenchmarkLabel(group)] =
			strings.ToUpper(strings.TrimSpace(benchmark))
	}
	return out
}

func normalizedBenchmarkLabel(value string) string {
	return strings.ToUpper(strings.Join(strings.Fields(value), " "))
}
