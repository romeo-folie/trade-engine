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

const defaultOllamaBaseURL = "http://localhost:11434"

// OllamaClient implements ModelProvider against a local/remote Ollama server.
type OllamaClient struct {
	cfg        config.ModelConfig
	baseURL    string
	httpClient *http.Client
}

func NewOllamaClient(cfg config.ModelConfig) *OllamaClient {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	baseURL := strings.TrimSpace(os.Getenv("OLLAMA_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultOllamaBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &OllamaClient{cfg: cfg, baseURL: baseURL, httpClient: &http.Client{Timeout: timeout}}
}

func (c *OllamaClient) ProviderName() string { return "ollama" }
func (c *OllamaClient) ModelName() string    { return c.cfg.Model }
func (c *OllamaClient) IsAvailable() bool    { return c.cfg.Model != "" }

func (c *OllamaClient) Analyze(ctx context.Context, req AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
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
			req.TimeframeRole, req.PrimaryThesisContext,
		)
	}
	combinedPrompt := systemPrompt + "\n\n" + userMessage
	promptHash := hashPrompt(combinedPrompt)
	indicatorJSON, _ := json.Marshal(req.Indicators)

	temperature := c.cfg.Temperature
	if temperature == 0 {
		temperature = 0.7
	}
	maxTokens := c.cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 1024
	}

	body := map[string]any{
		"model":  c.cfg.Model,
		"system": systemPrompt,
		"prompt": userMessage,
		"stream": false,
		"options": map[string]any{
			"temperature": temperature,
			"num_predict": maxTokens,
		},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("ollama marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/generate", bytes.NewReader(bodyBytes))
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("ollama build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := c.httpClient.Do(httpReq)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("ollama read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		snippet := respBytes
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("ollama API %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}

	var apiResp struct {
		Response        string `json:"response"`
		PromptEvalCount int    `json:"prompt_eval_count"`
		EvalCount       int    `json:"eval_count"`
	}
	if err := json.Unmarshal(respBytes, &apiResp); err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("ollama parse response: %w", err)
	}
	if strings.TrimSpace(apiResp.Response) == "" {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("ollama response empty")
	}

	meta := ModelCallMeta{
		Provider:      "ollama",
		Model:         c.cfg.Model,
		PromptHash:    promptHash,
		RawPrompt:     combinedPrompt,
		RawResponse:   apiResp.Response,
		InputTokens:   apiResp.PromptEvalCount,
		OutputTokens:  apiResp.EvalCount,
		LatencyMs:     latencyMs,
		IndicatorJSON: indicatorJSON,
	}

	analysisResp, err := extractAnalysisJSON(apiResp.Response)
	if err != nil {
		logger.Warn("ollama: failed to extract JSON", "ticker", req.Ticker, "err", err.Error())
		return AnalysisResponse{}, meta, fmt.Errorf("ollama extract JSON: %w", err)
	}
	return analysisResp, meta, nil
}
