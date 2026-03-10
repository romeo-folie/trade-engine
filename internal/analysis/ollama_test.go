package analysis

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func TestOllamaAnalyze_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Fatalf("path=%s want /api/generate", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"response":"{\"action\":\"BUY\",\"confidence\":0.82,\"size_pct\":2.0,\"stop_loss\":100.0,\"take_profit\":110.0,\"reasoning\":\"test\"}","prompt_eval_count":100,"eval_count":40}`)
	}))
	defer srv.Close()

	t.Setenv("OLLAMA_BASE_URL", srv.URL)
	c := NewOllamaClient(config.ModelConfig{Provider: "ollama", Model: "llama3.1", MaxTokens: 512})

	resp, meta, err := c.Analyze(context.Background(), AnalysisRequest{
		Ticker:             "AAPL",
		CustomSystemPrompt: "You are an analyst",
		CustomUserMessage:  "Return JSON only",
	})
	if err != nil {
		t.Fatalf("Analyze returned error: %v", err)
	}
	if resp.Action != ActionBuy {
		t.Fatalf("action=%s want BUY", resp.Action)
	}
	if meta.Provider != "ollama" {
		t.Fatalf("provider=%s want ollama", meta.Provider)
	}
	if meta.InputTokens != 100 || meta.OutputTokens != 40 {
		t.Fatalf("token counts mismatch: in=%d out=%d", meta.InputTokens, meta.OutputTokens)
	}
}

func TestOllamaAnalyze_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	t.Setenv("OLLAMA_BASE_URL", srv.URL)
	c := NewOllamaClient(config.ModelConfig{Provider: "ollama", Model: "llama3.1"})

	_, _, err := c.Analyze(context.Background(), AnalysisRequest{Ticker: "AAPL", CustomSystemPrompt: "s", CustomUserMessage: "u"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestOllamaAnalyze_BadJSONContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"response":"not json at all","prompt_eval_count":1,"eval_count":1}`)
	}))
	defer srv.Close()

	t.Setenv("OLLAMA_BASE_URL", srv.URL)
	c := NewOllamaClient(config.ModelConfig{Provider: "ollama", Model: "llama3.1"})
	_, _, err := c.Analyze(context.Background(), AnalysisRequest{Ticker: "AAPL", CustomSystemPrompt: "s", CustomUserMessage: "u"})
	if err == nil {
		t.Fatal("expected extract JSON error")
	}
}

func TestOllamaClient_IsAvailable(t *testing.T) {
	_ = os.Unsetenv("OLLAMA_BASE_URL")
	if NewOllamaClient(config.ModelConfig{Provider: "ollama", Model: ""}).IsAvailable() {
		t.Fatal("expected unavailable when model is empty")
	}
	if !NewOllamaClient(config.ModelConfig{Provider: "ollama", Model: "llama3.1"}).IsAvailable() {
		t.Fatal("expected available when model is set")
	}
}
