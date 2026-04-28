package analysis

import "github.com/romeo-folie/trade-engine/internal/config"

const deepseekAPIURL = "https://api.deepseek.com/v1/chat/completions"

// NewDeepSeekClient returns an OpenAIClient wired to the DeepSeek endpoint.
// DeepSeek's API is OpenAI-compatible: same request/response shape, Bearer auth.
func NewDeepSeekClient(cfg config.ModelConfig) *OpenAIClient {
	c := NewOpenAIClient(cfg)
	c.baseURL = deepseekAPIURL
	c.providerOverride = "deepseek"
	return c
}
