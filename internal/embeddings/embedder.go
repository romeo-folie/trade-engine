// Package embeddings provides vector embedding generation for analyses, news,
// and entities.  Embeddings are stored in pgvector columns and used for
// semantic similarity search ("find similar past setups").
//
// Dimension decision (Phase 11):
//
//	Provider "openai"  → text-embedding-3-small → 1536 dimensions (default).
//	Provider "ollama"  → nomic-embed-text        → 768  dimensions.
//
//	All vector() columns in the migrations are sized to 1536.  If you switch to
//	Ollama you must resize the columns and run `te embeddings reindex`.
package embeddings

import (
	"context"
	"fmt"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// Embedder generates dense vector embeddings for arbitrary text.
type Embedder interface {
	// Embed returns a single embedding vector for the given text.
	Embed(ctx context.Context, text string) ([]float32, error)

	// EmbedBatch returns one embedding per input text.
	// Implementations may call the provider in a single batched request.
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)

	// Dimension returns the fixed vector length produced by this embedder.
	Dimension() int
}

// New creates the configured Embedder (openai or ollama).
// Returns an error if the provider is unknown or required credentials are absent.
func New(cfg config.EmbeddingsConfig) (Embedder, error) {
	switch cfg.Provider {
	case "openai", "":
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("embeddings: provider=openai requires OPENAI_API_KEY (set via %s)", cfg.APIKeyEnv)
		}
		model := cfg.Model
		if model == "" {
			model = "text-embedding-3-small"
		}
		return newOpenAIEmbedder(cfg.APIKey, model), nil

	case "ollama":
		model := cfg.Model
		if model == "" {
			model = "nomic-embed-text"
		}
		base := cfg.OllamaBaseURL
		if base == "" {
			base = "http://localhost:11434"
		}
		return newOllamaEmbedder(base, model), nil

	default:
		return nil, fmt.Errorf("embeddings: unknown provider %q (valid: openai, ollama)", cfg.Provider)
	}
}
