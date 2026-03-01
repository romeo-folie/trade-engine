package backtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// PromptCacheEntry is a single cached model response keyed by (hash, provider, model).
type PromptCacheEntry struct {
	PromptHash   string
	Provider     string
	Model        string
	Response     string // raw JSON response text
	InputTokens  int
	OutputTokens int
	CreatedAt    time.Time
}

// PromptCache wraps the prompt_cache table with hit/miss tracking.
// It is safe for concurrent use.
type PromptCache struct {
	hits   atomic.Int64
	misses atomic.Int64
}

// NewPromptCache creates a PromptCache backed by the live database.
func NewPromptCache() *PromptCache {
	return &PromptCache{}
}

// RequestHash computes a deterministic SHA-256 digest of an AnalysisRequest.
// The hash is computed from the JSON-serialised request, so any change to
// the ticker, strategy, candles, indicators, or news produces a distinct key.
func RequestHash(req analysis.AnalysisRequest) string {
	data, _ := json.Marshal(req)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Get looks up a cached response by (hash, provider, model).
// Returns the entry and true on a hit; nil and false on a miss.
func (c *PromptCache) Get(ctx context.Context, hash, provider, model string) (*PromptCacheEntry, bool, error) {
	row := store.GetDB().QueryRow(ctx, `
		SELECT prompt_hash, model_provider, model_name,
		       response, token_count_input, token_count_output, created_at
		FROM prompt_cache
		WHERE prompt_hash = $1 AND model_provider = $2 AND model_name = $3
	`, hash, provider, model)

	var e PromptCacheEntry
	err := row.Scan(
		&e.PromptHash, &e.Provider, &e.Model,
		&e.Response, &e.InputTokens, &e.OutputTokens, &e.CreatedAt,
	)
	if err != nil {
		if store.IsNoRows(err) {
			c.misses.Add(1)
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("prompt cache get: %w", err)
	}
	c.hits.Add(1)
	return &e, true, nil
}

// Put inserts a cache entry. Duplicate (hash, provider, model) is ignored
// (ON CONFLICT DO NOTHING in the schema).
func (c *PromptCache) Put(ctx context.Context, e PromptCacheEntry) error {
	_, err := store.GetDB().Exec(ctx, `
		INSERT INTO prompt_cache (
			prompt_hash, model_provider, model_name,
			response, token_count_input, token_count_output
		) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (prompt_hash, model_provider, model_name) DO NOTHING
	`, e.PromptHash, e.Provider, e.Model,
		e.Response, e.InputTokens, e.OutputTokens,
	)
	if err != nil {
		return fmt.Errorf("prompt cache put: %w", err)
	}
	return nil
}

// HitRate returns the cache hit rate (0–1) over all lookups since creation.
// Returns 0 if no lookups have been made.
func (c *PromptCache) HitRate() float64 {
	hits := c.hits.Load()
	misses := c.misses.Load()
	total := hits + misses
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}

// Stats returns (hits, misses) counters.
func (c *PromptCache) Stats() (hits, misses int64) {
	return c.hits.Load(), c.misses.Load()
}
