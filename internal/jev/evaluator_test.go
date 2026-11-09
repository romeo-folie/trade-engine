package jev

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/analysis"
)

func fakeSystemOne(t *testing.T, answers map[string]Answer, seen *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if seen != nil {
			_ = json.Unmarshal(raw, seen)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev-2026-10", "answers": answers,
			"usage": map[string]int{"input_tokens": 900, "output_tokens": 0}})
	}))
}

func TestEvaluatorEndToEndProducesEngineDecision(t *testing.T) {
	var seen map[string]any
	srv := fakeSystemOne(t, qualifyingAnswers(), &seen)
	defer srv.Close()
	e := &Evaluator{Client: testClient(t, srv.URL, 0), Policy: DefaultPolicyConfig(), MinRiskReward: 1.5}

	d, err := e.Evaluate(context.Background(), entryRequest())
	if err != nil {
		t.Fatal(err)
	}
	if d.Response.Action != analysis.ActionBuy || d.Meta.InputTokens != 900 || d.Trace.QuestionSetVersion != QuestionSetVersion {
		t.Fatalf("decision = %+v", d)
	}
	// The request carries the frozen snapshot inputs as state, and exactly
	// the v1 entry questions.
	state, _ := seen["state"].(map[string]any)
	inputs, _ := state["inputs"].(map[string]any)
	if inputs["ticker"] != "AAPL" || inputs["decision_cutoff"] == nil {
		t.Fatalf("state = %v", state)
	}
	qs, _ := seen["questions"].(map[string]any)
	for _, q := range []string{QThesis, QAdverse, QLens, QTiming, QRegime} {
		if _, ok := qs[q]; !ok {
			t.Fatalf("question %s missing from request: %v", q, qs)
		}
	}
	if _, ok := qs[QExit]; ok {
		t.Fatal("an unheld ticker must not be asked the exit question")
	}
}

func TestEvaluatorMalformedAnswersAreAnErrorWithEvidence(t *testing.T) {
	answers := qualifyingAnswers()
	delete(answers, QLens)
	srv := fakeSystemOne(t, answers, nil)
	defer srv.Close()
	e := &Evaluator{Client: testClient(t, srv.URL, 0), Policy: DefaultPolicyConfig()}
	d, err := e.Evaluate(context.Background(), entryRequest())
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
	if d.Meta.RawResponse == "" || d.Response.Action != "" {
		t.Fatalf("malformed decision must keep the raw response and carry no action: %+v", d)
	}
}
