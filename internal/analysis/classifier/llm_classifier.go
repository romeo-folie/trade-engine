package classifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
)

// llmRequest / llmResponse mirror the OpenAI chat completions API shape used
// for the classifier role.  We use the OpenAI format for all providers in this
// role (gpt-4o-mini default; swap model via registry).
type llmRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	Messages  []llmMessage `json:"messages"`
}

type llmMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type llmResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

const classifierPromptTemplate = `You are a trading signal filter.
Ticker: %s | Timeframe: %s
RSI delta: %.2f | EMA gap delta: %.2f%% | Price move: %.2f%%
News novelty: %s
Last decision: %s (%.0f min ago)

Reply SKIP or ANALYZE with a reason in 10 words max.`

// LLMClassifier is the Layer-2 fallback classifier. It sends a minimal prompt
// to the configured classifier model and parses SKIP/ANALYZE from the response.
// On any error it fails open (returns DecisionAnalyze).
type LLMClassifier struct {
	cfg        config.ClassifierConfig
	modelCfg   config.ModelConfig
	httpClient *http.Client
	baseURL    string // overridable for tests
}

// NewLLMClassifier creates a Layer-2 classifier. modelCfg is the resolved
// registry entry for the "classifier" role.
//
// Note: the LLM classifier uses the OpenAI Chat Completions API format directly.
// Only "openai" provider registry entries are supported.
//
// Unsupported providers (anything other than "openai") or missing API keys return
// a stub LLMClassifier whose Classify always fails open without making HTTP calls.
// Use IsSupported() to check before wiring into the gate.
func NewLLMClassifier(cfg config.ClassifierConfig, modelCfg config.ModelConfig) *LLMClassifier {
	timeout := 15 * time.Second
	if modelCfg.TimeoutSec > 0 {
		timeout = time.Duration(modelCfg.TimeoutSec) * time.Second
	}
	return &LLMClassifier{
		cfg:        cfg,
		modelCfg:   modelCfg,
		httpClient: &http.Client{Timeout: timeout},
		baseURL:    "https://api.openai.com/v1/chat/completions",
	}
}

// IsSupported returns false when the configured provider or API key is
// incompatible with the LLM classifier. The gate should skip wiring Layer 2
// when this returns false.
func (l *LLMClassifier) IsSupported() bool {
	return l.modelCfg.Provider == "openai" && l.modelCfg.APIKey != ""
}

// SetBaseURL overrides the API endpoint. Used in tests to inject a test server.
func (l *LLMClassifier) SetBaseURL(u string) { l.baseURL = u }

// Classify sends a minimal prompt and parses SKIP or ANALYZE.
// Always fails open on error (proceed to analysis, log warning).
// Immediately fails open when the provider is not supported or the API key is absent.
func (l *LLMClassifier) Classify(ctx context.Context, s TickState) (Result, error) {
	if !l.IsSupported() {
		return failOpen("unsupported provider or missing API key", nil), nil
	}
	// Mirror Layer-1 novelty logic: only treat as "changed" when the current
	// hash is non-empty; an empty hash means no news data, not novel news.
	noveltyStr := "unchanged"
	if s.NewsHash != "" && s.NewsHash != s.LastNewsHash {
		noveltyStr = "changed"
	}
	prompt := fmt.Sprintf(classifierPromptTemplate,
		s.Ticker, s.Timeframe,
		s.RSIDelta, s.EMAGapDelta, s.PriceMovesPct,
		noveltyStr, s.LastDecision, s.ElapsedMin,
	)

	reqBody := llmRequest{
		Model:     l.modelCfg.Model,
		MaxTokens: 64,
		Messages:  []llmMessage{{Role: "user", Content: prompt}},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return failOpen("marshal failed", err), nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL, bytes.NewReader(body))
	if err != nil {
		return failOpen("request build failed", err), nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.modelCfg.APIKey)

	resp, err := l.httpClient.Do(req)
	if err != nil {
		return failOpen("http call failed", err), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return failOpen(fmt.Sprintf("HTTP %d", resp.StatusCode), fmt.Errorf("%s", raw)), nil
	}

	var llmResp llmResponse
	if err := json.NewDecoder(resp.Body).Decode(&llmResp); err != nil {
		return failOpen("decode failed", err), nil
	}
	if len(llmResp.Choices) == 0 {
		return failOpen("empty choices", nil), nil
	}

	content := strings.TrimSpace(llmResp.Choices[0].Message.Content)
	upper := strings.ToUpper(content)

	var decision Decision
	var reason string
	switch {
	case strings.HasPrefix(upper, "SKIP"):
		decision = DecisionSkip
		reason = "LLM_SKIP"
		if len(content) > 5 {
			reason = strings.TrimSpace(content[5:])
		}
	case strings.HasPrefix(upper, "ANALYZE"):
		decision = DecisionAnalyze
		reason = "LLM_ANALYZE"
		if len(content) > 8 {
			reason = strings.TrimSpace(content[8:])
		}
	default:
		// Unexpected prefix — fail open so analysis always proceeds on
		// ambiguous or malformed model output.
		logger.Warn("classifier LLM: unexpected response format — failing open",
			"content", content)
		return failOpen("unexpected response format", nil), nil
	}

	return Result{
		Decision:   decision,
		ReasonCode: reason,
		Deltas:     map[string]float64{"rsi_delta": s.RSIDelta, "ema_gap_delta_pct": s.EMAGapDelta, "price_move_pct": s.PriceMovesPct},
	}, nil
}

func failOpen(msg string, err error) Result {
	if err != nil {
		logger.Warn("classifier LLM call failed — failing open", "msg", msg, "err", err.Error())
	} else {
		logger.Warn("classifier LLM call failed — failing open", "msg", msg)
	}
	return Result{Decision: DecisionAnalyze, ReasonCode: "LLM_ERROR_FAIL_OPEN"}
}
