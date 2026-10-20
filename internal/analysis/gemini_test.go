package analysis

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func validGeminiResp(text string) map[string]any {
	return map[string]any{
		"candidates": []map[string]any{
			{
				"content": map[string]any{
					"parts": []map[string]any{{"text": text}},
				},
			},
		},
		"usageMetadata": map[string]any{
			"promptTokenCount":     10,
			"candidatesTokenCount": 20,
		},
	}
}

func TestGeminiClient_Metadata(t *testing.T) {
	cfg := config.ModelConfig{Model: "gemini-pro", APIKey: "test-key"}
	c := NewGeminiClient(cfg)
	if c.ProviderName() != "google" {
		t.Errorf("ProviderName = %q, want google", c.ProviderName())
	}
	if c.ModelName() != "gemini-pro" {
		t.Errorf("ModelName = %q, want gemini-pro", c.ModelName())
	}
	if !c.IsAvailable() {
		t.Error("IsAvailable should be true with non-empty API key")
	}
}

func TestGeminiClient_IsAvailable_NoKey(t *testing.T) {
	cfg := config.ModelConfig{Model: "gemini-pro"}
	c := NewGeminiClient(cfg)
	if c.IsAvailable() {
		t.Error("IsAvailable should be false with empty API key")
	}
}

func TestGeminiClient_Analyze_Success(t *testing.T) {
	payload := `{"action":"BUY","confidence":0.85,"size_pct":2.0,"stop_loss":130.0,"take_profit":150.0,"reasoning":"bullish breakout"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// verify auth header
		if r.Header.Get("x-goog-api-key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// verify URL contains model name
		if !strings.Contains(r.URL.Path, "gemini-pro") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(validGeminiResp(payload))
	}))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "gemini-pro", APIKey: "test-key"}
	c := NewGeminiClient(cfg)
	c.baseURL = srv.URL

	resp, meta, err := c.Analyze(context.Background(), AnalysisRequest{
		Ticker:    "AAPL",
		Strategy:  "momentum",
		Timeframe: "1d",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Action != ActionBuy {
		t.Errorf("action = %q, want BUY", resp.Action)
	}
	if resp.Confidence != 0.85 {
		t.Errorf("confidence = %.2f, want 0.85", resp.Confidence)
	}
	if meta.Provider != "google" {
		t.Errorf("meta.Provider = %q, want google", meta.Provider)
	}
	if meta.InputTokens != 10 || meta.OutputTokens != 20 {
		t.Errorf("token counts = %d/%d, want 10/20", meta.InputTokens, meta.OutputTokens)
	}
}

func TestGeminiClient_Analyze_RetriesOn429(t *testing.T) {
	payload := `{"action":"HOLD","confidence":0.5,"size_pct":0,"stop_loss":0,"take_profit":0,"reasoning":"x"}`
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"retryDelay":"0.01s"}]}}`))
			return
		}
		json.NewEncoder(w).Encode(validGeminiResp(payload))
	}))
	defer srv.Close()

	// High RPM so the limiter never adds delay; we're isolating the retry path.
	cfg := config.ModelConfig{Model: "gemini-2.5-flash", APIKey: "test-key", RequestsPerMinute: 600000}
	c := NewGeminiClient(cfg)
	c.baseURL = srv.URL

	resp, _, err := c.Analyze(context.Background(), AnalysisRequest{Ticker: "AAPL"})
	if err != nil {
		t.Fatalf("expected retry to succeed, got: %v", err)
	}
	if resp.Action != ActionHold {
		t.Errorf("action = %q, want HOLD", resp.Action)
	}
	if calls != 2 {
		t.Errorf("server hit %d times, want 2 (one 429 + one success)", calls)
	}
}

func TestGeminiClient_Analyze_429GivesUpWhenDelayExceedsDeadline(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		// 30s requested delay cannot fit a 100ms context deadline → no retry.
		w.Write([]byte(`{"error":{"code":429,"details":[{"retryDelay":"30s"}]}}`))
	}))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "gemini-2.5-flash", APIKey: "test-key", RequestsPerMinute: 600000}
	c := NewGeminiClient(cfg)
	c.baseURL = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := c.Analyze(ctx, AnalysisRequest{Ticker: "AAPL"})
	if err == nil {
		t.Fatal("expected error when retry delay exceeds deadline")
	}
	if !strings.Contains(err.Error(), "gemini API 429") {
		t.Errorf("error = %q, want gemini API 429", err.Error())
	}
	if calls != 1 {
		t.Errorf("server hit %d times, want 1 (no retry attempted)", calls)
	}
}

func TestGeminiClient_Analyze_PreservesTransientErrorWhenRetryLimiterTimesOut(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"details":[{"retryDelay":"0.01s"}]}}`))
	}))
	defer srv.Close()

	// Use a unique limiter key: model limiters are shared process-wide and earlier
	// tests may have registered the real Gemini model at a much higher RPM. The
	// first call is immediate; after retryDelay, the six-second slot is still
	// beyond this deadline, which exceeds retryMargin so the retry reaches wait().
	cfg := config.ModelConfig{Model: "gemini-2.5-flash-limiter-timeout-test", APIKey: "test-key", RequestsPerMinute: 10}
	c := NewGeminiClient(cfg)
	c.baseURL = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := c.Analyze(ctx, AnalysisRequest{Ticker: "AAPL"})
	if err == nil {
		t.Fatal("expected error when retry limiter exceeds the primary deadline")
	}
	var apiErr *geminiAPIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("error = %T %v, want typed status 429", err, err)
	}
	if !shouldUseGeminiFallback(err) {
		t.Fatalf("retry-limiter timeout must preserve fallback eligibility: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("server hit %d times, want 1 (retry blocked by limiter)", got)
	}
}

func TestGeminiClient_Analyze_HTTP4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("invalid api key"))
	}))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "gemini-pro", APIKey: "bad-key"}
	c := NewGeminiClient(cfg)
	c.baseURL = srv.URL

	_, _, err := c.Analyze(context.Background(), AnalysisRequest{Ticker: "AAPL"})
	if err == nil {
		t.Fatal("expected error on 4xx")
	}
	if !strings.Contains(err.Error(), "gemini API 401") {
		t.Errorf("error = %q, want gemini API 401", err.Error())
	}
}

func TestGeminiClient_Analyze_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(validGeminiResp("not-valid-json"))
	}))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "gemini-pro", APIKey: "test-key"}
	c := NewGeminiClient(cfg)
	c.baseURL = srv.URL

	// use __extraction__ ticker to suppress warn log noise
	_, _, err := c.Analyze(context.Background(), AnalysisRequest{Ticker: "__extraction__"})
	if err == nil {
		t.Fatal("expected JSON extraction error")
	}
}

// geminiRespWithFinish builds a response with an explicit finishReason and a
// thoughtsTokenCount, for exercising the thinking/truncation paths.
func geminiRespWithFinish(text, finishReason string, thoughts int) map[string]any {
	return map[string]any{
		"candidates": []map[string]any{
			{
				"finishReason": finishReason,
				"content": map[string]any{
					"parts": []map[string]any{{"text": text}},
				},
			},
		},
		"usageMetadata": map[string]any{
			"promptTokenCount":     10,
			"candidatesTokenCount": 20,
			"thoughtsTokenCount":   thoughts,
		},
	}
}

// captureGeminiBody returns a server that records the request body and replies
// with the given canned response, plus a channel to read the captured body.
func captureGeminiBody(t *testing.T, resp map[string]any) (*httptest.Server, <-chan map[string]any) {
	t.Helper()
	bodyCh := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		bodyCh <- body
		json.NewEncoder(w).Encode(resp)
	}))
	return srv, bodyCh
}

func genConfigFrom(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	gc, ok := body["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig missing or wrong type: %T", body["generationConfig"])
	}
	return gc
}

// With a positive thinking_budget, the client must bound thinking and add
// headroom so the visible answer can't be starved: maxOutputTokens = max + budget.
func TestGeminiClient_Analyze_ThinkingBudget_Headroom(t *testing.T) {
	payload := `{"action":"WAIT","confidence":0.6,"size_pct":0,"stop_loss":0,"take_profit":0,"reasoning":"ok"}`
	srv, bodyCh := captureGeminiBody(t, geminiRespWithFinish(payload, "STOP", 800))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "gemini-2.5-flash", APIKey: "test-key", MaxTokens: 2048, ThinkingBudget: 1024}
	c := NewGeminiClient(cfg)
	c.baseURL = srv.URL

	if _, _, err := c.Analyze(context.Background(), AnalysisRequest{Ticker: "AAPL"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gc := genConfigFrom(t, <-bodyCh)
	if got := gc["maxOutputTokens"].(float64); got != 3072 {
		t.Errorf("maxOutputTokens = %v, want 3072 (2048 answer + 1024 thinking)", got)
	}
	tc, ok := gc["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("thinkingConfig missing: %T", gc["thinkingConfig"])
	}
	if got := tc["thinkingBudget"].(float64); got != 1024 {
		t.Errorf("thinkingBudget = %v, want 1024", got)
	}
}

// With thinking_budget unset (0), the client sends no thinkingConfig (preserving
// the model's native default) and maxOutputTokens equals the answer budget.
func TestGeminiClient_Analyze_ThinkingUnsetIsNative(t *testing.T) {
	payload := `{"action":"HOLD","confidence":0.5,"size_pct":0,"stop_loss":0,"take_profit":0,"reasoning":"ok"}`
	srv, bodyCh := captureGeminiBody(t, geminiRespWithFinish(payload, "STOP", 0))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "gemini-2.5-flash-lite", APIKey: "test-key", MaxTokens: 2048}
	c := NewGeminiClient(cfg)
	c.baseURL = srv.URL

	if _, _, err := c.Analyze(context.Background(), AnalysisRequest{Ticker: "AAPL"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gc := genConfigFrom(t, <-bodyCh)
	if got := gc["maxOutputTokens"].(float64); got != 2048 {
		t.Errorf("maxOutputTokens = %v, want 2048 (no headroom)", got)
	}
	if _, present := gc["thinkingConfig"]; present {
		t.Errorf("thinkingConfig should be absent when thinking_budget is unset, got %v", gc["thinkingConfig"])
	}
}

func TestGeminiClient_Analyze_ThinkingLevelAddsOutputHeadroom(t *testing.T) {
	payload := `{"action":"HOLD","confidence":0.5,"size_pct":0,"stop_loss":0,"take_profit":0,"reasoning":"ok"}`
	srv, bodyCh := captureGeminiBody(t, geminiRespWithFinish(payload, "STOP", 0))
	defer srv.Close()

	cfg := config.ModelConfig{
		Model: "gemini-3.1-pro-preview", APIKey: "test-key", MaxTokens: 2048,
		ThinkingLevel: "low",
	}
	c := NewGeminiClient(cfg)
	c.baseURL = srv.URL

	if _, _, err := c.Analyze(context.Background(), AnalysisRequest{Ticker: "AAPL"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gc := genConfigFrom(t, <-bodyCh)
	if got := gc["maxOutputTokens"].(float64); got != 6144 {
		t.Errorf("maxOutputTokens = %v, want 6144 (2048 answer + 4096 headroom)", got)
	}
	tc, ok := gc["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("thinkingConfig missing: %T", gc["thinkingConfig"])
	}
	if got := tc["thinkingLevel"]; got != "LOW" {
		t.Errorf("thinkingLevel = %v, want LOW", got)
	}
	if _, present := tc["thinkingBudget"]; present {
		t.Errorf("thinkingBudget should not be sent with thinkingLevel: %v", tc)
	}
}

func TestGeminiClient_Analyze_RetryExhaustionReturnsTypedFallbackStatus(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"details":[{"retryDelay":"0.001s"}]}}`))
	}))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "gemini-3.1-pro-preview", APIKey: "test-key", RequestsPerMinute: 600000}
	c := NewGeminiClient(cfg)
	c.baseURL = srv.URL
	_, _, err := c.Analyze(context.Background(), AnalysisRequest{Ticker: "AAPL"})
	if err == nil {
		t.Fatal("expected exhausted 429 error")
	}
	var apiErr *geminiAPIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("error = %T %v, want typed status 429", err, err)
	}
	if !shouldUseGeminiFallback(err) {
		t.Fatal("exhausted 429 should permit configured fallback")
	}
	if calls != 2 {
		t.Fatalf("server hit %d times, want 2 (initial + built-in retry)", calls)
	}
}

// A negative thinking_budget requests dynamic thinking (-1) without adding
// headroom — the operator owns sizing max_tokens.
func TestGeminiClient_Analyze_ThinkingDynamic(t *testing.T) {
	payload := `{"action":"HOLD","confidence":0.5,"size_pct":0,"stop_loss":0,"take_profit":0,"reasoning":"ok"}`
	srv, bodyCh := captureGeminiBody(t, geminiRespWithFinish(payload, "STOP", 0))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "gemini-2.5-flash", APIKey: "test-key", MaxTokens: 4096, ThinkingBudget: -1}
	c := NewGeminiClient(cfg)
	c.baseURL = srv.URL

	if _, _, err := c.Analyze(context.Background(), AnalysisRequest{Ticker: "AAPL"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gc := genConfigFrom(t, <-bodyCh)
	if got := gc["maxOutputTokens"].(float64); got != 4096 {
		t.Errorf("maxOutputTokens = %v, want 4096 (no headroom for dynamic)", got)
	}
	tc := gc["thinkingConfig"].(map[string]any)
	if got := tc["thinkingBudget"].(float64); got != -1 {
		t.Errorf("thinkingBudget = %v, want -1 (dynamic)", got)
	}
}

// A MAX_TOKENS finish must surface as an error even though the partial JSON is
// otherwise salvageable — otherwise a truncated reasoning gets persisted.
func TestGeminiClient_Analyze_MaxTokensTruncation(t *testing.T) {
	// Valid-but-truncated style payload the lenient extractor could salvage.
	partial := `{"action":"WAIT","confidence":0.7,"size_pct":0,"stop_loss":0,"take_profit":0,"reasoning":"NVDA exhibits short-`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(geminiRespWithFinish(partial, "MAX_TOKENS", 1981))
	}))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "gemini-2.5-flash", APIKey: "test-key", MaxTokens: 2048, ThinkingBudget: 1024}
	c := NewGeminiClient(cfg)
	c.baseURL = srv.URL

	_, meta, err := c.Analyze(context.Background(), AnalysisRequest{Ticker: "NVDA"})
	if err == nil {
		t.Fatal("expected error on MAX_TOKENS finish")
	}
	if !strings.Contains(err.Error(), "MAX_TOKENS") {
		t.Errorf("error = %q, want mention of MAX_TOKENS", err.Error())
	}
	// Meta is still returned for observability (raw response preserved).
	if meta.RawResponse == "" {
		t.Error("expected meta.RawResponse to be populated for diagnostics")
	}
}

func TestGeminiClient_Analyze_CustomPrompt(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	payload := `{"action":"HOLD","confidence":0.5,"size_pct":0,"stop_loss":0,"take_profit":0,"reasoning":"custom"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		bodyCh <- body
		json.NewEncoder(w).Encode(validGeminiResp(payload))
	}))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "gemini-pro", APIKey: "test-key"}
	c := NewGeminiClient(cfg)
	c.baseURL = srv.URL

	_, _, err := c.Analyze(context.Background(), AnalysisRequest{
		Ticker:             "AAPL",
		CustomSystemPrompt: "You are an analyst",
		CustomUserMessage:  "Return JSON only",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	capturedBody := <-bodyCh
	// verify system instruction came from CustomSystemPrompt
	si, _ := capturedBody["systemInstruction"].(map[string]any)
	parts, _ := si["parts"].([]any)
	if len(parts) == 0 {
		t.Fatal("expected systemInstruction parts")
	}
	p0, _ := parts[0].(map[string]any)
	if p0["text"] != "You are an analyst" {
		t.Errorf("systemInstruction text = %v, want 'You are an analyst'", p0["text"])
	}
}
