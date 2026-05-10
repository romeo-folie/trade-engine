package analysis

// specialist.go — Issue #323: 4-role specialist arbiter path.
//
// Inspired by Grok 4.20's Alpha Arena stock-contest winning entry, which used
// four role-specialised agents (fundamental / technical / sentiment / risk
// manager) in debate before deciding. The previous ensemble used N copies of
// the same prompt across providers — diversity came only from model weights.
//
// This path is gated by analysis.specialist_mode_enabled (default false) so
// the existing arbiter remains the production path until operators opt in.

import (
	"context"
	"fmt"
	"math"
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

// specialistRoleOrder is the canonical ordering used in reasoning concatenation
// and table-driven tests. Risk-manager is last so its veto reads naturally.
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

// AnalyzeSpecialist runs one specialist analyst per configured role and
// resolves disagreement via 3-of-4 majority + risk-manager veto.
//
// Resolution rules:
//  1. If 3+ roles agree on action, use that action.
//  2. Otherwise, when the risk-manager returned WAIT or HOLD that verdict is
//     decisive — final action is whichever of WAIT/HOLD it returned.
//  3. If risk-manager isn't configured or didn't return successfully, fall
//     back to majority vote (the existing behavior).
//
// Confidence is the average over returning roles. Reasoning is concatenated
// per-role and prefixed by the role label so downstream UI can split it back.
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
	var rps []roleProvider
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
		rps = append(rps, roleProvider{role: role, registryKey: key, provider: p})
	}
	if len(rps) < 2 {
		return AnalyzeResult{}, fmt.Errorf("specialist mode: only %d/%d roles available — need at least 2", len(rps), len(specialistRoleOrder))
	}

	// Emit AnalysisStarted so the pipeline graph / WebSocket consumers can pulse
	// the selected analyst nodes — mirrors AnalyzeFull's behavior. SelectedAnalysts
	// reflects only the roles that will actually run (skipped/missing roles are
	// excluded). Naming convention: "analyst-<registry_key>".
	if a.bus != nil {
		selected := make([]string, 0, len(rps))
		for _, rp := range rps {
			selected = append(selected, "analyst-"+rp.registryKey)
		}
		a.bus.Publish(events.AnalysisStarted, events.AnalysisStartedPayload{
			Ticker:           req.Ticker,
			Depth:            len(rps),
			SelectedAnalysts: selected,
		})
	}

	// Build the per-role custom system prompt. The user message is shared.
	promptCandles := toPromptCandles(req.RecentCandles)
	promptInd := toPromptIndicators(req.Indicators)
	promptNews := toPromptNews(req.News)
	promptPortfolio := toPromptPortfolio(req.Portfolio)
	promptSimilar := toPromptSimilarSetups(req.SimilarAnalyses)

	mu := &sync.Mutex{}
	results := make([]specialistResult, 0, len(rps))
	var wg sync.WaitGroup

	for _, rp := range rps {
		rp := rp
		sys, user := prompts.BuildRolePrompt(
			rp.role, req.Ticker, req.Strategy, req.Timeframe,
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
			timeout := modelTimeout(rp.provider, a.cfg)
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

	// Project per-role results into AnalystResults so cost-tracking / UI keep working.
	analystResults := make([]AnalystResult, 0, len(results))
	successes := 0
	// Sort by canonical role order for deterministic output and stable telemetry.
	sort.Slice(results, func(i, j int) bool {
		return roleIndex(results[i].Role) < roleIndex(results[j].Role)
	})
	for _, r := range results {
		analystResults = append(analystResults, AnalystResult{
			Provider:  r.Meta.Provider,
			Model:     r.Meta.Model,
			Response:  r.Response,
			Meta:      r.Meta,
			Err:       r.Err,
			LatencyMs: r.Meta.LatencyMs,
		})
		if r.Err == nil {
			successes++
		}
	}
	if successes < 2 {
		return AnalyzeResult{AnalystResults: analystResults}, fmt.Errorf("specialist: only %d/%d roles succeeded (minimum 2)", successes, len(results))
	}

	final := resolveSpecialist(results)
	logger.Info("specialist arbitrated",
		"ticker", req.Ticker,
		"action", string(final.Action),
		"successes", successes,
		"roles", len(results),
	)
	return AnalyzeResult{
		Response:       final,
		Meta:           pickPrimarySpecialistMeta(results),
		AnalystResults: analystResults,
	}, nil
}

// resolveSpecialist applies the 3-of-4 majority + risk-manager veto rules.
// Exposed at package scope so tests can exercise it without standing up a
// full Analyzer + provider stack.
func resolveSpecialist(results []specialistResult) AnalysisResponse {
	successes := make([]specialistResult, 0, len(results))
	for _, r := range results {
		if r.Err == nil {
			successes = append(successes, r)
		}
	}
	if len(successes) == 0 {
		return AnalysisResponse{Action: ActionHold, Reasoning: "[specialist] no successful roles; defaulting to HOLD"}
	}

	// Tally action votes.
	counts := map[Action]int{}
	for _, r := range successes {
		counts[r.Response.Action]++
	}

	// Rule 1: 3+ roles agree → use that action.
	var majorityAction Action
	for action, c := range counts {
		if c >= 3 {
			majorityAction = action
			break
		}
	}

	// Locate the risk-manager response (if it succeeded).
	var rm *specialistResult
	for i := range successes {
		if successes[i].Role == "risk_manager" {
			rm = &successes[i]
			break
		}
	}

	var finalAction Action
	switch {
	case majorityAction != "":
		finalAction = majorityAction
	case rm != nil && (rm.Response.Action == ActionWait || rm.Response.Action == ActionHold):
		// Rule 2: risk-manager veto — its WAIT or HOLD is decisive when no majority.
		finalAction = rm.Response.Action
	default:
		// Rule 3: fallback to existing majority-vote helper. Wrap successes
		// into AnalystResult shape so we can reuse majorityVote() unchanged.
		fallback := make([]AnalystResult, 0, len(successes))
		for _, r := range successes {
			fallback = append(fallback, AnalystResult{
				Provider: r.Meta.Provider,
				Model:    r.Meta.Model,
				Response: r.Response,
			})
		}
		mv, ok := majorityVote(fallback)
		if !ok {
			finalAction = ActionHold
		} else {
			finalAction = mv.Action
		}
	}

	// Confidence averages over ALL successful roles (it expresses overall
	// committee certainty, not just the winners' certainty).
	var sumConf float64
	for _, r := range successes {
		sumConf += r.Response.Confidence
	}
	n := float64(len(successes))
	avgConf := math.Round(sumConf/n*1000) / 1000

	// Sizing / SL / TP must average ONLY over roles whose action matches the
	// final action. Otherwise a 3-of-4 BUY decision would inherit the dissenting
	// SELL voter's opposite-direction stop-loss / take-profit, corrupting the
	// risk levels on the resulting signal. See Codex review on PR #342.
	var winners []specialistResult
	for _, r := range successes {
		if r.Response.Action == finalAction {
			winners = append(winners, r)
		}
	}
	var avgSize, avgSL, avgTP float64
	if len(winners) == 0 {
		// Safety fallback — finalAction came from majority/veto, so at least one
		// successful role should match. If none do (e.g. majority-vote helper
		// returned a cascade winner that no role actually voted for), keep risk
		// levels at zero rather than averaging across opposite-direction voters.
		logger.Warn("specialist: no roles matched finalAction — zeroing sizing",
			"final_action", string(finalAction))
	} else {
		var sumSize, sumSL, sumTP float64
		var nSL, nTP int
		for _, r := range winners {
			sumSize += r.Response.SizePct
			if r.Response.StopLoss > 0 {
				sumSL += r.Response.StopLoss
				nSL++
			}
			if r.Response.TakeProfit > 0 {
				sumTP += r.Response.TakeProfit
				nTP++
			}
		}
		w := float64(len(winners))
		avgSize = math.Round(sumSize/w*100) / 100
		if nSL > 0 {
			avgSL = math.Round(sumSL/float64(nSL)*10000) / 10000
		}
		if nTP > 0 {
			avgTP = math.Round(sumTP/float64(nTP)*10000) / 10000
		}
	}

	// Suppress sizing for non-actionable verdicts (mirrors arbiter behavior).
	if finalAction == ActionHold || finalAction == ActionWait {
		avgSize = 0
		avgSL = 0
		avgTP = 0
	}

	return AnalysisResponse{
		Action:     finalAction,
		Confidence: avgConf,
		SizePct:    avgSize,
		StopLoss:   avgSL,
		TakeProfit: avgTP,
		Reasoning:  composeSpecialistReasoning(successes, finalAction, majorityAction != "", rm),
	}
}

// composeSpecialistReasoning concatenates per-role reasoning prefixed by the
// role label, then appends a one-line resolution note.
func composeSpecialistReasoning(successes []specialistResult, finalAction Action, hadMajority bool, rm *specialistResult) string {
	var b strings.Builder
	// Sort so the output is deterministic (canonical role order).
	sort.Slice(successes, func(i, j int) bool { return roleIndex(successes[i].Role) < roleIndex(successes[j].Role) })

	for _, r := range successes {
		// Cast the Action defined-string-type to string so %s formats as the
		// literal action verb instead of "%!s(analysis.Action=BUY)".
		fmt.Fprintf(&b, "[%s] %s — %s (conf %.2f)\n", r.Role, string(r.Response.Action), strings.TrimSpace(r.Response.Reasoning), r.Response.Confidence)
	}

	switch {
	case hadMajority:
		fmt.Fprintf(&b, "[specialist] resolved by 3-of-%d majority → %s", len(successes), string(finalAction))
	case rm != nil && (rm.Response.Action == ActionWait || rm.Response.Action == ActionHold):
		fmt.Fprintf(&b, "[specialist] resolved by risk-manager veto (%s) → %s", string(rm.Response.Action), string(finalAction))
	default:
		fmt.Fprintf(&b, "[specialist] resolved by majority-vote fallback → %s", string(finalAction))
	}
	return b.String()
}

// pickPrimarySpecialistMeta returns the first successful role's meta so callers
// have a non-empty Provider/Model for cost telemetry. Falls back to the first
// result of any kind.
func pickPrimarySpecialistMeta(results []specialistResult) ModelCallMeta {
	for _, r := range results {
		if r.Err == nil {
			return r.Meta
		}
	}
	if len(results) > 0 {
		return results[0].Meta
	}
	return ModelCallMeta{}
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
