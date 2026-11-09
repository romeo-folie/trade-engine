package analysis

// Issue #589: resolution must be a pure function of the recorded analyst
// answers. These tests pin latency/arrival-order independence, the seeded
// arbiter permutation, persisted failure reasons, and the decision clock.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
)

func identityResults() []AnalystResult {
	mk := func(ordinal int, provider string, action Action, conf, size, sl, tp float64, reason string, rejected map[string]string) AnalystResult {
		return AnalystResult{
			Provider: provider, Model: provider + "-m", Ordinal: ordinal,
			Response: AnalysisResponse{
				Action: action, Confidence: conf, SizePct: size, StopLoss: sl, TakeProfit: tp,
				Reasoning: provider + " reasoning", DecisiveReason: reason,
				SelectedStrategy: "momentum", Regime: "trending", RejectedStrategies: rejected,
			},
		}
	}
	return []AnalystResult{
		mk(0, "alpha", ActionBuy, 0.70, 2.0, 95, 120, "alpha decisive", map[string]string{"news_sentiment": "alpha reason"}),
		mk(1, "beta", ActionBuy, 0.72, 3.0, 96, 125, "beta decisive", map[string]string{"news_sentiment": "beta reason"}),
		mk(2, "gamma", ActionHold, 0.60, 0, 0, 0, "gamma decisive", nil),
	}
}

// permuteWithLatency reorders results and assigns latencies so the slowest
// analyst arrives first — the worst case for the former latency sort.
func permuteWithLatency(in []AnalystResult, order []int, latencies []int64) []AnalystResult {
	out := make([]AnalystResult, len(order))
	for i, idx := range order {
		out[i] = in[idx]
		out[i].LatencyMs = latencies[i]
	}
	return out
}

func TestResolutionIsIndependentOfLatencyAndArrivalOrder(t *testing.T) {
	base := identityResults()
	variants := [][]AnalystResult{
		permuteWithLatency(base, []int{0, 1, 2}, []int64{10, 20, 30}),
		permuteWithLatency(base, []int{2, 1, 0}, []int64{5, 900, 40}),
		permuteWithLatency(base, []int{1, 0, 2}, []int64{1, 2000, 3}),
	}

	want, ok := majorityVote(variants[0])
	if !ok {
		t.Fatal("expected a majority")
	}
	// Reference plan and first-wins evidence come from the lowest ordinal BUY.
	if want.SizePct != 2.0 || want.StopLoss != 95 || want.TakeProfit != 120 {
		t.Fatalf("majority reference plan = %+v, want alpha's plan", want)
	}
	if want.DecisiveReason != "alpha decisive" || want.RejectedStrategies["news_sentiment"] != "alpha reason" {
		t.Fatalf("first-wins evidence not from identity order: %+v", want)
	}
	for i, v := range variants[1:] {
		got, _ := majorityVote(v)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("variant %d majority differs:\n got %+v\nwant %+v", i+1, got, want)
		}
	}

	unanimous := identityResults()[:2]
	fpWant, ok := TryFastPath(permuteWithLatency(unanimous, []int{0, 1}, []int64{50, 1}), 0.6, 0.5)
	if !ok {
		t.Fatal("expected fast path")
	}
	fpGot, _ := TryFastPath(permuteWithLatency(unanimous, []int{1, 0}, []int64{1, 50}), 0.6, 0.5)
	if !reflect.DeepEqual(fpGot, fpWant) {
		t.Fatalf("fast path depends on arrival order:\n got %+v\nwant %+v", fpGot, fpWant)
	}
	if fpWant.DecisiveReason != "alpha decisive" {
		t.Fatalf("fast-path decisive reason = %q, want identity-first", fpWant.DecisiveReason)
	}
}

func TestArbiterPromptIsReproducibleAndRecordsOrdering(t *testing.T) {
	run := func(results []AnalystResult) (string, ArbiterResult) {
		cap := &captureArbiter{response: AnalysisResponse{Action: ActionBuy, Confidence: 0.7, Reasoning: "ok"}}
		res, err := Arbitrate(context.Background(), cap, AnalysisRequest{Ticker: "TEST"}, results)
		if err != nil {
			t.Fatalf("Arbitrate: %v", err)
		}
		return cap.captured, res
	}
	base := identityResults()
	p1, r1 := run(permuteWithLatency(base, []int{0, 1, 2}, []int64{1, 2, 3}))
	p2, r2 := run(permuteWithLatency(base, []int{2, 0, 1}, []int64{1, 300, 90}))
	if p1 != p2 {
		t.Fatalf("arbiter prompt not reproducible for identical answers:\n%s\n---\n%s", p1, p2)
	}
	if !reflect.DeepEqual(r1.Ordering, r2.Ordering) || len(r1.Ordering) != 3 {
		t.Fatalf("ordering not stable: %+v vs %+v", r1.Ordering, r2.Ordering)
	}
	if r1.PolicyVersion != ResolutionPolicyVersion {
		t.Fatalf("policy version = %q", r1.PolicyVersion)
	}
	// Every label in the recorded ordering must denote exactly that analyst's
	// block in the prompt.
	for _, slot := range r1.Ordering {
		block := p1[strings.Index(p1, "--- "+slot.Label+" ---"):]
		if next := strings.Index(block[4:], "--- Model-"); next >= 0 {
			block = block[:next+4]
		}
		if !strings.Contains(block, slot.Provider+" reasoning") {
			t.Fatalf("label %s does not map to %s in prompt block %q", slot.Label, slot.Provider, block)
		}
	}

	// Different answers must be able to produce a different permutation:
	// otherwise the order would be fixed, not anonymising.
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		v := identityResults()
		v[0].Response.Reasoning = fmt.Sprintf("variant %d", i)
		_, r := run(v)
		key := ""
		for _, s := range r.Ordering {
			key += s.Provider + ","
		}
		seen[key] = true
	}
	if len(seen) < 2 {
		t.Fatalf("arbiter permutation never varies across answers: %v", seen)
	}
}

func TestAnalystResultPersistsErrorText(t *testing.T) {
	r := AnalystResult{Provider: "p", Model: "m", Role: "technical", Ordinal: 2, LatencyMs: 7,
		Err: fmt.Errorf("wrap: %w", errors.New("deadline exceeded"))}
	raw, err := json.Marshal([]AnalystResult{r, {Provider: "ok", Model: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded[0]["Err"] != "wrap: deadline exceeded" {
		t.Fatalf("Err = %#v, want message string; raw %s", decoded[0]["Err"], raw)
	}
	if _, present := decoded[1]["Err"]; present {
		t.Fatalf("successful result must omit Err; raw %s", raw)
	}
	// Keys consumed by the ensemble_results SQL views and web mirror.
	for _, key := range []string{"Provider", "Model", "Role", "Response", "Meta", "LatencyMs", "Ordinal"} {
		if _, ok := decoded[0][key]; !ok {
			t.Fatalf("missing key %q in %s", key, raw)
		}
	}
}

// arrivalAnalyst finishes after a configured delay so goroutine completion
// order is the reverse of configured order.
type arrivalAnalyst struct {
	name  string
	delay time.Duration
}

func (a *arrivalAnalyst) Analyze(ctx context.Context, _ AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
	select {
	case <-time.After(a.delay):
	case <-ctx.Done():
		return AnalysisResponse{}, ModelCallMeta{}, ctx.Err()
	}
	return AnalysisResponse{Action: ActionBuy, Confidence: 0.7, Reasoning: a.name}, ModelCallMeta{Provider: a.name, Model: "m"}, nil
}
func (a *arrivalAnalyst) ProviderName() string { return a.name }
func (a *arrivalAnalyst) ModelName() string    { return "m" }
func (a *arrivalAnalyst) IsAvailable() bool    { return true }

func TestRunAnalystSetReturnsConfiguredOrder(t *testing.T) {
	an := &Analyzer{analysts: []ModelProvider{
		&arrivalAnalyst{name: "first", delay: 60 * time.Millisecond},
		&arrivalAnalyst{name: "second", delay: 30 * time.Millisecond},
		&arrivalAnalyst{name: "third", delay: 0},
	}}
	results, err := an.AnalyzeEnsembleWithSet(context.Background(), AnalysisRequest{Ticker: "X"}, an.analysts)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"first", "second", "third"} {
		if results[i].Provider != want || results[i].Ordinal != i {
			t.Fatalf("results[%d] = %s/%d, want %s/%d", i, results[i].Provider, results[i].Ordinal, want, i)
		}
	}
}

func TestPromptNewsAgeUsesDecisionCutoff(t *testing.T) {
	cutoff := time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC)
	req := AnalysisRequest{
		News: []NewsSnippet{{Headline: "Guidance raised", Source: "wire",
			PublishedAt: cutoff.Add(-5 * time.Hour), Sentiment: "bullish"}},
		DecisionCutoff: cutoff,
	}
	items := toPromptNews(req.News, req.DecisionCutoff)
	render := func() string {
		_, user := prompts.BuildStockPrompt("X", nil, "1d", nil, prompts.Indicators{}, items,
			prompts.Portfolio{}, "", nil, "", false)
		return user
	}
	first := render()
	if !strings.Contains(first, "| 5h ago] Guidance raised") {
		t.Fatalf("news age not measured from cutoff:\n%s", first)
	}
	if second := render(); second != first {
		t.Fatal("prompt re-render with the same cutoff is not byte-identical")
	}

	// Zero cutoff keeps legacy wall-clock behaviour (article is years old).
	legacy := toPromptNews(req.News, time.Time{})
	_, user := prompts.BuildStockPrompt("X", nil, "1d", nil, prompts.Indicators{}, legacy,
		prompts.Portfolio{}, "", nil, "", false)
	if strings.Contains(user, "| 5h ago]") {
		t.Fatal("zero cutoff must fall back to the wall clock")
	}
}

// TestConcurrentArbitrateIsRaceFree exercises the seeded permutation from
// many goroutines (the former global rand.Perm had shared state).
func TestConcurrentArbitrateIsRaceFree(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = arbiterPermutation("X", identityResults())
		}()
	}
	wg.Wait()
}
