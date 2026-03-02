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

// ollamaEmbedder calls a local Ollama server for embeddings.
// Default model: nomic-embed-text (768 dimensions).
type ollamaEmbedder struct {
	baseURL string
	model   string
	client  *http.Client
	dim     int
}

func newOllamaEmbedder(baseURL, model string) *ollamaEmbedder {
	dim := 768 // nomic-embed-text default
	return &ollamaEmbedder{
		baseURL: baseURL,
		model:   model,
		client:  &http.Client{Timeout: 60 * time.Second},
		dim:     dim,
	}
}

func (e *ollamaEmbedder) Dimension() int { return e.dim }

func (e *ollamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	url := e.baseURL + "/api/embed"
	body := map[string]any{
		"model": e.model,
		"input": text,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("ollama embed marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("ollama embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed http: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		errBody := string(respBytes)
		if len(errBody) > 300 {
			errBody = errBody[:300] + "…"
		}
		return nil, fmt.Errorf("ollama embed HTTP %d: %s", resp.StatusCode, errBody)
	}

	// Ollama /api/embed returns { "embeddings": [[...]] }
	var apiResp struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(respBytes, &apiResp); err != nil {
		return nil, fmt.Errorf("ollama embed parse: %w", err)
	}
	if len(apiResp.Embeddings) == 0 {
		return nil, fmt.Errorf("ollama embed: empty response")
	}
	return apiResp.Embeddings[0], nil
}

func (e *ollamaEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	// Ollama /api/embed accepts an array input as of v0.3.
	url := e.baseURL + "/api/embed"
	body := map[string]any{
		"model": e.model,
		"input": texts,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("ollama embed batch marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("ollama embed batch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed batch http: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		errBody := string(respBytes)
		if len(errBody) > 300 {
			errBody = errBody[:300] + "…"
		}
		return nil, fmt.Errorf("ollama embed batch HTTP %d: %s", resp.StatusCode, errBody)
	}

	var apiResp struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(respBytes, &apiResp); err != nil {
		return nil, fmt.Errorf("ollama embed batch parse: %w", err)
	}
	if len(apiResp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama embed batch: expected %d vectors, got %d", len(texts), len(apiResp.Embeddings))
	}
	return apiResp.Embeddings, nil
}
