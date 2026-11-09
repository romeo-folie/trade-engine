package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// cannedTransport answers every request with a fixed JSON body, so provider
// adapters can run end to end without network access.
type cannedTransport struct{ body []byte }

func (c cannedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(c.body)),
	}, nil
}

// TestProvidersRenderNewsAgeFromDecisionCutoff guards the adapter boundary:
// every provider must thread AnalysisRequest.DecisionCutoff into the prompt it
// actually sends, not only the helper (issue #589).
func TestProvidersRenderNewsAgeFromDecisionCutoff(t *testing.T) {
	const answer = `{"action":"HOLD","confidence":0.5,"size_pct":0,"stop_loss":0,"take_profit":0,"reasoning":"ok","decisive_reason":"none"}`
	mustJSON := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cutoff := time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC)
	req := AnalysisRequest{
		Ticker: "TEST", Strategy: "momentum", Timeframe: "1d",
		News: []NewsSnippet{{Headline: "Guidance raised", Source: "wire",
			PublishedAt: cutoff.Add(-5 * time.Hour), Sentiment: "bullish"}},
		DecisionCutoff: cutoff,
	}

	anthropic := NewAnthropicClient(config.ModelConfig{Provider: "anthropic", Model: "m", APIKey: "k"})
	anthropic.httpClient = &http.Client{Transport: cannedTransport{mustJSON(map[string]any{
		"content": []map[string]any{{"type": "text", "text": answer}},
	})}}
	openai := NewOpenAIClient(config.ModelConfig{Provider: "openai", Model: "gpt-4o", APIKey: "k"})
	openai.httpClient = &http.Client{Transport: cannedTransport{mustJSON(validOpenAIResp(answer))}}
	// Unique model and high RPM: Gemini limiters are shared process-wide per
	// model, so registering a real model name here would throttle other tests.
	gemini := NewGeminiClient(config.ModelConfig{Provider: "google", Model: "gemini-cutoff-test", APIKey: "k", RequestsPerMinute: 600000})
	gemini.httpClient = &http.Client{Transport: cannedTransport{mustJSON(validGeminiResp(answer))}}
	ollama := NewOllamaClient(config.ModelConfig{Provider: "ollama", Model: "llama3.1"})
	ollama.httpClient = &http.Client{Transport: cannedTransport{mustJSON(map[string]any{"response": answer})}}

	for _, p := range []ModelProvider{anthropic, openai, gemini, ollama} {
		_, meta, err := p.Analyze(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", p.ProviderName(), err)
		}
		if !strings.Contains(meta.RawPrompt, "| 5h ago] Guidance raised") {
			t.Fatalf("%s did not render news age from DecisionCutoff; prompt:\n%s", p.ProviderName(), meta.RawPrompt)
		}
	}
}
