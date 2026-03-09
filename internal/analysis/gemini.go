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
	return &GeminiClient{cfg: cfg, apiKey: apiKey, httpClient: &http.Client{Timeout: timeout}}
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
		"systemInstruction": map[string]any{"parts": []map[string]any{{"text": systemPrompt}}},
		"contents":          []map[string]any{{"role": "user", "parts": []map[string]any{{"text": userMessage}}}},
		"generationConfig":  map[string]any{"maxOutputTokens": maxTokens, "temperature": temperature},
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
	url := fmt.Sprintf("%s/%s:generateContent", geminiAPIBaseURL, model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("gemini build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
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
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(respBytes, &apiResp); err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("gemini parse response: %w", err)
	}
	var rawText string
	if len(apiResp.Candidates) > 0 && len(apiResp.Candidates[0].Content.Parts) > 0 {
		rawText = apiResp.Candidates[0].Content.Parts[0].Text
	}
	meta := ModelCallMeta{
		Provider: "google", Model: c.cfg.Model, PromptHash: promptHash,
		RawPrompt: rawPrompt, RawResponse: rawText,
		InputTokens:   apiResp.UsageMetadata.PromptTokenCount,
		OutputTokens:  apiResp.UsageMetadata.CandidatesTokenCount,
		LatencyMs:     latencyMs,
		IndicatorJSON: indicatorJSON,
	}
	analysis, err := extractAnalysisJSON(rawText)
	if err != nil {
		logger.Warn("gemini: failed to extract JSON", "ticker", req.Ticker, "err", err.Error(), "raw", rawText[:min(200, len(rawText))])
		return AnalysisResponse{}, meta, fmt.Errorf("gemini extract JSON: %w", err)
	}
	return analysis, meta, nil
}
