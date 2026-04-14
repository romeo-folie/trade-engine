package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
)

const anthropicAPIURL = "https://api.anthropic.com/v1/messages"
const anthropicVersion = "2023-06-01"

// jsonFenceRe matches ```json ... ``` blocks that models sometimes emit.
var jsonFenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")
var actionFieldRe = regexp.MustCompile(`"action"\s*:\s*"([^"]+)"`)
var confidenceFieldRe = regexp.MustCompile(`"confidence"\s*:\s*([-+]?[0-9]*\.?[0-9]+)`)
var sizePctFieldRe = regexp.MustCompile(`"size_pct"\s*:\s*([-+]?[0-9]*\.?[0-9]+)`)
var stopLossFieldRe = regexp.MustCompile(`"stop_loss"\s*:\s*([-+]?[0-9]*\.?[0-9]+)`)
var takeProfitFieldRe = regexp.MustCompile(`"take_profit"\s*:\s*([-+]?[0-9]*\.?[0-9]+)`)
var reasoningFieldRe = regexp.MustCompile(`"reasoning"\s*:\s*"([^"]*)`)

// AnthropicClient implements ModelProvider using the Anthropic Messages API.
type AnthropicClient struct {
	cfg        config.ModelConfig
	apiKey     string
	httpClient *http.Client
}

// NewAnthropicClient creates a Claude adapter from the given model config.
// The API key is resolved from the environment variable named by cfg.APIKeyEnv.
func NewAnthropicClient(cfg config.ModelConfig) *AnthropicClient {
	apiKey := cfg.APIKey
	if apiKey == "" && cfg.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.APIKeyEnv)
	}
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &AnthropicClient{
		cfg:        cfg,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: timeout},
	}
}

func (c *AnthropicClient) ProviderName() string { return "anthropic" }
func (c *AnthropicClient) ModelName() string    { return c.cfg.Model }
func (c *AnthropicClient) IsAvailable() bool    { return c.apiKey != "" }

// Analyze sends a structured market analysis request to Claude and returns
// a parsed AnalysisResponse.
func (c *AnthropicClient) Analyze(ctx context.Context, req AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
	var systemPrompt, userMessage string
	if req.CustomSystemPrompt != "" || req.CustomUserMessage != "" {
		// Custom analysis — use pre-built prompts directly.
		systemPrompt = req.CustomSystemPrompt
		userMessage = req.CustomUserMessage
	} else {
		systemPrompt, userMessage = prompts.BuildStockPrompt(req.Ticker, req.Strategy, req.Timeframe,
			toPromptCandles(req.RecentCandles),
			toPromptIndicators(req.Indicators),
			toPromptNews(req.News),
			toPromptPortfolio(req.Portfolio),
			req.KnowledgeContext,
			toPromptSimilarSetups(req.SimilarAnalyses),
			req.PerformanceContext,
		)
	}

	rawPrompt := systemPrompt + "\n\n---\n\n" + userMessage
	promptHash := hashPrompt(rawPrompt)

	indicatorJSON, _ := json.Marshal(req.Indicators)

	// Build the Anthropic Messages API request body.
	maxTokens := c.cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 1024
	}
	temperature := c.cfg.Temperature

	body := map[string]any{
		"model":       c.cfg.Model,
		"max_tokens":  maxTokens,
		"temperature": temperature,
		"system":      systemPrompt,
		"messages": []map[string]any{
			{"role": "user", "content": userMessage},
		},
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicAPIURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	httpReq.Header.Set("content-type", "application/json")

	start := time.Now()
	resp, err := c.httpClient.Do(httpReq)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("anthropic request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("anthropic API %d: %s", resp.StatusCode, bytes.TrimSpace(respBytes[:min(512, len(respBytes))]))
	}

	// Parse the Anthropic response envelope.
	var apiResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBytes, &apiResp); err != nil {
		return AnalysisResponse{}, ModelCallMeta{}, fmt.Errorf("parse anthropic response: %w", err)
	}

	var rawText string
	for _, block := range apiResp.Content {
		if block.Type == "text" {
			rawText = block.Text
			break
		}
	}

	meta := ModelCallMeta{
		Provider:      "anthropic",
		Model:         c.cfg.Model,
		PromptHash:    promptHash,
		RawPrompt:     rawPrompt,
		RawResponse:   rawText,
		InputTokens:   apiResp.Usage.InputTokens,
		OutputTokens:  apiResp.Usage.OutputTokens,
		LatencyMs:     latencyMs,
		IndicatorJSON: indicatorJSON,
	}

	analysis, err := extractAnalysisJSON(rawText)
	if err != nil {
		if req.Ticker != "__extraction__" {
			preview := rawText[:min(200, len(rawText))]
			logger.Warn("failed to extract JSON from model response",
				"ticker", req.Ticker,
				"err", err.Error(),
				"raw_preview", preview,
				"raw_len", len(rawText),
				"raw_truncated", len(rawText) > len(preview),
			)
		}
		return AnalysisResponse{}, meta, fmt.Errorf("extract JSON: %w", err)
	}

	return analysis, meta, nil
}

// extractAnalysisJSON parses an AnalysisResponse from model output.
// Handles both bare JSON objects and JSON wrapped in markdown code fences.
func extractAnalysisJSON(text string) (AnalysisResponse, error) {
	text = strings.TrimSpace(text)

	// Try markdown fence extraction first.
	if m := jsonFenceRe.FindStringSubmatch(text); len(m) == 2 {
		text = strings.TrimSpace(m[1])
	}
	// Tolerate unfinished fenced output like "```json\n{..." with missing closing ```.
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSuffix(text, "```")
		text = strings.TrimSpace(text)
	}

	// Fall back to finding the outermost { } block.
	if !strings.HasPrefix(text, "{") {
		start := strings.Index(text, "{")
		end := strings.LastIndex(text, "}")
		if start >= 0 && end > start {
			text = text[start : end+1]
		}
	}

	var result AnalysisResponse
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		// Recovery path for truncated model output (common with long reasoning):
		// best-effort extraction of required fields from partial JSON text.
		if strings.Contains(err.Error(), "unexpected end of JSON input") {
			if recovered, ok := recoverPartialAnalysis(text); ok {
				result = recovered
			} else {
				preview := text
				if utf8.RuneCountInString(preview) > 120 {
					preview = string([]rune(preview)[:120])
				}
				return AnalysisResponse{}, fmt.Errorf("unmarshal analysis: %w (text: %q)", err, preview)
			}
		} else {
			preview := text
			if utf8.RuneCountInString(preview) > 120 {
				preview = string([]rune(preview)[:120])
			}
			return AnalysisResponse{}, fmt.Errorf("unmarshal analysis: %w (text: %q)", err, preview)
		}
	}

	// Normalize action to canonical enum form (models sometimes emit lowercase).
	normAction := Action(strings.ToUpper(strings.TrimSpace(string(result.Action))))
	result.Action = normAction

	// Validate required fields.
	switch result.Action {
	case ActionBuy, ActionSell, ActionHold, ActionWait, ActionTrade, ActionSkip:
	default:
		return AnalysisResponse{}, fmt.Errorf("invalid action %q", result.Action)
	}
	if result.Confidence < 0 || result.Confidence > 1 {
		return AnalysisResponse{}, fmt.Errorf("confidence %f out of [0,1]", result.Confidence)
	}

	// Enforce invariant: non-directional actions must never carry position sizing.
	// Applied here (before any persistence path) so stored analyses are always
	// consistent, regardless of which code path calls extractAnalysisJSON.
	if result.Action == ActionHold || result.Action == ActionWait {
		result.SizePct = 0
		result.StopLoss = 0
		result.TakeProfit = 0
	}

	return result, nil
}

func recoverPartialAnalysis(text string) (AnalysisResponse, bool) {
	var out AnalysisResponse
	mAction := actionFieldRe.FindStringSubmatch(text)
	mConf := confidenceFieldRe.FindStringSubmatch(text)
	if len(mAction) < 2 || len(mConf) < 2 {
		return AnalysisResponse{}, false
	}
	conf, err := strconv.ParseFloat(mConf[1], 64)
	if err != nil {
		return AnalysisResponse{}, false
	}
	out.Action = Action(strings.TrimSpace(mAction[1]))
	out.Confidence = conf
	if m := sizePctFieldRe.FindStringSubmatch(text); len(m) == 2 {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			out.SizePct = v
		}
	}
	if m := stopLossFieldRe.FindStringSubmatch(text); len(m) == 2 {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			out.StopLoss = v
		}
	}
	if m := takeProfitFieldRe.FindStringSubmatch(text); len(m) == 2 {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			out.TakeProfit = v
		}
	}
	if m := reasoningFieldRe.FindStringSubmatch(text); len(m) == 2 {
		out.Reasoning = strings.TrimSpace(m[1])
	}
	return out, true
}

// ─── Conversion helpers ───────────────────────────────────────────────────────

func toPromptCandles(cs []CandleSnapshot) []prompts.Candle {
	out := make([]prompts.Candle, len(cs))
	for i, c := range cs {
		out[i] = prompts.Candle{
			Timestamp: c.Timestamp,
			Open:      c.Open,
			High:      c.High,
			Low:       c.Low,
			Close:     c.Close,
			Volume:    c.Volume,
			VWAP:      c.VWAP,
		}
	}
	return out
}

func toPromptIndicators(s IndicatorSnapshot) prompts.Indicators {
	return prompts.Indicators{
		EMA20: s.EMA20, EMA50: s.EMA50, EMA200: s.EMA200,
		SMA20: s.SMA20, SMA50: s.SMA50, SMA200: s.SMA200,
		RSI:           s.RSI,
		MACDLine:      s.MACDLine,
		MACDSignal:    s.MACDSignal,
		MACDHistogram: s.MACDHistogram,
		BBUpper:       s.BBUpper, BBMiddle: s.BBMiddle, BBLower: s.BBLower,
		ADX: s.ADX, ATR: s.ATR, OBV: s.OBV,
		StochK: s.StochK, StochD: s.StochD,
		VWAP: s.VWAP, VolSMA20: s.VolSMA20, VolRatio: s.VolRatio,
	}
}

func toPromptNews(ns []NewsSnippet) []prompts.NewsItem {
	out := make([]prompts.NewsItem, len(ns))
	for i, n := range ns {
		out[i] = prompts.NewsItem{
			Headline:    n.Headline,
			Source:      n.Source,
			PublishedAt: n.PublishedAt,
			Sentiment:   n.Sentiment,
		}
	}
	return out
}

func toPromptPortfolio(p PortfolioContext) prompts.Portfolio {
	pos := make([]prompts.Position, len(p.OpenPositions))
	for i, op := range p.OpenPositions {
		pos[i] = prompts.Position{
			Ticker:       op.Ticker,
			Quantity:     op.Quantity,
			AvgCostBasis: op.AvgCostBasis,
			CurrentValue: op.CurrentValue,
			UnrealizedPL: op.UnrealizedPL,
		}
	}
	return prompts.Portfolio{
		CashAvailable: p.CashAvailable,
		OpenPositions: pos,
	}
}

func toPromptSimilarSetups(setups []SimilarSetup) []prompts.SimilarPastSetup {
	out := make([]prompts.SimilarPastSetup, len(setups))
	for i, s := range setups {
		out[i] = prompts.SimilarPastSetup{
			Ticker:          s.Ticker,
			Action:          s.Action,
			Confidence:      s.Confidence,
			Reasoning:       s.Reasoning,
			CreatedAt:       s.CreatedAt,
			TradeOutcomePct: s.TradeOutcomePct,
		}
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
