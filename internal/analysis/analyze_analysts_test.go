package analysis

import (
	"context"
	"errors"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/analysis/depth"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
)

// cfgAnalyst is a fully controllable fake: fixed response, optional error, and
// configurable availability + call counting.
type cfgAnalyst struct {
	name  string
	resp  AnalysisResponse
	err   error
	avail bool
	calls int
}

func (c *cfgAnalyst) Analyze(_ context.Context, _ AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
	c.calls++
	return c.resp, ModelCallMeta{Provider: c.name, Model: "m"}, c.err
}
func (c *cfgAnalyst) ProviderName() string { return c.name }
func (c *cfgAnalyst) ModelName() string    { return "m" }
func (c *cfgAnalyst) IsAvailable() bool    { return c.avail }

func TestAnalyzeFullLegacyIDsMatchTopologyAtEveryDepth(t *testing.T) {
	for _, tt := range []struct {
		name  string
		depth int
		want  []string
	}{
		{name: "adaptive depth one", depth: 1, want: []string{"llm_analysis"}},
		{name: "ensemble", depth: 2, want: []string{"llm_analysis", "analyst-1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bus := events.New()
			sub := bus.Subscribe(events.AnalysisStarted)
			defer bus.Unsubscribe(sub)
			an := &Analyzer{
				analysts: []ModelProvider{
					&cfgAnalyst{name: "one", resp: AnalysisResponse{Action: ActionBuy, Confidence: 0.8}, avail: true},
					&cfgAnalyst{name: "two", resp: AnalysisResponse{Action: ActionBuy, Confidence: 0.8}, avail: true},
				},
				cfg: config.ModelsConfig{Consensus: config.ConsensusConfig{Enabled: true}},
				bus: bus,
			}

			if _, err := an.AnalyzeFull(context.Background(), AnalysisRequest{Ticker: "X"}, depth.ImpactScore{Depth: tt.depth}); err != nil {
				t.Fatalf("AnalyzeFull() error = %v", err)
			}
			ev := <-sub.C
			got := ev.Payload.(events.AnalysisStartedPayload).SelectedAnalysts
			if len(got) != len(tt.want) {
				t.Fatalf("selected analysts = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("selected analysts = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// TestAnalyzeAnalysts_OneAvailableNoArbiter: the ORB scan must keep scoring with a
// SINGLE reachable analyst and must never consult the arbiter.
func TestAnalyzeAnalysts_OneAvailableNoArbiter(t *testing.T) {
	cap := &captureArbiter{}
	solo := &cfgAnalyst{name: "solo", resp: AnalysisResponse{Action: ActionBuy, Confidence: 0.72}, avail: true}
	down := &cfgAnalyst{name: "down", avail: false} // unavailable → excluded, never called
	an := &Analyzer{analysts: []ModelProvider{solo, down}, arbiter: cap}

	results, err := an.AnalyzeAnalysts(context.Background(), AnalysisRequest{Ticker: "X"}, depth.ImpactScore{Depth: 3})
	if err != nil {
		t.Fatalf("one available analyst must succeed, got %v", err)
	}
	if len(successfulResults(results)) != 1 {
		t.Fatalf("expected 1 successful result, got %+v", results)
	}
	if cap.captured != "" {
		t.Fatalf("arbiter must NOT be invoked, captured=%q", cap.captured)
	}
	if down.calls != 0 {
		t.Errorf("unavailable analyst must not be called, calls=%d", down.calls)
	}
}

// TestAnalyzeAnalysts_SurvivesPartialFailure: with providers throttled/out-of-credits,
// a single success is enough — no ≥2 floor and no arbiter dependency.
func TestAnalyzeAnalysts_SurvivesPartialFailure(t *testing.T) {
	cap := &captureArbiter{}
	ok := &cfgAnalyst{name: "ok", resp: AnalysisResponse{Action: ActionBuy, Confidence: 0.70}, avail: true}
	bad1 := &cfgAnalyst{name: "bad1", err: errors.New("429 too many requests"), avail: true}
	bad2 := &cfgAnalyst{name: "bad2", err: errors.New("credit balance too low"), avail: true}
	an := &Analyzer{analysts: []ModelProvider{ok, bad1, bad2}, arbiter: cap}

	results, err := an.AnalyzeAnalysts(context.Background(), AnalysisRequest{Ticker: "X"}, depth.ImpactScore{Depth: 3})
	if err != nil {
		t.Fatalf("≥1 success must not error (unlike the ≥2 ensemble), got %v", err)
	}
	if len(successfulResults(results)) != 1 {
		t.Fatalf("expected 1 success among 3, got %d", len(successfulResults(results)))
	}
	if cap.captured != "" {
		t.Fatalf("arbiter must NOT be invoked, captured=%q", cap.captured)
	}
}

// TestAnalyzeAnalysts_AllFailErrors: only a TOTAL wipeout errors (the caller then skips
// that candidate) — distinct from the partial-failure case above.
func TestAnalyzeAnalysts_AllFailErrors(t *testing.T) {
	bad1 := &cfgAnalyst{name: "bad1", err: errors.New("x"), avail: true}
	bad2 := &cfgAnalyst{name: "bad2", err: errors.New("y"), avail: true}
	an := &Analyzer{analysts: []ModelProvider{bad1, bad2}}
	if _, err := an.AnalyzeAnalysts(context.Background(), AnalysisRequest{Ticker: "X"}, depth.ImpactScore{Depth: 3}); err == nil {
		t.Fatal("expected an error when every analyst fails")
	}
}

// TestAnalyzeAnalysts_NoneAvailableErrors: zero configured/ready analysts errors before
// any call.
func TestAnalyzeAnalysts_NoneAvailableErrors(t *testing.T) {
	down := &cfgAnalyst{name: "down", avail: false}
	an := &Analyzer{analysts: []ModelProvider{down}}
	if _, err := an.AnalyzeAnalysts(context.Background(), AnalysisRequest{Ticker: "X"}, depth.ImpactScore{Depth: 3}); err == nil {
		t.Fatal("expected an error when no analysts are available")
	}
	if down.calls != 0 {
		t.Errorf("unavailable analyst must not be called")
	}
}

// TestAnalyzeAnalysts_DepthCapsAnalysts: Depth < len(analysts) uses only the first Depth.
func TestAnalyzeAnalysts_DepthCapsAnalysts(t *testing.T) {
	a1 := &cfgAnalyst{name: "a1", resp: AnalysisResponse{Action: ActionBuy, Confidence: 0.7}, avail: true}
	a2 := &cfgAnalyst{name: "a2", resp: AnalysisResponse{Action: ActionBuy, Confidence: 0.7}, avail: true}
	a3 := &cfgAnalyst{name: "a3", resp: AnalysisResponse{Action: ActionBuy, Confidence: 0.7}, avail: true}
	an := &Analyzer{analysts: []ModelProvider{a1, a2, a3}}
	if _, err := an.AnalyzeAnalysts(context.Background(), AnalysisRequest{Ticker: "X"}, depth.ImpactScore{Depth: 2}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a1.calls != 1 || a2.calls != 1 || a3.calls != 0 {
		t.Errorf("Depth 2 should call only the first 2 analysts: a1=%d a2=%d a3=%d", a1.calls, a2.calls, a3.calls)
	}
}
