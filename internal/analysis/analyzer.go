package analysis

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
)

// Analyzer wraps the configured primary model and exposes a single Analyze call.
// Consensus (multi-model) is deferred to Phase 12.
type Analyzer struct {
	primary   ModelProvider
	secondary []ModelProvider
}

// NewAnalyzer constructs an Analyzer from the models config section.
// Unknown or unimplemented providers are registered as stubs so the config
// stays valid without crashing the engine.
func NewAnalyzer(cfg config.ModelsConfig) *Analyzer {
	return &Analyzer{
		primary:   buildProvider(cfg.Primary),
		secondary: buildProviders(cfg.Secondary),
	}
}

// Primary returns the primary ModelProvider.
func (a *Analyzer) Primary() ModelProvider { return a.primary }

// Providers returns all configured providers (primary + secondary).
func (a *Analyzer) Providers() []ModelProvider {
	all := make([]ModelProvider, 0, 1+len(a.secondary))
	all = append(all, a.primary)
	all = append(all, a.secondary...)
	return all
}

// Analyze dispatches to the primary model. If the primary is unavailable,
// it returns an error immediately rather than silently holding.
func (a *Analyzer) Analyze(ctx context.Context, req AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
	if !a.primary.IsAvailable() {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf(
			"primary model %s/%s is not available (check API key config)",
			a.primary.ProviderName(), a.primary.ModelName(),
		)
	}
	resp, meta, err := a.primary.Analyze(ctx, req)
	if err != nil {
		logger.Warn("primary model analysis failed",
			"ticker", req.Ticker,
			"provider", a.primary.ProviderName(),
			"model", a.primary.ModelName(),
			"err", err.Error(),
		)
	}
	return resp, meta, err
}

// ─── Factory ──────────────────────────────────────────────────────────────────

func buildProviders(cfgs []config.ModelConfig) []ModelProvider {
	out := make([]ModelProvider, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, buildProvider(c))
	}
	return out
}

func buildProvider(cfg config.ModelConfig) ModelProvider {
	switch cfg.Provider {
	case "anthropic":
		return NewAnthropicClient(cfg)
	case "openai":
		return NewOpenAIClient(cfg)
	case "ollama":
		return NewOllamaClient(cfg)
	default:
		logger.Warn("unknown model provider — registering as unavailable stub", "provider", cfg.Provider)
		return NewOpenAIClient(cfg) // generic unavailable stub
	}
}

// hashPrompt computes a SHA-256 hex digest of the raw prompt string.
func hashPrompt(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return fmt.Sprintf("%x", sum)
}
