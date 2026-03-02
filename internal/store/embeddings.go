package store

import (
	"context"
	"fmt"

	pgvector "github.com/pgvector/pgvector-go"
)

// ── Row types ─────────────────────────────────────────────────────────────────

// AnalysisForEmbedding holds the minimal fields needed for embedding generation.
type AnalysisForEmbedding struct {
	ID        string
	Reasoning string
	RawPrompt string
}

// NewsForEmbedding holds fields used to build the embedding text for a news item.
type NewsForEmbedding struct {
	ID       string
	Headline string
	Summary  string
}

// EntityForEmbedding holds fields used to build the embedding text for an entity.
type EntityForEmbedding struct {
	ID          string
	Name        string
	Description string
}

// SimilarAnalysis is a past analysis returned by semantic similarity search.
type SimilarAnalysis struct {
	ID         string
	Ticker     string
	Action     string
	Confidence float64
	Reasoning  string
	CreatedAt  string // formatted date string
	// Trade outcome fields (nullable — nil if no trade was opened from this analysis).
	TradeOutcomePct *float64 // positive = profit, negative = loss
}

// ── Fetch without embedding ───────────────────────────────────────────────────

// GetAnalysesWithoutEmbedding returns up to limit analyses that have a NULL embedding.
func GetAnalysesWithoutEmbedding(ctx context.Context, limit int) ([]AnalysisForEmbedding, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, COALESCE(reasoning,''), COALESCE(raw_prompt,'')
		FROM analyses
		WHERE embedding IS NULL
		ORDER BY created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("get analyses without embedding: %w", err)
	}
	defer rows.Close()
	var out []AnalysisForEmbedding
	for rows.Next() {
		var r AnalysisForEmbedding
		if err := rows.Scan(&r.ID, &r.Reasoning, &r.RawPrompt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetNewsWithoutEmbedding returns up to limit news items without embeddings.
func GetNewsWithoutEmbedding(ctx context.Context, limit int) ([]NewsForEmbedding, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, headline, COALESCE(summary,'')
		FROM news_items
		WHERE embedding IS NULL
		ORDER BY published_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("get news without embedding: %w", err)
	}
	defer rows.Close()
	var out []NewsForEmbedding
	for rows.Next() {
		var r NewsForEmbedding
		if err := rows.Scan(&r.ID, &r.Headline, &r.Summary); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetEntitiesWithoutEmbedding returns up to limit entities without embeddings.
func GetEntitiesWithoutEmbedding(ctx context.Context, limit int) ([]EntityForEmbedding, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, name, COALESCE(description,'')
		FROM entities
		WHERE embedding IS NULL
		ORDER BY created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("get entities without embedding: %w", err)
	}
	defer rows.Close()
	var out []EntityForEmbedding
	for rows.Next() {
		var r EntityForEmbedding
		if err := rows.Scan(&r.ID, &r.Name, &r.Description); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── Embedding updates ─────────────────────────────────────────────────────────

// UpdateAnalysisEmbedding writes a vector embedding to an analyses row.
func UpdateAnalysisEmbedding(ctx context.Context, id string, vec []float32) error {
	_, err := GetDB().Exec(ctx,
		`UPDATE analyses SET embedding = $1 WHERE id = $2`,
		pgvector.NewVector(vec), id)
	if err != nil {
		return fmt.Errorf("update analysis embedding %s: %w", id, err)
	}
	return nil
}

// UpdateNewsEmbedding writes a vector embedding to a news_items row.
func UpdateNewsEmbedding(ctx context.Context, id string, vec []float32) error {
	_, err := GetDB().Exec(ctx,
		`UPDATE news_items SET embedding = $1 WHERE id = $2`,
		pgvector.NewVector(vec), id)
	if err != nil {
		return fmt.Errorf("update news embedding %s: %w", id, err)
	}
	return nil
}

// UpdateEntityEmbedding writes a vector embedding to an entities row.
func UpdateEntityEmbedding(ctx context.Context, id string, vec []float32) error {
	_, err := GetDB().Exec(ctx,
		`UPDATE entities SET embedding = $1 WHERE id = $2`,
		pgvector.NewVector(vec), id)
	if err != nil {
		return fmt.Errorf("update entity embedding %s: %w", id, err)
	}
	return nil
}

// ── Semantic similarity search ────────────────────────────────────────────────

// FindSimilarAnalyses returns past analyses ranked by cosine similarity to the
// given query vector.  Only analyses with similarity >= threshold are returned.
// Results are joined with trades to surface the outcome when available.
func FindSimilarAnalyses(ctx context.Context, queryVec []float32, threshold float64, limit int) ([]SimilarAnalysis, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT
			a.id, a.ticker, a.action::text, a.confidence,
			COALESCE(a.reasoning,''),
			TO_CHAR(a.created_at, 'YYYY-MM-DD HH24:MI'),
			t.pnl_pct
		FROM analyses a
		LEFT JOIN LATERAL (
			SELECT pnl_pct
			FROM trades
			WHERE a.id = ANY(analysis_ids) AND status = 'closed'
			ORDER BY closed_at DESC
			LIMIT 1
		) t ON TRUE
		WHERE a.embedding IS NOT NULL
		  AND 1 - (a.embedding <=> $1::vector) >= $2
		ORDER BY a.embedding <=> $1::vector
		LIMIT $3`,
		pgvector.NewVector(queryVec), threshold, limit)
	if err != nil {
		return nil, fmt.Errorf("find similar analyses: %w", err)
	}
	defer rows.Close()

	var out []SimilarAnalysis
	for rows.Next() {
		var s SimilarAnalysis
		if err := rows.Scan(
			&s.ID, &s.Ticker, &s.Action, &s.Confidence,
			&s.Reasoning, &s.CreatedAt, &s.TradeOutcomePct,
		); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// FindSimilarNews returns news items ranked by cosine similarity to queryVec.
func FindSimilarNews(ctx context.Context, queryVec []float32, threshold float64, limit int) ([]NewsForEmbedding, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, headline, COALESCE(summary,'')
		FROM news_items
		WHERE embedding IS NOT NULL
		  AND 1 - (embedding <=> $1::vector) >= $2
		ORDER BY embedding <=> $1::vector
		LIMIT $3`,
		pgvector.NewVector(queryVec), threshold, limit)
	if err != nil {
		return nil, fmt.Errorf("find similar news: %w", err)
	}
	defer rows.Close()
	var out []NewsForEmbedding
	for rows.Next() {
		var r NewsForEmbedding
		if err := rows.Scan(&r.ID, &r.Headline, &r.Summary); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FindSimilarEntities returns entities ranked by cosine similarity to queryVec.
func FindSimilarEntities(ctx context.Context, queryVec []float32, threshold float64, limit int) ([]EntityForEmbedding, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT id, name, COALESCE(description,'')
		FROM entities
		WHERE embedding IS NOT NULL
		  AND 1 - (embedding <=> $1::vector) >= $2
		ORDER BY embedding <=> $1::vector
		LIMIT $3`,
		pgvector.NewVector(queryVec), threshold, limit)
	if err != nil {
		return nil, fmt.Errorf("find similar entities: %w", err)
	}
	defer rows.Close()
	var out []EntityForEmbedding
	for rows.Next() {
		var r EntityForEmbedding
		if err := rows.Scan(&r.ID, &r.Name, &r.Description); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClearAllEmbeddings sets the embedding column to NULL on all rows in analyses,
// news_items, and entities.  Used by `te embeddings reindex --force`.
func ClearAllEmbeddings(ctx context.Context) error {
	for _, table := range []string{"analyses", "news_items", "entities"} {
		// #nosec G201 — table name is from a controlled slice, not user input
		if _, err := GetDB().Exec(ctx,
			fmt.Sprintf("UPDATE %s SET embedding = NULL", table),
		); err != nil {
			return fmt.Errorf("clear embeddings on %s: %w", table, err)
		}
	}
	return nil
}

// EmbeddingCounts returns the number of rows with and without embeddings for
// each table (analyses, news_items, entities).
func EmbeddingCounts(ctx context.Context) (map[string][2]int, error) {
	tables := []string{"analyses", "news_items", "entities"}
	result := make(map[string][2]int, len(tables))
	for _, t := range tables {
		var withEmb, total int
		// #nosec G201 — table name is from a controlled slice, not user input
		if err := GetDB().QueryRow(ctx,
			fmt.Sprintf("SELECT COUNT(*) FILTER (WHERE embedding IS NOT NULL), COUNT(*) FROM %s", t),
		).Scan(&withEmb, &total); err != nil {
			return nil, fmt.Errorf("embedding counts for %s: %w", t, err)
		}
		result[t] = [2]int{withEmb, total}
	}
	return result, nil
}
