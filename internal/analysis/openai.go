package analysis

import (
	"context"
	"errors"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// OpenAIClient is a stub — not implemented in Phase 3.
type OpenAIClient struct {
	cfg config.ModelConfig
}

func NewOpenAIClient(cfg config.ModelConfig) *OpenAIClient { return &OpenAIClient{cfg: cfg} }

func (c *OpenAIClient) ProviderName() string { return "openai" }
func (c *OpenAIClient) ModelName() string    { return c.cfg.Model }
func (c *OpenAIClient) IsAvailable() bool    { return false }
func (c *OpenAIClient) Analyze(_ context.Context, _ AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
	return AnalysisResponse{}, ModelCallMeta{}, errors.New("openai provider not implemented")
}
