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
		fallback    *config.ModelConfig
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
		if fallback, ok := a.cfg.ResolveFallback(key); ok {
			rp.fallback = &fallback
		}
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
		sys, user := prompts.BuildRolePromptWithAnalysisContext(
			rp.role, req.Ticker, req.LensSet(), req.Timeframe,
			promptCandles, promptInd, promptNews, promptPortfolio,
			req.KnowledgeContext, promptSimilar, req.PerformanceContext, req.DiscoveryContext, req.PullbackContext,
			req.TimeframeRole, req.PrimaryThesisContext, req.RiskPolicy,
		)
		sys, user = appendTimeframeContexts(sys, user, req.TimeframeContexts)
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
			// Use registry-aware timeout values independently for the primary
			// and fallback calls. Both remain bounded by the overall analysis ctx.
			timeout := modelTimeoutForRegistry(rp.registryKey, rp.provider, a.cfg)
			t0 := time.Now()
			provider, resp, meta, err := analyzeSpecialistRole(ctx, rp.provider, rp.fallback, timeout, roleReq)
			lat := time.Since(t0).Milliseconds()
			meta.LatencyMs = lat
			if err != nil {
				logger.Warn("specialist analyst failed",
					"ticker", req.Ticker, "role", rp.role,
					"provider", provider.ProviderName(),
					"model", provider.ModelName(), "err", err)
			}
			mu.Lock()
			results = append(results, specialistResult{
				Role:     rp.role,
				Provider: provider,
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

	sys, user := prompts.BuildRiskManagerReviewPromptWithAnalysisContext(
		req.Ticker, req.LensSet(), req.Timeframe,
		promptCandles, promptInd, promptNews, promptPortfolio,
		req.KnowledgeContext, promptSimilar, req.PerformanceContext,
		formatDirectionalFindings(results), req.DiscoveryContext, req.PullbackContext,
		req.TimeframeRole, req.PrimaryThesisContext, req.RiskPolicy,
	)
	sys, user = appendTimeframeContexts(sys, user, req.TimeframeContexts)
	if sys == "" || user == "" {
		return AnalyzeResult{AnalystResults: projectSpecialistResults(results)},
			fmt.Errorf("specialist: failed to build mandatory risk-manager review prompt")
	}
	riskReq := req
	riskReq.CustomSystemPrompt = sys
	riskReq.CustomUserMessage = user
	timeout := modelTimeoutForRegistry(riskReviewer.registryKey, riskReviewer.provider, a.cfg)
	t0 := time.Now()
	riskProvider, riskResp, riskMeta, riskErr := analyzeSpecialistRole(ctx, riskReviewer.provider, riskReviewer.fallback, timeout, riskReq)
	riskMeta.LatencyMs = time.Since(t0).Milliseconds()
	riskResult := specialistResult{
		Role: "risk_manager", Provider: riskProvider,
		Response: riskResp, Meta: riskMeta, Err: riskErr,
	}
	results = append(results, riskResult)
	if riskErr != nil {
		logger.Warn("mandatory risk specialist failed",
			"ticker", req.Ticker,
			"provider", riskProvider.ProviderName(),
			"model", riskProvider.ModelName(),
			"err", riskErr)
		return AnalyzeResult{AnalystResults: projectSpecialistResults(results)},
			fmt.Errorf("specialist: mandatory risk-manager review failed: %w", riskErr)
	}

	final, policyAudit := resolveSpecialistRiskDecision(results, riskResp)
	final.Reasoning = composeSequentialSpecialistReasoning(results, final)
	// A risk BUY can retain an admissible selected lens. Any deterministic
	// downgrade must instead use consensus evidence for its non-directional
	// outcome; retaining a BUY lens on WAIT/HOLD would misstate the audit.
	riskSelectedLens := final.Action == ActionBuy && validSpecialistRiskLens(riskResp.SelectedStrategy, req.LensSet())
	if !riskSelectedLens {
		final.SelectedStrategy, final.Regime = specialistConsensusLens(results, final.Action)
	} else {
		final.SelectedStrategy, final.Regime = riskResp.SelectedStrategy, riskResp.Regime
	}
	// The risk manager runs the full candidate-lens contract too, so riskResp
	// carries ITS OWN rejection map. Left in place beside a consensus-derived
	// SelectedStrategy, the aggregate could name its selected lens as a
	// rejected one. Inert today (only len() is read, and the field is not
	// persisted), but the two fields must describe the same decision before
	// anything is ever wired to them.
	if riskSelectedLens {
		final.RejectedStrategies = riskResp.RejectedStrategies
	} else {
		final.RejectedStrategies = specialistConsensusRejections(results, final.Action, final.SelectedStrategy)
	}
	logger.Info("specialist risk review complete",
		"ticker", req.Ticker,
		"action", string(final.Action),
		"directional_successes", successes,
		"risk_reviewer", riskProvider.ModelName(),
	)
	return AnalyzeResult{
		Response:       final,
		Meta:           riskMeta,
		AnalystResults: projectSpecialistResults(results),
		DecisionPolicy: policyAudit,
	}, nil
}

// analyzeSpecialistRole tries the configured one-hop registry fallback only
// after Gemini exhausts its own retries on HTTP 429/503. Other failures (for
// example billing 4xx responses, timeouts, or MAX_TOKENS) remain visible and do
// not silently switch models.
func analyzeSpecialistRole(
	ctx context.Context,
	primary ModelProvider,
	fallbackConfig *config.ModelConfig,
	primaryTimeout time.Duration,
	req AnalysisRequest,
) (ModelProvider, AnalysisResponse, ModelCallMeta, error) {
	primaryCtx, cancelPrimary := context.WithTimeout(ctx, primaryTimeout)
	resp, meta, err := primary.Analyze(primaryCtx, req)
	cancelPrimary()
	if err == nil || fallbackConfig == nil || !shouldUseGeminiFallback(err) {
		return primary, resp, meta, err
	}

	fallback := specialistBuildProvider(*fallbackConfig)
	if !fallback.IsAvailable() {
		return primary, resp, meta, fmt.Errorf("%w; configured fallback %s/%s is unavailable",
			err, fallback.ProviderName(), fallback.ModelName())
	}
	fallbackTimeout := modelConfigTimeout(*fallbackConfig)
	fallbackCtx, cancelFallback := context.WithTimeout(ctx, fallbackTimeout)
	fallbackResp, fallbackMeta, fallbackErr := fallback.Analyze(fallbackCtx, req)
	cancelFallback()
	if fallbackErr != nil {
		return fallback, fallbackResp, fallbackMeta,
			fmt.Errorf("primary %s/%s failed: %w; fallback %s/%s failed: %v",
				primary.ProviderName(), primary.ModelName(), err,
				fallback.ProviderName(), fallback.ModelName(), fallbackErr)
	}
	logger.Warn("specialist model fallback succeeded",
		"ticker", req.Ticker,
		"primary_model", primary.ModelName(),
		"fallback_model", fallback.ModelName())
	return fallback, fallbackResp, fallbackMeta, nil
}

// modelConfigTimeout mirrors provider client defaults for a standalone config.
// A fallback gets its own configured/default timeout rather than inheriting the
// primary's possibly shorter timeout window.
func modelConfigTimeout(cfg config.ModelConfig) time.Duration {
	if cfg.TimeoutSec > 0 {
		return time.Duration(cfg.TimeoutSec) * time.Second
	}
	switch cfg.Provider {
	case "anthropic", "ollama":
		return 60 * time.Second
	default:
		return 30 * time.Second
	}
}

// resolveSpecialistRiskDecision gives the risk reviewer asymmetric authority:
// it may always veto a BUY, but it cannot invent a new long entry absent at
// least two independent directional BUY votes. Protective exits remain intact.
func resolveSpecialistRiskDecision(results []specialistResult, riskResp AnalysisResponse) (AnalysisResponse, *DecisionPolicyAudit) {
	votes := make(map[string]Action, 3)
	buyVotes := 0
	hasWait := false
	for _, result := range results {
		if result.Err != nil || result.Role == "risk_manager" {
			continue
		}
		votes[result.Role] = result.Response.Action
		if result.Response.Action == ActionBuy {
			buyVotes++
		}
		if result.Response.Action == ActionWait {
			hasWait = true
		}
	}
	audit := &DecisionPolicyAudit{
		Version: 1, Mode: "specialist_buy_authority", DirectionalVotes: votes,
		RiskAction: riskResp.Action, BuyVotes: buyVotes, RequiredBuyVotes: 2,
		OriginalAction: riskResp.Action, ResolvedAction: riskResp.Action,
	}
	if riskResp.Action != ActionBuy || buyVotes >= 2 {
		return riskResp, audit
	}
	resolved := ActionHold
	if hasWait {
		resolved = ActionWait
	}
	final := riskResp
	final.Action = resolved
	final.SizePct, final.StopLoss, final.TakeProfit = 0, 0, 0
	audit.ResolvedAction = resolved
	audit.Applied = true
	audit.Reason = fmt.Sprintf("risk BUY requires at least 2/3 directional BUY votes; received %d/3", buyVotes)
	return final, audit
}

func validSpecialistRiskLens(selected string, admissible []string) bool {
	selected = strings.ToLower(strings.TrimSpace(selected))
	if selected == "" || selected == "none" {
		return false
	}
	for _, lens := range prompts.Lenses(admissible) {
		if selected == lens {
			return true
		}
	}
	return false
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

// specialistConsensusRejections returns the rejection reasons that belong with
// a consensus-derived selection: the union of what the winning directional
// specialists rejected, minus the lens that was actually selected.
//
// Returns nil when there is no consensus selection, so an aggregate never
// carries rejections that no longer correspond to a decision.
func specialistConsensusRejections(results []specialistResult, winner Action, selected string) map[string]string {
	if selected == "" {
		return nil
	}
	var out map[string]string
	for _, r := range results {
		if r.Err != nil || r.Role == "risk_manager" || r.Response.Action != winner {
			continue
		}
		for lens, reason := range r.Response.RejectedStrategies {
			if lens == selected {
				continue
			}
			if out == nil {
				out = make(map[string]string, len(r.Response.RejectedStrategies))
			}
			if _, seen := out[lens]; !seen {
				out[lens] = reason
			}
		}
	}
	return out
}
