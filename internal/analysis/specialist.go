package analysis

// specialist.go — sequential specialist analysis path.
//
// Inspired by Grok 4.20's Alpha Arena stock-contest winning entry, which used
// role-specialised agents. Fundamental, technical, and sentiment analysis run
// in parallel; their findings then flow through a mandatory Risk Specialist.
//
// This path is gated by analysis.specialist_mode_enabled (default false).

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis/depth"
	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/logger"
)

// specialistRoleOrder is the canonical order used in persisted reasoning and UI.
var specialistRoleOrder = []string{"fundamental", "technical", "sentiment", "risk_manager"}

// specialistBuildProvider is the seam used by AnalyzeSpecialist to construct a
// per-role provider from its resolved ModelConfig. Defaults to buildProvider
// in production; tests overwrite this to inject fakes that don't require real
// API keys.
var specialistBuildProvider = buildProvider

// specialistResult bundles a per-role response with the provider that produced it.
type specialistResult struct {
	Role     string
	Provider ModelProvider
	Response AnalysisResponse
	Meta     ModelCallMeta
	Err      error
}

// AnalyzeSpecialist runs the three directional specialists in parallel, then
// passes their completed findings through a mandatory Risk Specialist review.
// The Risk Specialist owns the final analytical outcome; deterministic
// portfolio risk controls still run later in the engine.
func (a *Analyzer) AnalyzeSpecialist(ctx context.Context, req AnalysisRequest, _ depth.ImpactScore) (AnalyzeResult, error) {
	specialistCfg := a.specialistRolesConfig()
	if len(specialistCfg) == 0 {
		return AnalyzeResult{}, fmt.Errorf("specialist mode enabled but analysis.specialist_roles is empty")
	}

	// Resolve each role to a ModelProvider; skip with WARN when missing.
	type roleProvider struct {
		role        string
		registryKey string
		provider    ModelProvider
	}
	var directional []roleProvider
	var riskReviewer *roleProvider
	for _, role := range specialistRoleOrder {
		key, ok := specialistCfg[role]
		if !ok || key == "" {
			logger.Warn("specialist role not configured — skipping", "role", role)
			continue
		}
		mc, err := a.cfg.Resolve(key)
		if err != nil {
			logger.Warn("specialist role registry lookup failed — skipping",
				"role", role, "registry_key", key, "err", err)
			continue
		}
		p := specialistBuildProvider(mc)
		if !p.IsAvailable() {
			logger.Warn("specialist role provider not available — skipping",
				"role", role, "provider", p.ProviderName(), "model", p.ModelName())
			continue
		}
		rp := roleProvider{role: role, registryKey: key, provider: p}
		if role == "risk_manager" {
			riskReviewer = &rp
		} else {
			directional = append(directional, rp)
		}
	}
	if len(directional) != 3 {
		return AnalyzeResult{}, fmt.Errorf("specialist mode: only %d/3 directional roles available — all three are required", len(directional))
	}
	if riskReviewer == nil {
		return AnalyzeResult{}, fmt.Errorf("specialist mode: mandatory risk_manager role is unavailable")
	}

	// First stage: fan out only to directional specialists. Role-based node IDs
	// stay unique even when multiple roles share one registry model.
	if a.bus != nil {
		selected := make([]string, 0, len(directional))
		for _, rp := range directional {
			selected = append(selected, specialistNodeID(rp.role))
		}
		a.bus.Publish(events.AnalysisStarted, events.AnalysisStartedPayload{
			Ticker:           req.Ticker,
			Depth:            len(directional),
			SelectedAnalysts: selected,
		})
	}

	// Build the per-role custom system prompt. The user message is shared.
	promptCandles := toPromptCandles(req.RecentCandles)
	promptInd := toPromptIndicators(req.Indicators)
	promptNews := toPromptNews(req.News)
	promptPortfolio := toPromptPortfolio(req.Portfolio, req.Ticker)
	promptSimilar := toPromptSimilarSetups(req.SimilarAnalyses)

	mu := &sync.Mutex{}
	results := make([]specialistResult, 0, len(directional)+1)
	var wg sync.WaitGroup

	for _, rp := range directional {
		rp := rp
		sys, user := prompts.BuildRolePrompt(
			rp.role, req.Ticker, req.LensSet(), req.Timeframe,
			promptCandles, promptInd, promptNews, promptPortfolio,
			req.KnowledgeContext, promptSimilar, req.PerformanceContext,
		)
		if sys == "" || user == "" {
			logger.Warn("specialist role builder returned empty prompt — skipping", "role", rp.role)
			continue
		}
		// Per-role request — same data block, role-specific system prompt.
		// Custom prompts bypass the standard BuildStockPrompt path in each
		// provider's Analyze method.
		roleReq := req
		roleReq.CustomSystemPrompt = sys
		roleReq.CustomUserMessage = user

		wg.Add(1)
		go func() {
			defer wg.Done()
			// Use the registry-aware helper: specialist roles map to registry
			// keys that are typically NOT in models.roles.analysts, so the
			// analysts-only modelTimeout() would silently fall back to 30s
			// instead of honouring registry.<key>.timeout_sec.
			timeout := modelTimeoutForRegistry(rp.registryKey, rp.provider, a.cfg)
			mctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			t0 := time.Now()
			resp, meta, err := rp.provider.Analyze(mctx, roleReq)
			lat := time.Since(t0).Milliseconds()
			meta.LatencyMs = lat
			if err != nil {
				logger.Warn("specialist analyst failed",
					"ticker", req.Ticker, "role", rp.role,
					"provider", rp.provider.ProviderName(),
					"model", rp.provider.ModelName(), "err", err)
			}
			mu.Lock()
			results = append(results, specialistResult{
				Role:     rp.role,
				Provider: rp.provider,
				Response: resp,
				Meta:     meta,
				Err:      err,
			})
			mu.Unlock()
		}()
	}
	wg.Wait()

	// Sort by canonical role order for deterministic output and stable telemetry.
	sort.Slice(results, func(i, j int) bool {
		return roleIndex(results[i].Role) < roleIndex(results[j].Role)
	})
	successes := 0
	for _, r := range results {
		if r.Err == nil {
			successes++
		}
	}
	if successes != 3 {
		return AnalyzeResult{
			AnalystResults: projectSpecialistResults(results),
		}, fmt.Errorf("specialist: only %d/3 directional roles succeeded — risk review requires all three", successes)
	}

	// Second stage: all successful directional findings feed the mandatory
	// Risk Specialist. Publish the fan-in so the canvas shows the real path.
	sourceNodes := make([]string, 0, successes)
	for _, r := range results {
		if r.Err == nil {
			sourceNodes = append(sourceNodes, specialistNodeID(r.Role))
		}
	}
	if a.bus != nil {
		a.bus.Publish(events.AnalysisStarted, events.AnalysisStartedPayload{
			Ticker:           req.Ticker,
			Depth:            1,
			SelectedAnalysts: []string{"specialist-risk-manager"},
			SourceNodes:      sourceNodes,
		})
	}

	sys, user := prompts.BuildRiskManagerReviewPrompt(
		req.Ticker, req.LensSet(), req.Timeframe,
		promptCandles, promptInd, promptNews, promptPortfolio,
		req.KnowledgeContext, promptSimilar, req.PerformanceContext,
		formatDirectionalFindings(results),
	)
	if sys == "" || user == "" {
		return AnalyzeResult{AnalystResults: projectSpecialistResults(results)},
			fmt.Errorf("specialist: failed to build mandatory risk-manager review prompt")
	}
	riskReq := req
	riskReq.CustomSystemPrompt = sys
	riskReq.CustomUserMessage = user
	timeout := modelTimeoutForRegistry(riskReviewer.registryKey, riskReviewer.provider, a.cfg)
	riskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t0 := time.Now()
	riskResp, riskMeta, riskErr := riskReviewer.provider.Analyze(riskCtx, riskReq)
	riskMeta.LatencyMs = time.Since(t0).Milliseconds()
	riskResult := specialistResult{
		Role: "risk_manager", Provider: riskReviewer.provider,
		Response: riskResp, Meta: riskMeta, Err: riskErr,
	}
	results = append(results, riskResult)
	if riskErr != nil {
		logger.Warn("mandatory risk specialist failed",
			"ticker", req.Ticker,
			"provider", riskReviewer.provider.ProviderName(),
			"model", riskReviewer.provider.ModelName(),
			"err", riskErr)
		return AnalyzeResult{AnalystResults: projectSpecialistResults(results)},
			fmt.Errorf("specialist: mandatory risk-manager review failed: %w", riskErr)
	}

	final := riskResp
	final.Reasoning = composeSequentialSpecialistReasoning(results, riskResp)
	// The risk manager synthesises the directional findings but is not asked to
	// re-select a lens, so its response carries no usable lens fields. Derive
	// them from the directional specialists who reached the same verdict —
	// without this, specialist mode reports an empty selection and the engine's
	// selected-lens entry gates silently fall back to the configured primary
	// (the same defect fixed for the ensemble aggregation paths).
	final.SelectedStrategy, final.Regime = specialistConsensusLens(results, final.Action)
	logger.Info("specialist risk review complete",
		"ticker", req.Ticker,
		"action", string(final.Action),
		"directional_successes", successes,
		"risk_reviewer", riskReviewer.provider.ModelName(),
	)
	return AnalyzeResult{
		Response:       final,
		Meta:           riskMeta,
		AnalystResults: projectSpecialistResults(results),
	}, nil
}

func specialistNodeID(role string) string {
	return "specialist-" + strings.ReplaceAll(role, "_", "-")
}

func projectSpecialistResults(results []specialistResult) []AnalystResult {
	projected := make([]AnalystResult, 0, len(results))
	sort.Slice(results, func(i, j int) bool {
		return roleIndex(results[i].Role) < roleIndex(results[j].Role)
	})
	for _, r := range results {
		projected = append(projected, AnalystResult{
			Provider: r.Meta.Provider, Model: r.Meta.Model, Role: r.Role,
			Response: r.Response, Meta: r.Meta, Err: r.Err, LatencyMs: r.Meta.LatencyMs,
		})
	}
	return projected
}

func formatDirectionalFindings(results []specialistResult) string {
	var b strings.Builder
	for _, r := range results {
		if r.Err != nil || r.Role == "risk_manager" {
			continue
		}
		fmt.Fprintf(&b, "[%s] action=%s confidence=%.2f size_pct=%.2f stop_loss=%.4f take_profit=%.4f\n%s\n\n",
			r.Role, string(r.Response.Action), r.Response.Confidence, r.Response.SizePct,
			r.Response.StopLoss, r.Response.TakeProfit, strings.TrimSpace(r.Response.Reasoning))
	}
	return strings.TrimSpace(b.String())
}

func composeSequentialSpecialistReasoning(results []specialistResult, final AnalysisResponse) string {
	var b strings.Builder
	sort.Slice(results, func(i, j int) bool {
		return roleIndex(results[i].Role) < roleIndex(results[j].Role)
	})
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		fmt.Fprintf(&b, "[%s] %s — %s (conf %.2f)\n",
			r.Role, string(r.Response.Action), strings.TrimSpace(r.Response.Reasoning), r.Response.Confidence)
	}
	fmt.Fprintf(&b, "[specialist] resolved by mandatory risk-manager review → %s", string(final.Action))
	return b.String()
}

// roleIndex returns the canonical position of role in specialistRoleOrder.
// Unknown roles sort to the end.
func roleIndex(role string) int {
	for i, r := range specialistRoleOrder {
		if r == role {
			return i
		}
	}
	return len(specialistRoleOrder)
}

// specialistRolesConfig pulls the role→registry-key map from the analyzer's
// config. Returned map is a copy to avoid accidental mutation.
func (a *Analyzer) specialistRolesConfig() map[string]string {
	// We need the AnalysisConfig but Analyzer only holds ModelsConfig. The
	// engine passes specialist routing to AnalyzeSpecialist via the analyzer's
	// stored config when injected — see SetSpecialistConfig below.
	if a.specialist == nil {
		return nil
	}
	out := make(map[string]string, len(a.specialist.SpecialistRoles))
	for k, v := range a.specialist.SpecialistRoles {
		out[k] = v
	}
	return out
}

// SetSpecialistConfig wires the specialist sub-config into the analyzer so
// AnalyzeSpecialist can resolve roles. Called by the engine constructor when
// analysis.specialist_mode_enabled is true.
func (a *Analyzer) SetSpecialistConfig(cfg *config.AnalysisConfig) {
	a.specialist = cfg
}

// specialistConsensusLens mirrors consensusLens over specialist results:
// unanimous selection among the directional specialists who voted for the
// winning action, empty when they disagree. The risk manager is excluded — it
// reviews rather than votes, and is not asked to select a lens.
func specialistConsensusLens(results []specialistResult, winner Action) (selected, regime string) {
	first := true
	for _, r := range results {
		if r.Err != nil || r.Role == "risk_manager" || r.Response.Action != winner {
			continue
		}
		if first {
			selected, regime = r.Response.SelectedStrategy, r.Response.Regime
			first = false
			continue
		}
		if r.Response.SelectedStrategy != selected {
			selected = ""
		}
		if r.Response.Regime != regime {
			regime = ""
		}
	}
	return selected, regime
}
