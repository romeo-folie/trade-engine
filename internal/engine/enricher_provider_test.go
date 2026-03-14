package engine

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
)

// stubAnalyzer wraps an analysis.Analyzer and exposes only what
// resolveEnricherProvider / pickExtractionProvider need.
func makeAnalyzerWithProviders(cfgs []config.ModelConfig) *analysis.Analyzer {
	mc := config.ModelsConfig{}
	mc.Analysts = cfgs
	return analysis.NewAnalyzer(mc)
}

// TestResolveEnricherProvider_RegistryPath verifies that when
// models.roles.news_enricher is configured in the registry, resolveEnricherProvider
// returns a provider built from that role — not from the analysts list.
// This is the fix for the bug where extraction always searched analysts,
// making it impossible to use a dedicated enricher model.
func TestResolveEnricherProvider_RegistryPath(t *testing.T) {
	// Dummy keys so IsAvailable() returns true without real credentials.
	t.Setenv("OPENAI_API_KEY", "test-openai-key")
	t.Setenv("ANTHROPIC_API_KEY", "test-anthropic-key")

	cfg := config.Defaults()

	// Analyst list contains only an Anthropic analyst — no OpenAI model.
	cfg.Models.Analysts = []config.ModelConfig{
		{Provider: "anthropic", Model: "claude-sonnet-4-6", APIKeyEnv: "ANTHROPIC_API_KEY", MaxTokens: 2048, TimeoutSec: 60},
	}

	// news_enricher role points to OpenAI gpt-4o-mini via registry — NOT in analysts list.
	cfg.Models.Registry = config.ModelRegistry{
		"news_enricher": {
			ModelConfig: config.ModelConfig{
				Provider:   "openai",
				Model:      "gpt-4o-mini",
				APIKeyEnv:  "OPENAI_API_KEY",
				MaxTokens:  256,
				TimeoutSec: 15,
			},
		},
	}
	cfg.Models.Roles = config.ModelRoles{
		NewsEnricher: "news_enricher",
	}

	az := analysis.NewAnalyzer(cfg.Models)
	p := resolveEnricherProvider(az, cfg)

	if p == nil {
		t.Fatal("expected a non-nil provider from registry news_enricher role, got nil")
	}
	if p.ProviderName() != "openai" {
		t.Errorf("provider name = %q, want %q", p.ProviderName(), "openai")
	}
	if p.ModelName() != "gpt-4o-mini" {
		t.Errorf("model name = %q, want %q", p.ModelName(), "gpt-4o-mini")
	}
}

// TestResolveEnricherProvider_LegacyFallback verifies that when no
// models.roles.news_enricher is configured, resolveEnricherProvider falls back
// to scanning analysts using the legacy extraction_provider / extraction_model fields.
func TestResolveEnricherProvider_LegacyFallback(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-openai-key")

	cfg := config.Defaults()

	// No registry / roles configured.
	cfg.Models.Registry = nil
	cfg.Models.Roles = config.ModelRoles{}

	// Analyst matches the legacy extraction fields.
	cfg.Models.Analysts = []config.ModelConfig{
		{Provider: "openai", Model: "gpt-4o", APIKeyEnv: "OPENAI_API_KEY", MaxTokens: 2048, TimeoutSec: 60},
	}
	cfg.Markets.News.ExtractionProvider = "openai"
	cfg.Markets.News.ExtractionModel = "gpt-4o"

	az := analysis.NewAnalyzer(cfg.Models)
	p := resolveEnricherProvider(az, cfg)

	if p == nil {
		t.Fatal("expected a non-nil provider via legacy analyst scan, got nil")
	}
	if p.ProviderName() != "openai" {
		t.Errorf("provider name = %q, want %q", p.ProviderName(), "openai")
	}
	if p.ModelName() != "gpt-4o" {
		t.Errorf("model name = %q, want %q", p.ModelName(), "gpt-4o")
	}
}

// TestResolveEnricherProvider_NoMatchReturnsNil verifies that when the
// legacy extraction_model does not match any analyst and no registry role is
// set, resolveEnricherProvider returns nil (no silent fallback to wrong model).
func TestResolveEnricherProvider_NoMatchReturnsNil(t *testing.T) {
	cfg := config.Defaults()
	cfg.Models.Registry = nil
	cfg.Models.Roles = config.ModelRoles{}

	// Analyst is Anthropic; extraction_provider requests OpenAI — no match.
	cfg.Models.Analysts = []config.ModelConfig{
		{Provider: "anthropic", Model: "claude-sonnet-4-6", APIKeyEnv: "ANTHROPIC_API_KEY", MaxTokens: 2048, TimeoutSec: 60},
	}
	cfg.Markets.News.ExtractionProvider = "openai"
	cfg.Markets.News.ExtractionModel = "gpt-4o-mini"

	az := analysis.NewAnalyzer(cfg.Models)
	p := resolveEnricherProvider(az, cfg)

	if p != nil {
		t.Errorf("expected nil when no matching provider, got %s/%s", p.ProviderName(), p.ModelName())
	}
}
