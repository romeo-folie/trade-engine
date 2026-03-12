package embeddings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAIEmbedder_EmbedBatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		
		var reqBody map[string]any
		json.NewDecoder(r.Body).Decode(&reqBody)
		
		inputs, _ := reqBody["input"].([]any)
		
		resp := map[string]any{
			"data": []map[string]any{},
		}
		
		for i := range inputs {
			resp["data"] = append(resp["data"].([]map[string]any), map[string]any{
				"embedding": []float32{0.1, 0.2, 0.3},
				"index":     i,
			})
		}
		
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	emb := newOpenAIEmbedder("test-key", "test-model", 3)
	emb.baseURL = ts.URL

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

func TestOpenAIEmbedder_Error(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad request"))
	}))
	defer ts.Close()

	emb := newOpenAIEmbedder("test-key", "test-model", 3)
	emb.baseURL = ts.URL

	_, err := emb.Embed(context.Background(), "error test")
	if err == nil {
		t.Fatal("expected error, got none")
	}
}
