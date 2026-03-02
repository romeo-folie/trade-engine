package embeddings

import (
	"context"
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// Worker is a background goroutine that finds rows with NULL embedding columns,
// generates embeddings via the configured provider, and writes them back.
//
// It processes analyses, news_items, and entities in each cycle.
// A semaphore (batch size) prevents overwhelming the API.
type Worker struct {
	embedder Embedder
	cfg      config.EmbeddingsConfig
}

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
	interval := time.Duration(w.cfg.WorkerIntervalSec) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	logger.Info("embedding worker started",
		"provider", w.cfg.Provider,
		"interval_sec", w.cfg.WorkerIntervalSec,
		"batch_size", w.cfg.BatchSize,
	)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run immediately on start.
	w.RunCycle(ctx)

	for {
		select {
		case <-ctx.Done():
			logger.Info("embedding worker stopped")
			return
		case <-ticker.C:
			w.RunCycle(ctx)
		}
	}
}

// RunCycle processes one batch of unembedded rows across all three tables.
// Exported for use by the `te embeddings reindex` command.
func (w *Worker) RunCycle(ctx context.Context) {
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
		// Embed the reasoning; fall back to raw prompt when reasoning is empty.
		text := r.Reasoning
		if text == "" {
			text = r.RawPrompt
		}
		texts[i] = text
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

// EmbedText is a convenience helper used by the reindex command.
func (w *Worker) EmbedText(ctx context.Context, text string) ([]float32, error) {
	return w.embedder.Embed(ctx, text)
}
