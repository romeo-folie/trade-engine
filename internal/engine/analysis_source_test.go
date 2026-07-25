package engine

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
)

func TestAnalysisSourceNodeFollowsConfiguredVerdictPath(t *testing.T) {
	tests := []struct {
		name   string
		cfg    *config.Config
		result analysis.AnalyzeResult
		want   string
	}{
		{
			name: "single registry analyst",
			cfg: &config.Config{Models: config.ModelsConfig{
				Roles: config.ModelRoles{Analysts: []string{"primary"}},
			}},
			want: "analyst-primary",
		},
		{
			name: "ensemble fast path",
			cfg: &config.Config{Models: config.ModelsConfig{
				Consensus: config.ConsensusConfig{Enabled: true},
			}},
			result: analysis.AnalyzeResult{
				FastPathed: true,
				AnalystResults: []analysis.AnalystResult{
					{Model: "one"}, {Model: "two"},
				},
			},
			want: "consensus",
		},
		{
			name: "ensemble arbiter path",
			cfg: &config.Config{Models: config.ModelsConfig{
				Consensus: config.ConsensusConfig{Enabled: true},
			}},
			result: analysis.AnalyzeResult{
				AnalystResults: []analysis.AnalystResult{{Model: "one"}, {Model: "two"}},
				Arbiter:        &analysis.ArbiterResult{},
			},
			want: "arbiter_node",
		},
		{
			name: "specialist",
			cfg: &config.Config{Analysis: config.AnalysisConfig{
				SpecialistModeEnabled: true,
			}},
			want: "specialist-risk-manager",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := &Engine{cfg: tt.cfg}
			if got := engine.analysisSourceNode(tt.result); got != tt.want {
				t.Fatalf("analysisSourceNode() = %q, want %q", got, tt.want)
			}
		})
	}
}
