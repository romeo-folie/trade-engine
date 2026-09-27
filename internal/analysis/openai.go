package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
)

const openAIAPIURL = "https://api.openai.com/v1/chat/completions"

// isOpenAIReasoningModel reports whether an OpenAI model uses the reasoning-model
// Chat Completions schema (max_completion_tokens, no custom temperature). This
// covers the gpt-5 family (gpt-5, gpt-5-mini, gpt-5-nano, …) and the o-series
// (o1, o3, o4, …). Matching is by name prefix so future point releases and
// dated snapshots (e.g. "gpt-5-nano-2025-08-07") are caught automatically.
//
// The gpt-5 *chat* aliases (e.g. "gpt-5-chat-latest") are deliberately excluded:
// they are conventional chat models that still accept temperature and the classic
// max_tokens field, so routing them through the reasoning branch would silently
// drop a configured temperature.
func isOpenAIReasoningModel(model string) bool {
	if strings.Contains(model, "-chat") {
		return false
	}
	for _, prefix := range []string{"gpt-5", "o1", "o3", "o4"} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

// defaultReasoningEffort returns the reasoning_effort to send for a reasoning
// model when none is configured. "low" stops a large prompt's reasoning from
// starving the visible answer and is accepted by every reasoning family —
// except gpt-5-pro, which only supports "high". For gpt-5-pro we return ""
// (omit the field) and let the model use its native default rather than turn a
// previously valid request into a 400. An explicit config value always wins.
func defaultReasoningEffort(model string) string {
	if strings.Contains(model, "gpt-5-pro") {
		return ""
	}
	return "low"
}

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
func (c *OpenAIClient) ModelName() string { return c.cfg.Model }
func (c *OpenAIClient) IsAvailable() bool { return c.apiKey != "" }

func (c *OpenAIClient) Analyze(ctx context.Context, req AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
	var systemPrompt, userMessage string
	if req.CustomSystemPrompt != "" || req.CustomUserMessage != "" {
		systemPrompt = req.CustomSystemPrompt
		userMessage = req.CustomUserMessage
	} else {
		systemPrompt, userMessage = prompts.BuildStockPromptWithAnalysisContext(
			req.Ticker, req.LensSet(), req.Timeframe,
			toPromptCandles(req.RecentCandles), toPromptIndicators(req.Indicators),
			toPromptNews(req.News), toPromptPortfolio(req.Portfolio, req.Ticker),
			req.KnowledgeContext, toPromptSimilarSetups(req.SimilarAnalyses),
			req.PerformanceContext, req.MomentumPlay, req.DiscoveryContext, req.PullbackContext,
			req.TimeframeRole, req.PrimaryThesisContext, req.RiskPolicy,
		)
		systemPrompt, userMessage = appendTimeframeContexts(systemPrompt, userMessage, req.TimeframeContexts)
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
	}
	// OpenAI's gpt-5 and o-series reasoning models renamed the token-limit
	// field to "max_completion_tokens" and reject any non-default temperature
	// (only the implicit 1 is allowed). Only genuine OpenAI uses this schema;
	// OpenAI-compatible providers like DeepSeek (providerOverride != "") still
	// expect the classic "max_tokens"/"temperature" pair.
	if c.providerOverride == "" && isOpenAIReasoningModel(c.cfg.Model) {
		body["max_completion_tokens"] = maxTokens
		// Reasoning tokens count inside max_completion_tokens; on a large prompt
		// unbounded reasoning eats the whole budget and returns an empty answer
		// (finish_reason=length). Cap it — default "low" keeps room for the JSON.
		effort := c.cfg.ReasoningEffort
		if effort == "" {
			effort = defaultReasoningEffort(c.cfg.Model)
		}
		if effort != "" {
			body["reasoning_effort"] = effort
		}
	} else {
		body["max_tokens"] = maxTokens
		body["temperature"] = temperature
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
