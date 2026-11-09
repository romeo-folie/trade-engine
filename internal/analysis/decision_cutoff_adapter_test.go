package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/strategy/antichase"
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

// TestRenderedPromptIsZoneIndependent: snapshots store every time in UTC, so
// the model-visible prompt must not depend on the zone a timestamp carries —
// otherwise identical input hashes could stand for different prompts.
func TestRenderedPromptIsZoneIndependent(t *testing.T) {
	const answer = `{"action":"HOLD","confidence":0.5,"size_pct":0,"stop_loss":0,"take_profit":0,"reasoning":"ok","decisive_reason":"none"}`
	body, _ := json.Marshal(map[string]any{"content": []map[string]any{{"type": "text", "text": answer}}})
	cutoff := time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC)
	build := func(loc *time.Location) AnalysisRequest {
		bar := func(h int) CandleSnapshot {
			return CandleSnapshot{Timestamp: cutoff.Add(-time.Duration(h) * time.Hour).In(loc), Open: 1, High: 2, Low: 1, Close: 1.5, Volume: 10}
		}
		catalyst := cutoff.Add(-3 * time.Hour).In(loc)
		return AnalysisRequest{
			Ticker: "TEST", Strategy: "momentum", Timeframe: "1d", DecisionCutoff: cutoff.In(loc),
			RecentCandles: []CandleSnapshot{bar(0), bar(24)},
			Indicators: IndicatorSnapshot{TimingSnapshot: &antichase.Snapshot{SchemaVersion: 1, Ticker: "TEST",
				ObservedAt: cutoff.In(loc), CatalystAt: &catalyst, EvidenceStatus: "available"}},
			TimeframeContexts: []TimeframeContext{{Timeframe: "1h", Role: "entry_timing", RecentCandles: []CandleSnapshot{bar(1), bar(2)}}},
			News:              []NewsSnippet{{Headline: "h", Source: "s", PublishedAt: cutoff.Add(-5 * time.Hour).In(loc), Sentiment: "bullish"}},
		}
	}
	render := func(req AnalysisRequest) string {
		c := NewAnthropicClient(config.ModelConfig{Provider: "anthropic", Model: "m", APIKey: "k"})
		c.httpClient = &http.Client{Transport: cannedTransport{body}}
		_, meta, err := c.Analyze(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		return meta.RawPrompt
	}
	utc := render(build(time.UTC))
	if est := render(build(time.FixedZone("EST", -5*3600))); est != utc {
		t.Fatalf("prompt depends on timestamp zone:\n--- UTC\n%s\n--- EST\n%s", utc, est)
	}
}

type failingTransport struct {
	status int // 0 = transport error
}

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if f.status == 0 {
		return nil, errors.New("connection reset")
	}
	return &http.Response{StatusCode: f.status, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(`{"error":"boom"}`))}, nil
}

// TestProvidersReturnAttemptMetadataOnFailure: a failed call must still report
// what was attempted (identity, prompt, settings) so decision snapshots keep
// the evidence, without fabricating a response or token spend.
func TestProvidersReturnAttemptMetadataOnFailure(t *testing.T) {
	req := AnalysisRequest{Ticker: "TEST", Strategy: "momentum", Timeframe: "1d", DecisionCutoff: time.Now()}
	for _, tr := range []failingTransport{{status: 500}, {status: 0}} {
		anthropic := NewAnthropicClient(config.ModelConfig{Provider: "anthropic", Model: "am", APIKey: "k"})
		anthropic.httpClient = &http.Client{Transport: tr}
		openai := NewOpenAIClient(config.ModelConfig{Provider: "openai", Model: "gpt-4o", APIKey: "k"})
		openai.httpClient = &http.Client{Transport: tr}
		gemini := NewGeminiClient(config.ModelConfig{Provider: "google", Model: "gemini-failure-test", APIKey: "k", RequestsPerMinute: 600000})
		gemini.httpClient = &http.Client{Transport: tr}
		ollama := NewOllamaClient(config.ModelConfig{Provider: "ollama", Model: "llama3.1"})
		ollama.httpClient = &http.Client{Transport: tr}
		for _, p := range []ModelProvider{anthropic, openai, gemini, ollama} {
			_, meta, err := p.Analyze(context.Background(), req)
			if err == nil {
				t.Fatalf("%s (status %d): expected error", p.ProviderName(), tr.status)
			}
			if meta.Provider != p.ProviderName() || meta.Model != p.ModelName() || meta.RawPrompt == "" ||
				meta.PromptHash == "" || len(meta.RequestParams) == 0 {
				t.Fatalf("%s (status %d): attempt metadata missing: %+v", p.ProviderName(), tr.status, meta)
			}
			if meta.RawResponse != "" || meta.InputTokens != 0 || meta.OutputTokens != 0 {
				t.Fatalf("%s (status %d): failure must not fabricate a response or spend: %+v", p.ProviderName(), tr.status, meta)
			}
			for k := range meta.RequestParams {
				if k == "messages" || k == "system" || k == "prompt" || k == "contents" || k == "systemInstruction" {
					t.Fatalf("%s: request params include prompt body key %q", p.ProviderName(), k)
				}
			}
		}
	}
}
