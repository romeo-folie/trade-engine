package embeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const openAIEmbeddingURL = "https://api.openai.com/v1/embeddings"

// openAIEmbedder calls the OpenAI Embeddings API.
type openAIEmbedder struct {
	apiKey string
	model  string
	client *http.Client
	dim    int
}

// defaultOpenAIDim is the dimension for text-embedding-3-small, used when
// config.Dimension is 0 (not explicitly set).
const defaultOpenAIDim = 1536

func newOpenAIEmbedder(apiKey, model string, dim int) *openAIEmbedder {
	if dim <= 0 {
		dim = defaultOpenAIDim
	}
	return &openAIEmbedder{
		apiKey: apiKey,
		model:  model,
		client: &http.Client{Timeout: 30 * time.Second},
		dim:    dim,
	}
}

func (e *openAIEmbedder) Dimension() int { return e.dim }

func (e *openAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := e.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

func (e *openAIEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body := map[string]any{
		"model": e.model,
		"input": texts,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai embed marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIEmbeddingURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("openai embed request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai embed http: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		errBody := string(respBytes)
		if len(errBody) > 300 {
			errBody = errBody[:300] + "…"
		}
		return nil, fmt.Errorf("openai embed HTTP %d: %s", resp.StatusCode, errBody)
	}

	var apiResp struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBytes, &apiResp); err != nil {
		return nil, fmt.Errorf("openai embed parse: %w", err)
	}
	if len(apiResp.Data) != len(texts) {
		return nil, fmt.Errorf("openai embed: expected %d vectors, got %d", len(texts), len(apiResp.Data))
	}

	// API may return results out of order; sort by Index.
	out := make([][]float32, len(texts))
	for _, d := range apiResp.Data {
		out[d.Index] = d.Embedding
	}
	return out, nil
}
