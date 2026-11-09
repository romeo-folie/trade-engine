package jev

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "ts-live-SECRET-KEY"

func fptr(v float64) *float64 { return &v }

func testClient(t *testing.T, url string, retries int) *Client {
	t.Helper()
	c, err := NewClient(ClientConfig{BaseURL: url, APIKey: testKey, Model: "jev-2026-10", Timeout: time.Second, MaxRetries: retries, Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var simpleQuestions = map[string]Question{
	"q": {Type: TypeNoul, Instructions: "Is it true?"},
}

func TestEvaluateSendsContractAndParsesAnswers(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"model":"jev-2026-10-resolved","answers":{"q":{"type":"noul","noul":0.8}},"usage":{"input_tokens":120,"output_tokens":3}}`))
	}))
	defer srv.Close()

	resp, meta, err := testClient(t, srv.URL, 0).Evaluate(context.Background(), map[string]any{"x": 1}, simpleQuestions, "qs/v1")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/systemone" || gotAuth != "Bearer "+testKey {
		t.Fatalf("path %q auth %q", gotPath, gotAuth)
	}
	if gotBody["model"] != "jev-2026-10" || gotBody["state"] == nil || gotBody["questions"] == nil {
		t.Fatalf("request body = %v", gotBody)
	}
	if a := resp.Answers["q"]; a.Noul == nil || *a.Noul != 0.8 {
		t.Fatalf("answer = %+v", a)
	}
	if meta.Provider != ProviderName || meta.Model != "jev-2026-10" || meta.ResolvedModel != "jev-2026-10-resolved" ||
		meta.InputTokens != 120 || meta.RawPrompt == "" || meta.PromptHash == "" || meta.RawResponse == "" ||
		meta.RequestParams["question_set_version"] != "qs/v1" {
		t.Fatalf("meta = %+v", meta)
	}
	if strings.Contains(meta.RawPrompt, testKey) || strings.Contains(meta.RawResponse, testKey) {
		t.Fatal("API key leaked into recorded request/response")
	}
}

func TestEvaluateRetriesTransientFailuresAndRecordsThem(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"detail":"overloaded"}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"m","answers":{"q":{"type":"noul","noul":0.1}},"usage":{}}`))
	}))
	defer srv.Close()
	_, meta, err := testClient(t, srv.URL, 2).Evaluate(context.Background(), "s", simpleQuestions, "v")
	if err != nil || calls.Load() != 2 {
		t.Fatalf("err %v calls %d", err, calls.Load())
	}
	if len(meta.Retries) != 1 || meta.Retries[0].StatusCode != 503 || !strings.Contains(meta.Retries[0].Error, "overloaded") {
		t.Fatalf("retries = %+v", meta.Retries)
	}
}

func TestEvaluateDoesNotRetryClientErrorsAndKeepsAttemptMetadata(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":[{"msg":"bad question"}]}`))
	}))
	defer srv.Close()
	_, meta, err := testClient(t, srv.URL, 3).Evaluate(context.Background(), "s", simpleQuestions, "v")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 422 || calls.Load() != 1 {
		t.Fatalf("err %v calls %d", err, calls.Load())
	}
	if meta.Provider != ProviderName || meta.RawPrompt == "" || meta.RawResponse != "" || meta.InputTokens != 0 {
		t.Fatalf("failure meta = %+v", meta)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatal("API key leaked into error")
	}
}

func TestEvaluateRetryExhaustionAndPerAttemptTimeout(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond) // exceeds the per-attempt timeout below
	}))
	defer srv.Close()
	c, _ := NewClient(ClientConfig{BaseURL: srv.URL, APIKey: testKey, Model: "m", Timeout: 10 * time.Millisecond, MaxRetries: 2, Backoff: time.Millisecond})
	_, meta, err := c.Evaluate(context.Background(), "s", simpleQuestions, "v")
	if err == nil || calls.Load() != 3 || len(meta.Retries) != 2 {
		t.Fatalf("err %v calls %d retries %d", err, calls.Load(), len(meta.Retries))
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatal("API key leaked into error")
	}
}

func TestEvaluateStopsRetryingWhenCallerContextEnds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c, _ := NewClient(ClientConfig{BaseURL: srv.URL, APIKey: testKey, Model: "m", Timeout: time.Second, MaxRetries: 5, Backoff: 200 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := c.Evaluate(ctx, "s", simpleQuestions, "v")
	if err == nil || calls.Load() != 1 || time.Since(start) > 150*time.Millisecond {
		t.Fatalf("err %v calls %d elapsed %v: a retry must not outlive the caller deadline", err, calls.Load(), time.Since(start))
	}
}

func TestNewClientRequiresModelAndKey(t *testing.T) {
	if _, err := NewClient(ClientConfig{APIKey: "k"}); err == nil {
		t.Fatal("missing model must be rejected")
	}
	if _, err := NewClient(ClientConfig{Model: "m"}); err == nil {
		t.Fatal("missing key must be rejected")
	}
}

func TestValidateAnswersRejectsMalformedResponses(t *testing.T) {
	qs := map[string]Question{
		"c": {Type: TypeChoice, Criteria: map[string]string{"a": "A", "b": "B"}},
		"s": {Type: TypeScore, Criteria: []string{"low", "mid", "high"}},
		"n": {Type: TypeNoul},
	}
	good := func() map[string]Answer {
		return map[string]Answer{
			"c": {Type: TypeChoice, Choice: "a", Confidence: fptr(0.7), Probabilities: map[string]float64{"a": 0.7, "b": 0.3}},
			"s": {Type: TypeScore, Score: fptr(1.4), Confidence: fptr(0.5), Probabilities: map[string]float64{"0": 0.1, "1": 0.4, "2": 0.5}},
			"n": {Type: TypeNoul, Noul: fptr(0.2)},
		}
	}
	if err := ValidateAnswers(qs, good()); err != nil {
		t.Fatalf("valid answers rejected: %v", err)
	}
	nan := 0.0
	nan = nan / nan
	cases := map[string]func(map[string]Answer){
		"unanswered":      func(a map[string]Answer) { delete(a, "n") },
		"wrong type":      func(a map[string]Answer) { x := a["n"]; x.Type = TypeChoice; a["n"] = x },
		"unknown choice":  func(a map[string]Answer) { x := a["c"]; x.Choice = "z"; a["c"] = x },
		"unknown option":  func(a map[string]Answer) { x := a["c"]; x.Probabilities = map[string]float64{"a": 0.7, "z": 0.3}; a["c"] = x },
		"bad sum":         func(a map[string]Answer) { x := a["c"]; x.Probabilities = map[string]float64{"a": 0.7, "b": 0.7}; a["c"] = x },
		"nan probability": func(a map[string]Answer) { x := a["c"]; x.Probabilities = map[string]float64{"a": nan, "b": 0.3}; a["c"] = x },
		"noul range":      func(a map[string]Answer) { a["n"] = Answer{Type: TypeNoul, Noul: fptr(1.2)} },
		"score range":     func(a map[string]Answer) { x := a["s"]; x.Score = fptr(2.5); a["s"] = x },
		"missing conf":    func(a map[string]Answer) { x := a["c"]; x.Confidence = nil; a["c"] = x },
	}
	for name, mutate := range cases {
		a := good()
		mutate(a)
		if err := ValidateAnswers(qs, a); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
}
