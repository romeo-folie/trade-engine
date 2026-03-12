package embeddings

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func TestNew_OpenAI(t *testing.T) {
	cfg := config.EmbeddingsConfig{
		Provider: "openai",
		APIKey:   "test-key",
	}
	emb, err := New(cfg)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if emb == nil {
		t.Fatal("expected an embedder, got nil")
	}
	// Check type if possible, or just successful return.
}

func TestNew_OpenAIFailsWithoutKey(t *testing.T) {
	cfg := config.EmbeddingsConfig{
		Provider: "openai",
		APIKey:   "",
	}
	_, err := New(cfg)
	if err == nil {
		t.Fatal("expected error for missing API key, got none")
	}
}

func TestNew_Ollama(t *testing.T) {
	cfg := config.EmbeddingsConfig{
		Provider: "ollama",
	}
	emb, err := New(cfg)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if emb == nil {
		t.Fatal("expected an embedder, got nil")
	}
}

func TestNew_UnknownProvider(t *testing.T) {
	cfg := config.EmbeddingsConfig{
		Provider: "unknown",
	}
	_, err := New(cfg)
	if err == nil {
		t.Fatal("expected error for unknown provider, got none")
	}
}
