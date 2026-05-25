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

const geminiAPIBaseURL = "https://generativelanguage.googleapis.com/v1beta/models"

// GeminiClient implements ModelProvider using the Google Gemini REST API.
type GeminiClient struct {
	cfg        config.ModelConfig
	apiKey     string
	baseURL    string // overridable for tests; defaults to geminiAPIBaseURL
	httpClient *http.Client
}

func NewGeminiClient(cfg config.ModelConfig) *GeminiClient {
	apiKey := cfg.APIKey
	if apiKey == "" && cfg.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.APIKeyEnv)
	}
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &GeminiClient{cfg: cfg, apiKey: apiKey, baseURL: geminiAPIBaseURL, httpClient: &http.Client{Timeout: timeout}}
}

func (c *GeminiClient) ProviderName() string { return "google" }
func (c *GeminiClient) ModelName() string    { return c.cfg.Model }
func (c *GeminiClient) IsAvailable() bool    { return c.apiKey != "" }

func (c *GeminiClient) Analyze(ctx context.Context, req AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
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
			req.PerformanceContext, req.MomentumPlay,
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

	// Gemini 2.5 counts thinking tokens inside maxOutputTokens. Left unbounded,
	// a verbose thinking phase can consume the whole budget and leave too few
	// tokens for the JSON answer, truncating it mid-string. When a thinking_budget
	// is configured we send an explicit thinkingConfig and add headroom so the
	// answer (maxTokens) can never be crowded out by thinking (thinkingBudget).
	genConfig := map[string]any{
		"temperature":      temperature,
		"responseMimeType": "application/json",
	}
	thinkingBudget := c.cfg.ThinkingBudget
	maxOut := maxTokens
	switch {
	case thinkingBudget > 0:
		// Bounded thinking + headroom: the answer (maxTokens) can never be
		// crowded out because thinking has its own dedicated budget on top.
		maxOut = maxTokens + thinkingBudget
		genConfig["thinkingConfig"] = map[string]any{"thinkingBudget": thinkingBudget}
	case thinkingBudget < 0:
		// Dynamic thinking (-1): Gemini sizes its own budget; the operator owns
		// keeping max_tokens generous enough to leave room for the answer.
		genConfig["thinkingConfig"] = map[string]any{"thinkingBudget": -1}
	}
	// thinkingBudget == 0 (unset): send no thinkingConfig and leave maxOutputTokens
	// at the answer budget. This preserves each model's native default — some
	// Gemini models (2.5-pro, gemini-3) cannot have thinking disabled, so we must
	// not force a budget of 0 on them. The MAX_TOKENS guard below still catches
	// any truncation that results.
	genConfig["maxOutputTokens"] = maxOut

	body := map[string]any{
		"systemInstruction": map[string]any{"parts": []map[string]any{{"text": systemPrompt}}},
		"contents":          []map[string]any{{"role": "user", "parts": []map[string]any{{"text": userMessage}}}},
		"generationConfig":  genConfig,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("gemini marshal: %w", err)
	}

	model := c.cfg.Model
	if model == "" {
		model = "gemini-pro"
	}
	// Key in header — never in URL (prevents leaking via access logs, proxies, referers).
	url := fmt.Sprintf("%s/%s:generateContent", c.baseURL, model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("gemini build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", c.apiKey)

	start := time.Now()
	resp, err := c.httpClient.Do(httpReq)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("gemini request: %w", err)
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("gemini read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		snippet := respBytes
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("gemini API %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}

	var apiResp struct {
		Candidates []struct {
			FinishReason string `json:"finishReason"`
			Content      struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(respBytes, &apiResp); err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("gemini parse response: %w", err)
	}
	var rawText, finishReason string
	if len(apiResp.Candidates) > 0 {
		finishReason = apiResp.Candidates[0].FinishReason
		if len(apiResp.Candidates[0].Content.Parts) > 0 {
			rawText = apiResp.Candidates[0].Content.Parts[0].Text
		}
	}
	meta := ModelCallMeta{
		Provider: "google", Model: c.cfg.Model, PromptHash: promptHash,
		RawPrompt: rawPrompt, RawResponse: rawText,
		InputTokens:   apiResp.UsageMetadata.PromptTokenCount,
		OutputTokens:  apiResp.UsageMetadata.CandidatesTokenCount,
		LatencyMs:     latencyMs,
		IndicatorJSON: indicatorJSON,
	}

	// A MAX_TOKENS finish means the answer was cut off — thinking and/or output
	// hit maxOutputTokens. extractAnalysisJSON would otherwise leniently salvage
	// a partial JSON (e.g. a reasoning string truncated mid-word) and persist it.
	// Fail loudly instead so the analyst is dropped, not stored half-formed.
	if finishReason == "MAX_TOKENS" {
		if req.Ticker != "__extraction__" {
			logger.Warn("gemini: response truncated by token cap",
				"ticker", req.Ticker,
				"model", c.cfg.Model,
				"thinking_tokens", apiResp.UsageMetadata.ThoughtsTokenCount,
				"answer_tokens", apiResp.UsageMetadata.CandidatesTokenCount,
				"max_output_tokens", maxOut,
				"raw_len", len(rawText),
			)
		}
		return AnalysisResponse{}, meta, fmt.Errorf(
			"gemini truncated (finishReason=MAX_TOKENS): thinking %d + answer %d hit maxOutputTokens %d; raise max_tokens or lower thinking_budget",
			apiResp.UsageMetadata.ThoughtsTokenCount, apiResp.UsageMetadata.CandidatesTokenCount, maxOut)
	}
	analysis, err := extractAnalysisJSON(rawText)
	if err != nil {
		if req.Ticker != "__extraction__" {
			preview := rawText[:min(200, len(rawText))]
			logger.Warn("gemini: failed to extract JSON",
				"ticker", req.Ticker,
				"err", err.Error(),
				"raw_preview", preview,
				"raw_len", len(rawText),
				"raw_truncated", len(rawText) > len(preview),
			)
		}
		return AnalysisResponse{}, meta, fmt.Errorf("gemini extract JSON: %w", err)
	}
	return analysis, meta, nil
}
