package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
)

const openAIAPIURL = "https://api.openai.com/v1/chat/completions"

// OpenAIClient implements ModelProvider using the OpenAI Chat Completions API.
// It is also reused for OpenAI-compatible providers (e.g. DeepSeek) by
// setting providerOverride and baseURL at construction time.
type OpenAIClient struct {
	cfg              config.ModelConfig
	apiKey           string
	baseURL          string // overridable for tests or compatible providers
	providerOverride string // non-empty overrides "openai" in ProviderName / meta
	httpClient       *http.Client
}

func NewOpenAIClient(cfg config.ModelConfig) *OpenAIClient {
	apiKey := cfg.APIKey
	if apiKey == "" && cfg.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.APIKeyEnv)
	}
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &OpenAIClient{cfg: cfg, apiKey: apiKey, baseURL: openAIAPIURL, httpClient: &http.Client{Timeout: timeout}}
}

func (c *OpenAIClient) ProviderName() string {
	if c.providerOverride != "" {
		return c.providerOverride
	}
	return "openai"
}
func (c *OpenAIClient) ModelName() string    { return c.cfg.Model }
func (c *OpenAIClient) IsAvailable() bool    { return c.apiKey != "" }

func (c *OpenAIClient) Analyze(ctx context.Context, req AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
	var systemPrompt, userMessage string
	if req.CustomSystemPrompt != "" || req.CustomUserMessage != "" {
		systemPrompt = req.CustomSystemPrompt
		userMessage = req.CustomUserMessage
	} else {
		systemPrompt, userMessage = prompts.BuildStockPrompt(
			req.Ticker, req.Strategy, req.Timeframe,
			toPromptCandles(req.RecentCandles), toPromptIndicators(req.Indicators),
			toPromptNews(req.News), toPromptPortfolio(req.Portfolio),
			req.KnowledgeContext, toPromptSimilarSetups(req.SimilarAnalyses),
			req.PerformanceContext,
		)
	}
	rawPrompt := systemPrompt + "\n\n---\n\n" + userMessage
	promptHash := hashPrompt(rawPrompt)
	indicatorJSON, _ := json.Marshal(req.Indicators)

	maxTokens := c.cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	temperature := c.cfg.Temperature
	if temperature == 0 {
		temperature = 0.7
	}

	body := map[string]any{
		"model": c.cfg.Model,
		"messages": []map[string]any{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userMessage},
		},
		"max_tokens":  maxTokens,
		"temperature": temperature,
	}
	provider := c.ProviderName()
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("%s marshal: %w", provider, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("%s build request: %w", provider, err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("content-type", "application/json")

	start := time.Now()
	resp, err := c.httpClient.Do(httpReq)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("%s request: %w", provider, err)
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("%s read response: %w", provider, err)
	}
	if resp.StatusCode >= 400 {
		snippet := respBytes
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("%s API %d: %s", provider, resp.StatusCode, bytes.TrimSpace(snippet))
	}

	var apiResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBytes, &apiResp); err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("%s parse response: %w", provider, err)
	}
	var rawText string
	if len(apiResp.Choices) > 0 {
		rawText = apiResp.Choices[0].Message.Content
	}
	meta := ModelCallMeta{
		Provider: provider, Model: c.cfg.Model, PromptHash: promptHash,
		RawPrompt: rawPrompt, RawResponse: rawText,
		InputTokens: apiResp.Usage.PromptTokens, OutputTokens: apiResp.Usage.CompletionTokens,
		LatencyMs: latencyMs, IndicatorJSON: indicatorJSON,
	}
	analysis, err := extractAnalysisJSON(rawText)
	if err != nil {
		if req.Ticker != "__extraction__" {
			preview := rawText[:min(200, len(rawText))]
			logger.Warn(provider+": failed to extract JSON",
				"ticker", req.Ticker,
				"err", err.Error(),
				"raw_preview", preview,
				"raw_len", len(rawText),
				"raw_truncated", len(rawText) > len(preview),
			)
		}
		return AnalysisResponse{}, meta, fmt.Errorf("%s extract JSON: %w", provider, err)
	}
	return analysis, meta, nil
}
