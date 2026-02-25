package analysis

import (
	"context"
	"errors"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// OllamaClient is a stub — not implemented in Phase 3.
type OllamaClient struct {
	cfg config.ModelConfig
}

func NewOllamaClient(cfg config.ModelConfig) *OllamaClient { return &OllamaClient{cfg: cfg} }

func (c *OllamaClient) ProviderName() string { return "ollama" }
func (c *OllamaClient) ModelName() string    { return c.cfg.Model }
func (c *OllamaClient) IsAvailable() bool    { return false }
func (c *OllamaClient) Analyze(_ context.Context, _ AnalysisRequest) (AnalysisResponse, ModelCallMeta, error) {
	return AnalysisResponse{}, ModelCallMeta{}, errors.New("ollama provider not implemented")
}
