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

func TestDeepSeekClient_Metadata(t *testing.T) {
	cfg := config.ModelConfig{Model: "deepseek-chat", APIKey: "test-key"}
	c := NewDeepSeekClient(cfg)
	if c.ProviderName() != "deepseek" {
		t.Errorf("ProviderName = %q, want deepseek", c.ProviderName())
	}
	if c.ModelName() != "deepseek-chat" {
		t.Errorf("ModelName = %q, want deepseek-chat", c.ModelName())
	}
	if !c.IsAvailable() {
		t.Error("IsAvailable should be true with non-empty API key")
	}
}

func TestDeepSeekClient_UsesDeepSeekURL(t *testing.T) {
	hitPath := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		payload := `{"action":"HOLD","confidence":0.5,"size_pct":0,"stop_loss":0,"take_profit":0,"reasoning":"ok"}`
		json.NewEncoder(w).Encode(validOpenAIResp(payload))
	}))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "deepseek-chat", APIKey: "test-key"}
	c := NewDeepSeekClient(cfg)
	c.baseURL = srv.URL + "/v1/chat/completions"

	_, meta, err := c.Analyze(context.Background(), AnalysisRequest{Ticker: "TSLA", Strategy: "trend_following", Timeframe: "1d"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hitPath != "/v1/chat/completions" {
		t.Errorf("request path = %q, want /v1/chat/completions", hitPath)
	}
	if meta.Provider != "deepseek" {
		t.Errorf("meta.Provider = %q, want deepseek", meta.Provider)
	}
}

func TestDeepSeekClient_Analyze_HTTP4xx_ErrorPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("rate limited"))
	}))
	defer srv.Close()

	cfg := config.ModelConfig{Model: "deepseek-chat", APIKey: "test-key"}
	c := NewDeepSeekClient(cfg)
	c.baseURL = srv.URL

	_, _, err := c.Analyze(context.Background(), AnalysisRequest{Ticker: "TSLA"})
	if err == nil {
		t.Fatal("expected error on 4xx")
	}
	if !strings.Contains(err.Error(), "deepseek API 429") {
		t.Errorf("error = %q, want deepseek API 429 prefix", err.Error())
	}
	if strings.Contains(err.Error(), "openai") {
		t.Errorf("error %q must not contain 'openai' for deepseek provider", err.Error())
	}
}
