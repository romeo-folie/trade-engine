package embeddings

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// Worker is a background goroutine that finds rows with NULL embedding columns,
// generates embeddings via the configured provider, and writes them back.
//
// Embedding text alignment
//
// analyses:  compact indicator snapshot — same format as the engine's
//            similarity search query so stored and query vectors occupy the
//            same region of the embedding space.
//            Format: "<TICKER> RSI:<v> EMA20:<v> MACD:<v> ADX:<v>"
//
// news_items: headline + summary (natural language).
// entities:   name + description (natural language).
//
// Backoff
//
// After maxConsecutiveErrors cycles with zero progress (API down, invalid key, …)
// the worker doubles its sleep interval up to maxBackoffInterval, then resets on
// the next successful batch.
type Worker struct {
	embedder Embedder
	cfg      config.EmbeddingsConfig
}

const (
	maxConsecutiveErrors = 3
	maxBackoffInterval   = 5 * time.Minute
)

// NewWorker creates a Worker. Returns an error if the embedder cannot be
// initialised (e.g. missing API key).
func NewWorker(cfg config.EmbeddingsConfig) (*Worker, error) {
	emb, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("embedding worker: %w", err)
	}
	return &Worker{embedder: emb, cfg: cfg}, nil
}

// Run starts the background polling loop. Blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	baseInterval := time.Duration(w.cfg.WorkerIntervalSec) * time.Second
	if baseInterval <= 0 {
		baseInterval = 30 * time.Second
	}
	logger.Info("embedding worker started",
		"provider", w.cfg.Provider,
		"interval_sec", w.cfg.WorkerIntervalSec,
		"batch_size", w.cfg.BatchSize,
	)

	currentInterval := baseInterval
	consecutiveEmpty := 0 // cycles with zero rows updated

	// Run immediately on start.
	updated := w.RunCycle(ctx)
	if updated == 0 {
		consecutiveEmpty++
	} else {
		consecutiveEmpty = 0
		currentInterval = baseInterval
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("embedding worker stopped")
			return
		case <-time.After(currentInterval):
			updated = w.RunCycle(ctx)
			if updated == 0 {
				consecutiveEmpty++
				if consecutiveEmpty >= maxConsecutiveErrors {
					currentInterval = min(currentInterval*2, maxBackoffInterval)
					logger.Warn("embedding worker backing off",
						"consecutive_empty", consecutiveEmpty,
						"next_interval_sec", currentInterval.Seconds())
				}
			} else {
				if consecutiveEmpty >= maxConsecutiveErrors {
					logger.Info("embedding worker recovered, resetting interval")
				}
				consecutiveEmpty = 0
				currentInterval = baseInterval
			}
		}
	}
}

// RunCycle processes one batch of unembedded rows across all three tables.
// Returns total rows updated. Exported for use by `te embeddings reindex`.
func (w *Worker) RunCycle(ctx context.Context) int {
	limit := w.cfg.BatchSize
	if limit <= 0 {
		limit = 50
	}

	total := 0
	total += w.embedAnalyses(ctx, limit)
	total += w.embedNews(ctx, limit)
	total += w.embedEntities(ctx, limit)

	if total > 0 {
		logger.Info("embedding cycle complete", "rows_updated", total)
	}
	return total
}

// ── Per-table embedding ───────────────────────────────────────────────────────

func (w *Worker) embedAnalyses(ctx context.Context, limit int) int {
	rows, err := store.GetAnalysesWithoutEmbedding(ctx, limit)
	if err != nil {
		logger.Warn("embedding worker: fetch analyses", "err", err.Error())
		return 0
	}
	if len(rows) == 0 {
		return 0
	}

	texts := make([]string, len(rows))
	for i, r := range rows {
		// Build compact indicator text — same format as engine search query
		// so stored vectors and query vectors share the same embedding space.
		texts[i] = analysisEmbedText(r.Ticker, r.IndicatorsJSON)
	}

	vecs, err := w.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		logger.Warn("embedding worker: embed analyses batch", "err", err.Error())
		return 0
	}

	updated := 0
	for i, r := range rows {
		if i >= len(vecs) || vecs[i] == nil {
			continue
		}
		if err := store.UpdateAnalysisEmbedding(ctx, r.ID, vecs[i]); err != nil {
			logger.Warn("embedding worker: update analysis embedding", "id", r.ID, "err", err.Error())
			continue
		}
		updated++
	}
	return updated
}

func (w *Worker) embedNews(ctx context.Context, limit int) int {
	rows, err := store.GetNewsWithoutEmbedding(ctx, limit)
	if err != nil {
		logger.Warn("embedding worker: fetch news", "err", err.Error())
		return 0
	}
	if len(rows) == 0 {
		return 0
	}

	texts := make([]string, len(rows))
	for i, r := range rows {
		texts[i] = r.Headline
		if r.Summary != "" {
			texts[i] += " " + r.Summary
		}
	}

	vecs, err := w.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		logger.Warn("embedding worker: embed news batch", "err", err.Error())
		return 0
	}

	updated := 0
	for i, r := range rows {
		if i >= len(vecs) || vecs[i] == nil {
			continue
		}
		if err := store.UpdateNewsEmbedding(ctx, r.ID, vecs[i]); err != nil {
			logger.Warn("embedding worker: update news embedding", "id", r.ID, "err", err.Error())
			continue
		}
		updated++
	}
	return updated
}

func (w *Worker) embedEntities(ctx context.Context, limit int) int {
	rows, err := store.GetEntitiesWithoutEmbedding(ctx, limit)
	if err != nil {
		logger.Warn("embedding worker: fetch entities", "err", err.Error())
		return 0
	}
	if len(rows) == 0 {
		return 0
	}

	texts := make([]string, len(rows))
	for i, r := range rows {
		texts[i] = r.Name
		if r.Description != "" {
			texts[i] += " — " + r.Description
		}
	}

	vecs, err := w.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		logger.Warn("embedding worker: embed entities batch", "err", err.Error())
		return 0
	}

	updated := 0
	for i, r := range rows {
		if i >= len(vecs) || vecs[i] == nil {
			continue
		}
		if err := store.UpdateEntityEmbedding(ctx, r.ID, vecs[i]); err != nil {
			logger.Warn("embedding worker: update entity embedding", "id", r.ID, "err", err.Error())
			continue
		}
		updated++
	}
	return updated
}

// ── Indicator text builder ────────────────────────────────────────────────────

// indicatorJSON is a minimal struct for unmarshalling the stored indicators JSONB.
// Only the key fields used for semantic similarity are extracted.
type indicatorJSON struct {
	RSI          float64 `json:"RSI"`
	EMA20        float64 `json:"EMA20"`
	MACDLine     float64 `json:"MACDLine"`
	MACDSignal   float64 `json:"MACDSignal"`
	ADX          float64 `json:"ADX"`
}

// analysisEmbedText builds a compact, deterministic indicator snapshot string.
// This is the canonical embedding text for analyses — both the worker (at index
// time) and engine.runAnalysis (at search time) must use this exact format so
// stored vectors and query vectors occupy the same region of the embedding space.
//
// Format: "<TICKER> RSI:<v> EMA20:<v> MACD:<v> ADX:<v>"
func analysisEmbedText(ticker string, indicatorsJSON []byte) string {
	var ind indicatorJSON
	// Best-effort parse; zero values produce a valid (if less precise) string.
	_ = json.Unmarshal(indicatorsJSON, &ind)
	return fmt.Sprintf("%s RSI:%.1f EMA20:%.2f MACD:%.4f ADX:%.1f",
		ticker, ind.RSI, ind.EMA20, ind.MACDLine, ind.ADX)
}

// AnalysisEmbedText is exported so engine.runAnalysis can use the same format
// for search queries, guaranteeing vector-space alignment.
func AnalysisEmbedText(ticker string, rsi, ema20, macdLine, adx float64) string {
	return fmt.Sprintf("%s RSI:%.1f EMA20:%.2f MACD:%.4f ADX:%.1f",
		ticker, rsi, ema20, macdLine, adx)
}

// EmbedText is a convenience helper used by the reindex command.
func (w *Worker) EmbedText(ctx context.Context, text string) ([]float32, error) {
	return w.embedder.Embed(ctx, text)
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
