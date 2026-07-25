package analysis

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis/depth"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/logger"
)

// Analyzer wraps configured analyst models and optional arbiter (Phase 12).
// In single-analyst mode it behaves identically to the Phase 3 design.
type Analyzer struct {
	analysts []ModelProvider
	arbiter  ModelProvider
	cfg      config.ModelsConfig
	bus      *events.Bus

	// specialist is the optional sequential specialist sub-config.
	// Wired by the engine via SetSpecialistConfig when
	// analysis.specialist_mode_enabled is true. Nil means specialist mode is
	// off and AnalyzeSpecialist will refuse to run.
	specialist *config.AnalysisConfig
}

// NewAnalyzer constructs an Analyzer from the models config section.
// Uses registry roles when present (models.registry + models.roles.analysts).
// Falls back to legacy cfg.Analysts[], then cfg.Primary.
func NewAnalyzer(cfg config.ModelsConfig, bus *events.Bus) *Analyzer {
	var analysts []ModelProvider
	// Registry path: AnalystConfigs() transparently returns registry entries
	// when available, falling back to legacy Analysts[] otherwise.
	for _, ac := range cfg.AnalystConfigs() {
		analysts = append(analysts, buildProvider(ac))
	}
	// Legacy fallback: old primary/secondary config.
	if len(analysts) == 0 && cfg.Primary.Provider != "" {
		analysts = append(analysts, buildProvider(cfg.Primary))
		for _, sc := range cfg.Secondary {
			analysts = append(analysts, buildProvider(sc))
		}
	}
	var arbiter ModelProvider
	// ArbiterConfig() transparently returns registry entry when available.
	if ac := cfg.ArbiterConfig(); ac.Provider != "" {
		arbiter = buildProvider(ac)
	}
	return &Analyzer{analysts: analysts, arbiter: arbiter, cfg: cfg, bus: bus}
}

// Primary returns the first configured analyst for backward compat.
func (a *Analyzer) Primary() ModelProvider {
	if len(a.analysts) == 0 {
		return nil
	}
	return a.analysts[0]
}

// Providers returns all configured analyst providers.
func (a *Analyzer) Providers() []ModelProvider {
	out := make([]ModelProvider, len(a.analysts))
	copy(out, a.analysts)
	return out
}

// Arbiter returns the configured arbiter provider (may be nil).
func (a *Analyzer) Arbiter() ModelProvider { return a.arbiter }

// Bus returns the event bus used by this analyzer.
func (a *Analyzer) Bus() *events.Bus { return a.bus }

// Analyze is the primary engine entry-point. Routes to single or ensemble mode.
func (a *Analyzer) Analyze(ctx context.Context, req AnalysisRequest, impact depth.ImpactScore) (AnalysisResponse, ModelCallMeta, error) {
	res, err := a.AnalyzeFull(ctx, req, impact)
	return res.Response, res.Meta, err
}

// AnalyzeFull executes either a single-model analysis or a full ensemble
// based on consensus config.
func (a *Analyzer) AnalyzeFull(ctx context.Context, req AnalysisRequest, impact depth.ImpactScore) (AnalyzeResult, error) {
	if !a.cfg.Consensus.Enabled || len(a.analysts) <= 1 {
		if a.bus != nil {
			selected := []string{"llm_analysis"}
			if len(a.cfg.Roles.Analysts) > 0 {
				selected[0] = "analyst-" + a.cfg.Roles.Analysts[0]
			}
			a.bus.Publish(events.AnalysisStarted, events.AnalysisStartedPayload{
				Ticker: req.Ticker, Depth: 1, SelectedAnalysts: selected,
			})
		}
		resp, meta, err := a.analyzeSingle(ctx, req)
		single := AnalystResult{Provider: meta.Provider, Model: meta.Model, Response: resp, Meta: meta, Err: err, LatencyMs: meta.LatencyMs}
		return AnalyzeResult{Response: resp, Meta: meta, AnalystResults: []AnalystResult{single}}, err
	}

	// Filter analysts based on adaptive depth (when enabled).
	activeAnalysts := a.analysts
	if impact.Depth > 0 && impact.Depth < len(a.analysts) {
		activeAnalysts = a.analysts[:impact.Depth]
	}

	// Depth-1 bypass: single analyst cannot satisfy ensemble quorum — use single-analyst path.
	if len(activeAnalysts) == 1 {
		resp, meta, err := activeAnalysts[0].Analyze(ctx, req)
		single := AnalystResult{
			Provider: meta.Provider, Model: meta.Model,
			Response: resp, Meta: meta, Err: err, LatencyMs: meta.LatencyMs,
		}
		if a.bus != nil {
			selected := []string{"llm_analysis"}
			if len(a.cfg.Roles.Analysts) > 0 {
				selected = []string{"analyst-" + a.cfg.Roles.Analysts[0]}
			}
			a.bus.Publish(events.AnalysisStarted, events.AnalysisStartedPayload{
				Ticker:           req.Ticker,
				Depth:            impact.Depth,
				SelectedAnalysts: selected,
			})
		}
		return AnalyzeResult{Response: resp, Meta: meta, AnalystResults: []AnalystResult{single}}, err
	}

	// Emit AnalysisStarted for visualization (depth ≥ 2).
	if a.bus != nil {
		selected := make([]string, len(activeAnalysts))
		for i := range activeAnalysts {
			if i < len(a.cfg.Roles.Analysts) {
				selected[i] = "analyst-" + a.cfg.Roles.Analysts[i]
			} else {
				selected[i] = fmt.Sprintf("analyst-%d", i)
			}
		}
		a.bus.Publish(events.AnalysisStarted, events.AnalysisStartedPayload{
			Ticker:           req.Ticker,
			Depth:            impact.Depth,
			SelectedAnalysts: selected,
		})
	}

	results, err := a.AnalyzeEnsembleWithSet(ctx, req, activeAnalysts)
	if err != nil {
		return AnalyzeResult{AnalystResults: results}, err
	}

	fpConf := a.cfg.Consensus.FastPathConfidence
	if fpConf == 0 {
		fpConf = 0.80
	}
	fpSpread := a.cfg.Consensus.FastPathMaxSpread
	if fpSpread == 0 {
		fpSpread = 0.15
	}

	if resp, ok := TryFastPath(results, fpConf, fpSpread); ok {
		logger.Info("ensemble fast-path hit", "ticker", req.Ticker, "action", string(resp.Action))
		meta := primaryMeta(results)
		meta.RawResponse = "[fast-path: unanimous analyst agreement]"
		return AnalyzeResult{Response: resp, Meta: meta, AnalystResults: results, FastPathed: true}, nil
	}

	if a.arbiter == nil {
		logger.Warn("no arbiter configured — falling back to majority vote", "ticker", req.Ticker)
		resp, ok := majorityVote(results)
		if !ok {
			return AnalyzeResult{}, fmt.Errorf("ensemble: no majority among %d analysts", len(results))
		}
		return AnalyzeResult{Response: resp, Meta: primaryMeta(results), AnalystResults: results}, nil
	}

	arbResult, err := Arbitrate(ctx, a.arbiter, req, results)
	if err != nil {
		logger.Warn("arbiter failed — falling back to majority vote", "ticker", req.Ticker, "err", err)
		resp, ok := majorityVote(results)
		if !ok {
			return AnalyzeResult{}, fmt.Errorf("ensemble: arbiter failed and no majority: %w", err)
		}
		return AnalyzeResult{Response: resp, Meta: primaryMeta(results), AnalystResults: results}, nil
	}

	logger.Info("ensemble arbitrated", "ticker", req.Ticker, "action", string(arbResult.Final.Action), "dissent", arbResult.DissentSummary != "")
	return AnalyzeResult{Response: arbResult.Final, Meta: arbResult.Meta, AnalystResults: results, Arbiter: &arbResult}, nil
}

func (a *Analyzer) analyzeSingle(ctx context.Context, req AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
	primary := a.Primary()
	if primary == nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("no analysts configured")
	}
	if !primary.IsAvailable() {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("primary analyst %s/%s is not available", primary.ProviderName(), primary.ModelName())
	}
	resp, meta, err := primary.Analyze(ctx, req)
	if err != nil {
		logger.Warn("primary analyst failed", "ticker", req.Ticker, "provider", primary.ProviderName(), "model", primary.ModelName(), "err", err)
	}
	return resp, meta, err
}

// ─── 12.3: Parallel analyst fan-out ──────────────────────────────────────────

// AnalyzeEnsemble fans out to all configured analysts in parallel.
// Requires at least 2 successful responses; returns error otherwise.
func (a *Analyzer) AnalyzeEnsemble(ctx context.Context, req AnalysisRequest) ([]AnalystResult, error) {
	return a.AnalyzeEnsembleWithSet(ctx, req, a.analysts)
}

// AnalyzeEnsembleWithSet fans out to a specific set of analysts in parallel.
func (a *Analyzer) AnalyzeEnsembleWithSet(ctx context.Context, req AnalysisRequest, analysts []ModelProvider) ([]AnalystResult, error) {
	available := availableProviders(analysts)
	if len(available) == 0 {
		return nil, fmt.Errorf("ensemble: no analysts are available (check API keys)")
	}
	results := a.runAnalystSet(ctx, req, available)
	if n := len(successfulResults(results)); n < 2 {
		return results, fmt.Errorf("ensemble: only %d/%d analysts succeeded (minimum 2 required)", n, len(available))
	}
	return results, nil
}

// availableProviders filters to the configured+ready providers.
func availableProviders(analysts []ModelProvider) []ModelProvider {
	available := make([]ModelProvider, 0, len(analysts))
	for _, p := range analysts {
		if p.IsAvailable() {
			available = append(available, p)
		}
	}
	return available
}

// runAnalystSet runs every provider in parallel and returns all results (failures
// included, each marked with Err). It enforces no minimum-success count and never
// invokes the arbiter — callers decide how to aggregate the raw analyst opinions.
func (a *Analyzer) runAnalystSet(ctx context.Context, req AnalysisRequest, available []ModelProvider) []AnalystResult {
	mu := &sync.Mutex{}
	results := make([]AnalystResult, 0, len(available))

	var wg sync.WaitGroup
	for _, p := range available {
		wg.Add(1)
		go func() {
			defer wg.Done()
			timeout := modelTimeout(p, a.cfg)
			mctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			t0 := time.Now()
			resp, meta, err := p.Analyze(mctx, req)
			lat := time.Since(t0).Milliseconds()
			if err != nil {
				logger.Warn("analyst failed", "provider", p.ProviderName(), "model", p.ModelName(), "ticker", req.Ticker, "err", err)
			}
			mu.Lock()
			results = append(results, AnalystResult{
				Provider: p.ProviderName(), Model: p.ModelName(),
				Response: resp, Meta: meta, Err: err, LatencyMs: lat,
			})
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results
}

// AnalyzeAnalysts runs the depth-filtered analysts and returns their raw per-analyst
// results with NO arbiter and only a >=1-success requirement (it errors only when
// EVERY analyst fails). Callers that do their own aggregation — e.g. the ORB gap scan,
// which must keep selecting candidates when the arbiter is unconfigured/failing or when
// only a single analyst is reachable — use this instead of Analyze/AnalyzeFull.
func (a *Analyzer) AnalyzeAnalysts(ctx context.Context, req AnalysisRequest, impact depth.ImpactScore) ([]AnalystResult, error) {
	active := a.analysts
	if impact.Depth > 0 && impact.Depth < len(a.analysts) {
		active = a.analysts[:impact.Depth]
	}
	available := availableProviders(active)
	if len(available) == 0 {
		return nil, fmt.Errorf("no analysts available (check API keys)")
	}
	results := a.runAnalystSet(ctx, req, available)
	if len(successfulResults(results)) == 0 {
		return results, fmt.Errorf("all %d analysts failed", len(available))
	}
	return results, nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func modelTimeout(p ModelProvider, cfg config.ModelsConfig) time.Duration {
	for _, ac := range cfg.AnalystConfigs() {
		if ac.Provider == p.ProviderName() && ac.Model == p.ModelName() && ac.TimeoutSec > 0 {
			return time.Duration(ac.TimeoutSec) * time.Second
		}
	}
	return 30 * time.Second
}

// modelTimeoutForRegistry resolves the per-call timeout for a registry-keyed
// provider. Specialist roles (Issue #323) often map to registry entries that
// are NOT in models.roles.analysts, so the analysts-only modelTimeout() scan
// would silently fall back to the hardcoded 30s and ignore registry-level
// timeout_sec configuration. Lookup order:
//  1. cfg.Registry[registryKey].TimeoutSec  (specialist + arbiter source of truth)
//  2. modelTimeout(p, cfg)                  (analysts-config fallback)
//  3. 30s                                    (final hard default, via modelTimeout)
func modelTimeoutForRegistry(registryKey string, p ModelProvider, cfg config.ModelsConfig) time.Duration {
	if registryKey != "" && cfg.Registry != nil {
		if entry, ok := cfg.Registry[registryKey]; ok && entry.TimeoutSec > 0 {
			return time.Duration(entry.TimeoutSec) * time.Second
		}
	}
	return modelTimeout(p, cfg)
}

func primaryMeta(results []AnalystResult) ModelCallMeta {
	for _, r := range results {
		if r.Err == nil {
			return r.Meta
		}
	}
	return ModelCallMeta{}
}

// ─── Factory ──────────────────────────────────────────────────────────────────

// NewProviderFromConfig builds a standalone ModelProvider from a ModelConfig.
// Useful for callers outside this package (e.g. the engine's extraction path)
// that need a provider that is not part of the analyst ensemble.
func NewProviderFromConfig(cfg config.ModelConfig) ModelProvider {
	return buildProvider(cfg)
}

func buildProvider(cfg config.ModelConfig) ModelProvider {
	switch cfg.Provider {
	case "anthropic":
		return NewAnthropicClient(cfg)
	case "openai":
		return NewOpenAIClient(cfg)
	case "deepseek":
		return NewDeepSeekClient(cfg)
	case "google":
		return NewGeminiClient(cfg)
	case "ollama":
		return NewOllamaClient(cfg)
	default:
		logger.Warn("unknown model provider — registering as unavailable stub", "provider", cfg.Provider)
		return &unavailableStub{provider: cfg.Provider, model: cfg.Model}
	}
}

type unavailableStub struct{ provider, model string }

func (s *unavailableStub) ProviderName() string { return s.provider }
func (s *unavailableStub) ModelName() string    { return s.model }
func (s *unavailableStub) IsAvailable() bool    { return false }
func (s *unavailableStub) Analyze(_ context.Context, _ AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
	return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("provider %q not implemented", s.provider)
}

func buildProviders(cfgs []config.ModelConfig) []ModelProvider {
	out := make([]ModelProvider, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, buildProvider(c))
	}
	return out
}

func hashPrompt(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return fmt.Sprintf("%x", sum)
}
