package analysis

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestGeminiClient_Analyze_CustomPrompt(t *testing.T) {
	var capturedBody map[string]any
	payload := `{"action":"HOLD","confidence":0.5,"size_pct":0,"stop_loss":0,"take_profit":0,"reasoning":"custom"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody)
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
