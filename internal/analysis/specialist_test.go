package analysis

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis/depth"
	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
)

// fakeRoleProvider is a stub ModelProvider used by AnalyzeSpecialist tests so
// we don't need real API keys. Records the request and returns a fixed BUY.
type fakeRoleProvider struct {
	provider string
	model    string
	request  AnalysisRequest
}

type captureRiskProvider struct {
	request  AnalysisRequest
	response AnalysisResponse
}

func (p *captureRiskProvider) ProviderName() string { return "risk-provider" }
func (p *captureRiskProvider) ModelName() string    { return "risk-model" }
func (p *captureRiskProvider) IsAvailable() bool    { return true }
func (p *captureRiskProvider) Analyze(_ context.Context, req AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
	p.request = req
	return p.response, ModelCallMeta{Provider: p.ProviderName(), Model: p.ModelName()}, nil
}

func (f *fakeRoleProvider) ProviderName() string { return f.provider }
func (f *fakeRoleProvider) ModelName() string    { return f.model }
func (f *fakeRoleProvider) IsAvailable() bool    { return true }
func (f *fakeRoleProvider) Analyze(_ context.Context, req AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
	f.request = req
	return AnalysisResponse{
			Action: ActionBuy, Confidence: 0.7,
			SizePct: 1.0, StopLoss: 100, TakeProfit: 110,
			Reasoning: "fake " + f.provider,
		},
		ModelCallMeta{Provider: f.provider, Model: f.model},
		nil
}

type fallbackTestProvider struct {
	provider        string
	model           string
	err             error
	calls           int
	waitFor         time.Duration
	remainingAtCall time.Duration
	onAnalyze       func()
}

func (p *fallbackTestProvider) ProviderName() string { return p.provider }
func (p *fallbackTestProvider) ModelName() string    { return p.model }
func (p *fallbackTestProvider) IsAvailable() bool    { return true }
func (p *fallbackTestProvider) Analyze(ctx context.Context, _ AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
	p.calls++
	if deadline, ok := ctx.Deadline(); ok {
		p.remainingAtCall = time.Until(deadline)
	}
	if p.waitFor > 0 {
		select {
		case <-time.After(p.waitFor):
		case <-ctx.Done():
			return AnalysisResponse{}, ModelCallMeta{Provider: p.provider, Model: p.model}, ctx.Err()
		}
	}
	if p.onAnalyze != nil {
		p.onAnalyze()
	}
	if p.err != nil {
		return AnalysisResponse{}, ModelCallMeta{Provider: p.provider, Model: p.model}, p.err
	}
	return AnalysisResponse{Action: ActionHold, Reasoning: "fallback"},
		ModelCallMeta{Provider: p.provider, Model: p.model}, nil
}

// withSpecialistBuildProvider swaps the package-level provider builder for the
// duration of a test. Restores the original on Cleanup.
func withSpecialistBuildProvider(t *testing.T, fn func(config.ModelConfig) ModelProvider) {
	t.Helper()
	prev := specialistBuildProvider
	specialistBuildProvider = fn
	t.Cleanup(func() { specialistBuildProvider = prev })
}

func TestAnalyzeSpecialistRoleUsesConfiguredFallbackForTransientGeminiError(t *testing.T) {
	primary := &fallbackTestProvider{
		provider: "google", model: "gemini-3.1-pro-preview",
		err: &geminiAPIError{StatusCode: 503, Body: "temporarily unavailable"}, waitFor: 20 * time.Millisecond,
	}
	fallback := &fallbackTestProvider{provider: "google", model: "gemini-2.5-flash"}
	withSpecialistBuildProvider(t, func(c config.ModelConfig) ModelProvider {
		if c.Model == fallback.model {
			return fallback
		}
		t.Fatalf("unexpected fallback model config: %+v", c)
		return nil
	})

	used, resp, meta, err := analyzeSpecialistRole(context.Background(), primary,
		&config.ModelConfig{Provider: "google", Model: fallback.model, APIKey: "test"}, 40*time.Millisecond,
		AnalysisRequest{Ticker: "AAPL"})
	if err != nil {
		t.Fatalf("fallback call failed: %v", err)
	}
	if primary.calls != 1 || fallback.calls != 1 {
		t.Fatalf("calls primary/fallback = %d/%d, want 1/1", primary.calls, fallback.calls)
	}
	if used != fallback || meta.Model != fallback.model || resp.Reasoning != "fallback" {
		t.Fatalf("fallback result provider=%v meta=%+v response=%+v", used, meta, resp)
	}
	if fallback.remainingAtCall < 29*time.Second {
		t.Fatalf("fallback received only %v of its 30s provider-default timeout", fallback.remainingAtCall)
	}
}

func TestAnalyzeSpecialistRoleDoesNotFallbackForNonTransientError(t *testing.T) {
	primary := &fallbackTestProvider{
		provider: "google", model: "gemini-3.1-pro-preview",
		err: errors.New("gemini truncated (finishReason=MAX_TOKENS)"),
	}
	fallbackBuilds := 0
	withSpecialistBuildProvider(t, func(config.ModelConfig) ModelProvider {
		fallbackBuilds++
		return &fallbackTestProvider{provider: "google", model: "gemini-2.5-flash"}
	})

	used, _, _, err := analyzeSpecialistRole(context.Background(), primary,
		&config.ModelConfig{Provider: "google", Model: "gemini-2.5-flash", APIKey: "test"}, time.Second,
		AnalysisRequest{Ticker: "AAPL"})
	if err == nil {
		t.Fatal("expected primary error")
	}
	if used != primary || fallbackBuilds != 0 {
		t.Fatalf("used=%v fallback builds=%d, want primary and no fallback", used, fallbackBuilds)
	}
}

func TestAnalyzeSpecialistRoleDoesNotFallbackAfterParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	primary := &fallbackTestProvider{
		provider: "google", model: "gemini-3.1-pro-preview",
		err:       &geminiAPIError{StatusCode: 429, Body: "rate limited"},
		onAnalyze: cancel,
	}
	fallbackBuilds := 0
	withSpecialistBuildProvider(t, func(config.ModelConfig) ModelProvider {
		fallbackBuilds++
		return &fallbackTestProvider{provider: "google", model: "gemini-2.5-flash"}
	})

	used, _, _, err := analyzeSpecialistRole(ctx, primary,
		&config.ModelConfig{Provider: "google", Model: "gemini-2.5-flash", APIKey: "test"}, time.Second,
		AnalysisRequest{Ticker: "AAPL"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want overall context cancellation", err)
	}
	if used != primary || fallbackBuilds != 0 {
		t.Fatalf("used=%v fallback builds=%d, want primary and no fallback", used, fallbackBuilds)
	}
}

// TestAnalyzeSpecialist_EmitsAnalysisStarted is the regression guard for the
// Copilot-flagged real bug: AnalyzeSpecialist must publish events.AnalysisStarted
// (mirroring AnalyzeFull) so the pipeline graph / WebSocket protocol can pulse
// the selected analyst nodes when specialist mode is on. The SelectedAnalysts
// list must reflect only the roles that actually run.
func TestAnalyzeSpecialist_EmitsAnalysisStarted(t *testing.T) {
	withSpecialistBuildProvider(t, func(c config.ModelConfig) ModelProvider {
		return &fakeRoleProvider{provider: c.Provider, model: c.Model}
	})

	bus := events.New()
	sub := bus.Subscribe(events.AnalysisStarted)

	registry := config.ModelRegistry{
		"analyst_frontier": {ModelConfig: config.ModelConfig{Provider: "anthropic", Model: "claude"}},
		"analyst_mid":      {ModelConfig: config.ModelConfig{Provider: "openai", Model: "gpt"}},
		"analyst_cheap":    {ModelConfig: config.ModelConfig{Provider: "deepseek", Model: "ds"}},
	}
	a := &Analyzer{
		cfg: config.ModelsConfig{Registry: registry},
		bus: bus,
		specialist: &config.AnalysisConfig{
			SpecialistRoles: map[string]string{
				"fundamental":  "analyst_frontier",
				"technical":    "analyst_mid",
				"sentiment":    "analyst_cheap",
				"risk_manager": "analyst_frontier",
			},
		},
	}

	if _, err := a.AnalyzeSpecialist(context.Background(), AnalysisRequest{Ticker: "AAPL"}, depth.ImpactScore{}); err != nil {
		t.Fatalf("AnalyzeSpecialist returned error: %v", err)
	}

	// Drain the directional fan-out and mandatory risk-review fan-in.
	var got []events.AnalysisStartedPayload
	timeout := time.After(500 * time.Millisecond)
loop:
	for {
		select {
		case ev := <-sub.C:
			p, ok := ev.Payload.(events.AnalysisStartedPayload)
			if !ok {
				t.Fatalf("unexpected payload type: %T", ev.Payload)
			}
			got = append(got, p)
		case <-timeout:
			break loop
		}
		if len(got) >= 2 {
			// Give one more tick to surface duplicates, then break.
			break
		}
	}
	// Brief drain for any duplicate.
	select {
	case ev := <-sub.C:
		got = append(got, ev.Payload.(events.AnalysisStartedPayload))
	case <-time.After(50 * time.Millisecond):
	}

	if len(got) != 2 {
		t.Fatalf("expected exactly 2 AnalysisStarted events, got %d (%+v)", len(got), got)
	}
	p := got[0]
	if p.Ticker != "AAPL" {
		t.Errorf("ticker = %q, want AAPL", p.Ticker)
	}
	if p.Depth != 3 {
		t.Errorf("depth = %d, want 3 directional roles", p.Depth)
	}
	want := []string{
		"specialist-fundamental",
		"specialist-technical",
		"specialist-sentiment",
	}
	if len(p.SelectedAnalysts) != len(want) {
		t.Fatalf("SelectedAnalysts length = %d, want %d (%v vs %v)",
			len(p.SelectedAnalysts), len(want), p.SelectedAnalysts, want)
	}
	// Order follows the directional specialist role order.
	for i := range want {
		if p.SelectedAnalysts[i] != want[i] {
			t.Errorf("SelectedAnalysts[%d] = %q, want %q (full: %v)",
				i, p.SelectedAnalysts[i], want[i], p.SelectedAnalysts)
		}
	}
	review := got[1]
	if review.Depth != 1 || len(review.SelectedAnalysts) != 1 ||
		review.SelectedAnalysts[0] != "specialist-risk-manager" {
		t.Fatalf("risk review dispatch = %+v; want mandatory specialist-risk-manager", review)
	}
	if len(review.SourceNodes) != 3 {
		t.Fatalf("risk review sources = %v; want all three directional specialists", review.SourceNodes)
	}
}

// TestAnalyzeSpecialist_RejectsMissingDirectionalRole verifies the sequential
// review never runs on an incomplete evidence set.
func TestAnalyzeSpecialist_RejectsMissingDirectionalRole(t *testing.T) {
	withSpecialistBuildProvider(t, func(c config.ModelConfig) ModelProvider {
		return &fakeRoleProvider{provider: c.Provider, model: c.Model}
	})

	bus := events.New()
	sub := bus.Subscribe(events.AnalysisStarted)

	// Sentiment maps to a missing key. The mandatory risk reviewer remains
	// configured, but it must not run without all three directional findings.
	registry := config.ModelRegistry{
		"analyst_frontier": {ModelConfig: config.ModelConfig{Provider: "anthropic", Model: "claude"}},
		"analyst_mid":      {ModelConfig: config.ModelConfig{Provider: "openai", Model: "gpt"}},
	}
	a := &Analyzer{
		cfg: config.ModelsConfig{Registry: registry},
		bus: bus,
		specialist: &config.AnalysisConfig{
			SpecialistRoles: map[string]string{
				"fundamental":  "analyst_frontier",
				"technical":    "analyst_mid",
				"sentiment":    "missing_key", // Resolve will fail
				"risk_manager": "analyst_frontier",
			},
		},
	}

	if _, err := a.AnalyzeSpecialist(context.Background(), AnalysisRequest{Ticker: "T"}, depth.ImpactScore{}); err == nil {
		t.Fatal("AnalyzeSpecialist succeeded with a missing directional role")
	} else if !strings.Contains(err.Error(), "all three are required") {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case ev := <-sub.C:
		t.Fatalf("unexpected AnalysisStarted for incomplete specialist set: %+v", ev.Payload)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAnalyzeSpecialist_RiskReviewerReceivesFindingsAndOwnsFinalOutcome(t *testing.T) {
	policy := prompts.RiskPolicy{MaxTickerExposurePct: 10, MaxPerTicker: 2, MinRiskReward: 1.5}
	risk := &captureRiskProvider{
		response: AnalysisResponse{
			Action: ActionWait, Confidence: 0.81,
			Reasoning: "directional evidence conflicts with portfolio exposure",
		},
	}
	directional := make(map[string]*fakeRoleProvider)
	withSpecialistBuildProvider(t, func(c config.ModelConfig) ModelProvider {
		if c.Model == "risk-model" {
			return risk
		}
		p := &fakeRoleProvider{provider: c.Provider, model: c.Model}
		directional[c.Model] = p
		return p
	})

	registry := config.ModelRegistry{
		"fund": {ModelConfig: config.ModelConfig{Provider: "p1", Model: "fund-model"}},
		"tech": {ModelConfig: config.ModelConfig{Provider: "p2", Model: "tech-model"}},
		"sent": {ModelConfig: config.ModelConfig{Provider: "p3", Model: "sent-model"}},
		"risk": {ModelConfig: config.ModelConfig{Provider: "risk-provider", Model: "risk-model"}},
	}
	a := &Analyzer{
		cfg: config.ModelsConfig{Registry: registry},
		specialist: &config.AnalysisConfig{SpecialistRoles: map[string]string{
			"fundamental": "fund", "technical": "tech", "sentiment": "sent", "risk_manager": "risk",
		}},
	}

	result, err := a.AnalyzeSpecialist(context.Background(), AnalysisRequest{
		Ticker: "AAPL", Strategy: "momentum", Timeframe: "1h", RiskPolicy: policy,
	}, depth.ImpactScore{})
	if err != nil {
		t.Fatalf("AnalyzeSpecialist returned error: %v", err)
	}
	if result.Response.Action != ActionWait {
		t.Fatalf("final action = %s, want mandatory risk review WAIT", result.Response.Action)
	}
	if result.Meta.Model != "risk-model" {
		t.Fatalf("final meta model = %q, want risk-model", result.Meta.Model)
	}
	if len(result.AnalystResults) != 4 || result.AnalystResults[3].Role != "risk_manager" {
		t.Fatalf("analyst results = %+v; want three directional roles followed by risk_manager", result.AnalystResults)
	}
	for _, role := range []string{"[fundamental]", "[technical]", "[sentiment]"} {
		if !strings.Contains(risk.request.CustomUserMessage, role) {
			t.Errorf("risk review prompt missing %s finding: %q", role, risk.request.CustomUserMessage)
		}
	}
	if !strings.Contains(result.Response.Reasoning, "mandatory risk-manager review") {
		t.Fatalf("final reasoning does not describe mandatory review: %q", result.Response.Reasoning)
	}
	for name, provider := range directional {
		if provider.request.RiskPolicy != policy || !strings.Contains(provider.request.CustomSystemPrompt, "exceed 10%") ||
			!strings.Contains(provider.request.CustomSystemPrompt, "If R:R < 1.5") {
			t.Errorf("%s lost configured risk policy in directional request", name)
		}
	}
	if risk.request.RiskPolicy != policy || !strings.Contains(risk.request.CustomSystemPrompt, "At most 2 open long positions") ||
		!strings.Contains(risk.request.CustomSystemPrompt, "If R:R < 1.5") {
		t.Error("risk reviewer lost configured risk policy")
	}
}

func TestAnalyzeSpecialistKeepsRiskReviewerRejectionsWithRiskSelectedLens(t *testing.T) {
	riskRejections := map[string]string{"trend_following": "late extension", "pullback": "no reclaim"}
	risk := &captureRiskProvider{response: AnalysisResponse{
		Action: ActionBuy, Confidence: .8, SizePct: 1, StopLoss: 90, TakeProfit: 110,
		SelectedStrategy: "momentum", Regime: "trending", RejectedStrategies: riskRejections,
	}}
	withSpecialistBuildProvider(t, func(c config.ModelConfig) ModelProvider {
		if c.Model == "risk-model" {
			return risk
		}
		return &fakeRoleProvider{provider: c.Provider, model: c.Model}
	})
	registry := config.ModelRegistry{
		"fund": {ModelConfig: config.ModelConfig{Provider: "p1", Model: "fund-model"}},
		"tech": {ModelConfig: config.ModelConfig{Provider: "p2", Model: "tech-model"}},
		"sent": {ModelConfig: config.ModelConfig{Provider: "p3", Model: "sent-model"}},
		"risk": {ModelConfig: config.ModelConfig{Provider: "risk-provider", Model: "risk-model"}},
	}
	a := &Analyzer{cfg: config.ModelsConfig{Registry: registry}, specialist: &config.AnalysisConfig{SpecialistRoles: map[string]string{
		"fundamental": "fund", "technical": "tech", "sentiment": "sent", "risk_manager": "risk",
	}}}
	result, err := a.AnalyzeSpecialist(context.Background(), AnalysisRequest{
		Ticker: "AAPL", Strategies: []string{"momentum", "trend_following", "pullback"},
	}, depth.ImpactScore{})
	if err != nil {
		t.Fatalf("AnalyzeSpecialist returned error: %v", err)
	}
	if result.Response.SelectedStrategy != "momentum" || len(result.Response.RejectedStrategies) != len(riskRejections) ||
		result.Response.RejectedStrategies["trend_following"] != "late extension" {
		t.Fatalf("risk-selected response lost its rejections: %+v", result.Response)
	}
}

func TestResolveSpecialistRiskDecision_BuyRequiresTwoDirectionalBuys(t *testing.T) {
	tests := []struct {
		name       string
		votes      []Action
		wantAction Action
		applied    bool
	}{
		{"one buy and wait becomes wait", []Action{ActionBuy, ActionWait, ActionHold}, ActionWait, true},
		{"one buy and holds becomes hold", []Action{ActionBuy, ActionHold, ActionHold}, ActionHold, true},
		{"two directional buys permit risk buy", []Action{ActionBuy, ActionBuy, ActionWait}, ActionBuy, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roles := []string{"fundamental", "technical", "sentiment"}
			results := make([]specialistResult, 0, 3)
			for i, action := range tt.votes {
				results = append(results, specialistResult{Role: roles[i], Response: AnalysisResponse{Action: action}})
			}
			risk := AnalysisResponse{Action: ActionBuy, Confidence: .8, SizePct: 3, StopLoss: 90, TakeProfit: 120}
			got, audit := resolveSpecialistRiskDecision(results, risk)
			if got.Action != tt.wantAction || audit.Applied != tt.applied {
				t.Fatalf("resolved = %s applied=%t; want %s applied=%t", got.Action, audit.Applied, tt.wantAction, tt.applied)
			}
			if tt.applied && (got.SizePct != 0 || got.StopLoss != 0 || got.TakeProfit != 0) {
				t.Fatalf("downgraded response retained BUY order fields: %+v", got)
			}
		})
	}
}

func TestResolveSpecialistRiskDecision_RiskSellIsUnrestricted(t *testing.T) {
	results := []specialistResult{
		{Role: "fundamental", Response: AnalysisResponse{Action: ActionHold}},
		{Role: "technical", Response: AnalysisResponse{Action: ActionWait}},
		{Role: "sentiment", Response: AnalysisResponse{Action: ActionHold}},
	}
	got, audit := resolveSpecialistRiskDecision(results, AnalysisResponse{Action: ActionSell})
	if got.Action != ActionSell || audit.Applied {
		t.Fatalf("risk SELL must be preserved, got %s audit=%+v", got.Action, audit)
	}
}

// TestModelTimeoutForRegistry_RegistryWins is the regression guard for the
// Copilot-flagged real bug: specialist roles map to registry keys that aren't
// in models.roles.analysts, so modelTimeout() silently returned the hardcoded
// 30s and ignored the per-registry timeout_sec. modelTimeoutForRegistry must
// honour cfg.Registry[key].TimeoutSec.
func TestModelTimeoutForRegistry_RegistryWins(t *testing.T) {
	cfg := config.ModelsConfig{
		Registry: config.ModelRegistry{
			"analyst_frontier": {
				ModelConfig: config.ModelConfig{
					Provider:   "anthropic",
					Model:      "claude",
					TimeoutSec: 60,
				},
			},
		},
		// Crucially: roles.analysts does NOT mention analyst_frontier, so the
		// existing modelTimeout() scan would miss it.
	}
	p := &fakeRoleProvider{provider: "anthropic", model: "claude"}
	got := modelTimeoutForRegistry("analyst_frontier", p, cfg)
	if got != 60*time.Second {
		t.Fatalf("modelTimeoutForRegistry = %v, want 60s (registry timeout_sec must win over the 30s default)", got)
	}
}

// TestModelTimeoutForRegistry_FallsBackToAnalystsConfig verifies that when the
// registry key is absent or has no TimeoutSec, the helper falls through to the
// analysts-config path (modelTimeout) — preserving existing behavior for the
// non-specialist callers.
func TestModelTimeoutForRegistry_FallsBackToAnalystsConfig(t *testing.T) {
	cfg := config.ModelsConfig{
		Analysts: []config.ModelConfig{
			{Provider: "openai", Model: "gpt", TimeoutSec: 45},
		},
	}
	p := &fakeRoleProvider{provider: "openai", model: "gpt"}
	got := modelTimeoutForRegistry("missing_key", p, cfg)
	if got != 45*time.Second {
		t.Fatalf("modelTimeoutForRegistry = %v, want 45s (analysts-config fallback)", got)
	}
}

// TestModelTimeoutForRegistry_FinalDefault verifies the 30s hardcoded default
// fires only when no registry entry and no analysts entry matches.
func TestModelTimeoutForRegistry_FinalDefault(t *testing.T) {
	cfg := config.ModelsConfig{}
	p := &fakeRoleProvider{provider: "anthropic", model: "claude"}
	got := modelTimeoutForRegistry("nope", p, cfg)
	if got != 30*time.Second {
		t.Fatalf("modelTimeoutForRegistry = %v, want 30s (final default)", got)
	}
}
