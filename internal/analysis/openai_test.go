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

func validOpenAIResp(content string) map[string]any {
	return map[string]any{
		"choices": []map[string]any{
			{"message": map[string]any{"content": content}},
		},
		"usage": map[string]any{
			"prompt_tokens":     15,
			"completion_tokens": 25,
		},
	}
}

func TestOpenAIClient_Metadata(t *testing.T) {
	cfg := config.ModelConfig{Model: "gpt-4o", APIKey: "test-key"}
	c := NewOpenAIClient(cfg)
	if c.ProviderName() != "openai" {
		t.Errorf("ProviderName = %q, want openai", c.ProviderName())
	}
	if c.ModelName() != "gpt-4o" {
		t.Errorf("ModelName = %q, want gpt-4o", c.ModelName())
	}
	if !c.IsAvailable() {
		t.Error("IsAvailable should be true with non-empty API key")
	}
}

func TestOpenAIClient_IsAvailable_NoKey(t *testing.T) {
	cfg := config.ModelConfig{Model: "gpt-4o"}
	c := NewOpenAIClient(cfg)
	if c.IsAvailable() {
		t.Error("IsAvailable should be false with empty API key")
	}
}

func TestOpenAIClient_Analyze_Success(t *testing.T) {
	payload := `{"action":"SELL","confidence":0.78,"size_pct":1.5,"stop_loss":155.0,"take_profit":135.0,"reasoning":"bearish reversal"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// verify endpoint path
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		// verify auth header
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("Authorization header = %q, want Bearer prefix", auth)
		}
		// verify content type
		if ct := r.Header.Get("content-type"); ct != "application/json" {
			t.Errorf("content-type = %q, want application/json", ct)
		}
		// verify model in request body
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if body["model"] != "gpt-4o" {
			t.Errorf("model = %v, want gpt-4o", body["model"])
		}
		json.NewEncoder(w).Encode(validOpenAIResp(payload))
	}))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "gpt-4o", APIKey: "test-key"}
	c := NewOpenAIClient(cfg)
	c.baseURL = srv.URL + "/v1/chat/completions"

	resp, meta, err := c.Analyze(context.Background(), AnalysisRequest{
		Ticker:    "TSLA",
		Strategy:  "trend_following",
		Timeframe: "1d",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Action != ActionSell {
		t.Errorf("action = %q, want SELL", resp.Action)
	}
	if resp.Confidence != 0.78 {
		t.Errorf("confidence = %.2f, want 0.78", resp.Confidence)
	}
	if meta.Provider != "openai" {
		t.Errorf("meta.Provider = %q, want openai", meta.Provider)
	}
	if meta.InputTokens != 15 || meta.OutputTokens != 25 {
		t.Errorf("token counts = %d/%d, want 15/25", meta.InputTokens, meta.OutputTokens)
	}
}

func TestOpenAIClient_Analyze_HTTP4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("insufficient quota"))
	}))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "gpt-4o", APIKey: "bad-key"}
	c := NewOpenAIClient(cfg)
	c.baseURL = srv.URL

	_, _, err := c.Analyze(context.Background(), AnalysisRequest{Ticker: "TSLA"})
	if err == nil {
		t.Fatal("expected error on 4xx")
	}
	if !strings.Contains(err.Error(), "openai API 403") {
		t.Errorf("error = %q, want openai API 403", err.Error())
	}
}

func TestOpenAIClient_Analyze_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(validOpenAIResp("this is not json"))
	}))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "gpt-4o", APIKey: "test-key"}
	c := NewOpenAIClient(cfg)
	c.baseURL = srv.URL

	_, _, err := c.Analyze(context.Background(), AnalysisRequest{Ticker: "__extraction__"})
	if err == nil {
		t.Fatal("expected JSON extraction error")
	}
}

func TestOpenAIClient_Analyze_CustomPrompt(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	payload := `{"action":"HOLD","confidence":0.5,"size_pct":0,"stop_loss":0,"take_profit":0,"reasoning":"custom"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		bodyCh <- body
		json.NewEncoder(w).Encode(validOpenAIResp(payload))
	}))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "gpt-4o", APIKey: "test-key"}
	c := NewOpenAIClient(cfg)
	c.baseURL = srv.URL

	_, _, err := c.Analyze(context.Background(), AnalysisRequest{
		Ticker:             "TSLA",
		CustomSystemPrompt: "You are a trader",
		CustomUserMessage:  "Return JSON",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	capturedBody := <-bodyCh
	// verify system message was injected correctly
	messages, _ := capturedBody["messages"].([]any)
	if len(messages) < 2 {
		t.Fatal("expected at least 2 messages")
	}
	sysMsg, _ := messages[0].(map[string]any)
	if sysMsg["role"] != "system" {
		t.Errorf("first message role = %v, want system", sysMsg["role"])
	}
	if sysMsg["content"] != "You are a trader" {
		t.Errorf("system content = %v, want 'You are a trader'", sysMsg["content"])
	}
}

func TestOpenAIClient_Analyze_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{},
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 0},
		})
	}))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "gpt-4o", APIKey: "test-key"}
	c := NewOpenAIClient(cfg)
	c.baseURL = srv.URL

	_, _, err := c.Analyze(context.Background(), AnalysisRequest{Ticker: "__extraction__"})
	if err == nil {
		t.Fatal("expected error on empty choices")
	}
}
