package embeddings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOllamaEmbedder_EmbedBatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody map[string]any
		json.NewDecoder(r.Body).Decode(&reqBody)
		
		inputs, isArray := reqBody["input"].([]any)
		if !isArray {
			// Single input fallback
			inputs = []any{reqBody["input"]}
		}
		
		resp := map[string]any{
			"embeddings": [][]float32{},
		}
		
		for range inputs {
			resp["embeddings"] = append(resp["embeddings"].([][]float32), []float32{0.4, 0.5, 0.6})
		}
		
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	emb := newOllamaEmbedder(ts.URL, "test-model", 3)

	ctx := context.Background()
	texts := []string{"hello", "world"}
	res, err := emb.EmbedBatch(ctx, texts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(res) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(res))
	}
	if len(res[0]) != 3 {
		t.Fatalf("expected dimension 3, got %d", len(res[0]))
	}

	// Test single embed
	vec, err := emb.Embed(ctx, "single")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vec) != 3 {
		t.Fatalf("expected dimension 3, got %d", len(vec))
	}
}

func TestOllamaEmbedder_Error(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("server error"))
	}))
	defer ts.Close()

	emb := newOllamaEmbedder(ts.URL, "test-model", 3)

	_, err := emb.Embed(context.Background(), "error test")
	if err == nil {
		t.Fatal("expected error, got none")
	}
}
