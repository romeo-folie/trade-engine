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
func NewLLMClassifier(cfg config.ClassifierConfig, modelCfg config.ModelConfig) *LLMClassifier {
	return &LLMClassifier{
		cfg:        cfg,
		modelCfg:   modelCfg,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		baseURL:    "https://api.openai.com/v1/chat/completions",
	}
}

// SetBaseURL overrides the API endpoint. Used in tests to inject a test server.
func (l *LLMClassifier) SetBaseURL(u string) { l.baseURL = u }

// Classify sends a minimal prompt and parses SKIP or ANALYZE.
// Always fails open on error (proceed to analysis, log warning).
func (l *LLMClassifier) Classify(ctx context.Context, s TickState) (Result, error) {
	noveltyStr := "unchanged"
	if s.NewsHash != s.LastNewsHash {
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
	body, _ := json.Marshal(reqBody)

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
	if strings.HasPrefix(upper, "SKIP") {
		decision = DecisionSkip
		reason = "LLM_SKIP"
		if len(content) > 5 {
			reason = strings.TrimSpace(content[5:])
		}
	} else {
		decision = DecisionAnalyze
		reason = "LLM_ANALYZE"
		if len(content) > 8 {
			reason = strings.TrimSpace(content[8:])
		}
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
