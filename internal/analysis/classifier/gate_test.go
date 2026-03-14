package classifier_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/analysis/classifier"
	"github.com/romeo-folie/trade-engine/internal/config"
)

func newTestLLMServer(reply string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		resp := map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]string{"content": reply}},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func makeGate(mode string, llm *classifier.LLMClassifier) *classifier.Gate {
	cfg := testCfg()
	cfg.Mode = mode
	rb := classifier.NewRuleBased(cfg)
	return classifier.NewGate(mode, rb, llm)
}

func TestGateShadowModeDoesNotBlock(t *testing.T) {
	g := makeGate("shadow", nil)
	s := base()
	s.RSIDelta = 0    // all below threshold → WOULD_SKIP
	s.NewsHash = s.LastNewsHash

	proceed, res, err := g.Classify(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !proceed {
		t.Error("shadow mode must never block analysis")
	}
	// Result reflects SKIP decision but proceed=true.
	if res.Decision != classifier.DecisionSkip {
		t.Errorf("expected SKIP decision in shadow, got %v", res.Decision)
	}
}

func TestGateActiveModeBlocks(t *testing.T) {
	g := makeGate("active", nil)
	s := base()
	// all below threshold → SKIP
	s.RSIDelta = 0
	s.EMAGapDelta = 0
	s.PriceMovesPct = 0
	s.NewsHash = s.LastNewsHash

	proceed, res, _ := g.Classify(context.Background(), s)
	if proceed {
		t.Error("active mode should block when SKIP")
	}
	if res.Decision != classifier.DecisionSkip {
		t.Errorf("expected SKIP, got %v", res.Decision)
	}
}

func TestGateActiveModeAnalyze(t *testing.T) {
	g := makeGate("active", nil)
	s := base()
	s.RSIDelta = 5.0 // above threshold

	proceed, _, _ := g.Classify(context.Background(), s)
	if !proceed {
		t.Error("active mode should allow analysis when threshold exceeded")
	}
}

func TestGateDisabledMode(t *testing.T) {
	g := makeGate("disabled", nil)
	s := base()
	// Even with all zeros, disabled mode always proceeds.
	proceed, res, _ := g.Classify(context.Background(), s)
	if !proceed {
		t.Error("disabled mode must always proceed")
	}
	if res.ReasonCode != "CLASSIFIER_DISABLED" {
		t.Errorf("expected CLASSIFIER_DISABLED reason, got %q", res.ReasonCode)
	}
}

func TestGateLLMSkipInActiveMode(t *testing.T) {
	srv := newTestLLMServer("SKIP low activity consolidation", http.StatusOK)
	defer srv.Close()

	modelCfg := config.ModelConfig{Provider: "openai", Model: "gpt-4o-mini", APIKey: "test"}
	llm := classifier.NewLLMClassifier(testCfg(), modelCfg)
	llm.SetBaseURL(srv.URL)

	// Layer 1 returns NO_MATERIAL_CHANGE → Layer 2 consulted.
	g := classifier.NewGate("active", classifier.NewRuleBased(testCfg()), llm)
	s := base()
	s.RSIDelta = 0
	s.EMAGapDelta = 0
	s.PriceMovesPct = 0
	s.NewsHash = s.LastNewsHash

	proceed, _, _ := g.Classify(context.Background(), s)
	if proceed {
		t.Error("expected LLM SKIP to block analysis in active mode")
	}
}

func TestGateLLMErrorFailOpen(t *testing.T) {
	// Server returns 500 → fail open.
	srv := newTestLLMServer("", http.StatusInternalServerError)
	defer srv.Close()

	modelCfg := config.ModelConfig{Provider: "openai", Model: "gpt-4o-mini", APIKey: "test"}
	llm := classifier.NewLLMClassifier(testCfg(), modelCfg)
	llm.SetBaseURL(srv.URL)

	g := classifier.NewGate("active", classifier.NewRuleBased(testCfg()), llm)
	s := base()
	s.RSIDelta = 0
	s.EMAGapDelta = 0
	s.PriceMovesPct = 0
	s.NewsHash = s.LastNewsHash

	proceed, _, _ := g.Classify(context.Background(), s)
	if !proceed {
		t.Error("LLM error must fail open (proceed to analysis)")
	}
}

func TestGateUnknownModeNormalisedToShadow(t *testing.T) {
	// A typo like "actve" must not silently become active gating — it should
	// fall back to shadow (never blocks) with a warning.
	g := makeGate("actve", nil) // intentional typo
	s := base()
	s.RSIDelta = 0 // all below threshold → WOULD_SKIP in shadow

	proceed, _, _ := g.Classify(context.Background(), s)
	if !proceed {
		t.Error("unknown mode must normalise to shadow and never block analysis")
	}
}
