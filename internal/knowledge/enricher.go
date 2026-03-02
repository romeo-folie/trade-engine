package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// maxEnricherConcurrency caps the number of simultaneous LLM extraction calls
// fired by the enricher.  Without a bound, 10 tickers × 30 articles = 300
// concurrent calls, immediately exhausting the rate limit.
const maxEnricherConcurrency = 5

// Enricher extracts entity relationships from news headlines and summaries and
// writes them to the knowledge graph as auto-generated edges.
//
// The extraction uses the configured primary LLM via a dedicated system prompt
// that is separate from the trading analysis prompt.  Auto-generated
// relationships are stored with auto_generated=TRUE so they can be reviewed and
// confirmed (or rejected) via `te graph review`.
type Enricher struct {
	cfg    *config.Config
	callFn ExtractionCallFn
	sem    chan struct{} // bounds concurrent LLM calls
}

// ExtractionCallFn is the function the enricher calls to run the extraction
// prompt.  Injected rather than depending on the full analysis.Analyzer so
// the knowledge package stays free of circular imports.
//
// It receives (ctx, systemPrompt, userMessage) and returns the raw JSON string
// returned by the model.
type ExtractionCallFn func(ctx context.Context, system, user string) (string, error)

// NewEnricher creates an Enricher with the given LLM call function.
func NewEnricher(cfg *config.Config, callFn ExtractionCallFn) *Enricher {
	return &Enricher{
		cfg:    cfg,
		callFn: callFn,
		sem:    make(chan struct{}, maxEnricherConcurrency),
	}
}

// EnrichNewsItem runs relationship extraction on a single news article
// (identified by newsItemID for entity-mention bookkeeping; pass empty string
// to skip entity-mention recording).
//
// The call is non-blocking: work is dispatched to a bounded goroutine pool
// (maxEnricherConcurrency slots).  If all slots are occupied the call blocks
// only until a slot is free, preventing unbounded goroutine creation.
// Each goroutine has a defer/recover so a panic in one article cannot
// propagate to the calling pipeline.
func (en *Enricher) EnrichNewsItem(ctx context.Context, newsItemID, headline, summary string) {
	en.sem <- struct{}{} // acquire slot (blocks if at capacity)
	go func() {
		defer func() { <-en.sem }() // release slot
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("knowledge enrichment panic",
					"news_id", newsItemID,
					"panic", fmt.Sprintf("%v", rec),
				)
			}
		}()
		if err := en.enrich(ctx, newsItemID, headline, summary); err != nil {
			logger.Warn("knowledge enrichment failed",
				"news_id", newsItemID,
				"err", err.Error(),
			)
		}
	}()
}

func (en *Enricher) enrich(ctx context.Context, newsItemID, headline, summary string) error {
	system, user := buildExtractionPrompt(headline, summary)

	raw, err := en.callFn(ctx, system, user)
	if err != nil {
		return fmt.Errorf("extraction call: %w", err)
	}

	rels, err := parseExtractionResponse(raw)
	if err != nil {
		return fmt.Errorf("parse extraction response: %w", err)
	}

	for _, r := range rels {
		if err := en.persistExtractedRelationship(ctx, newsItemID, r); err != nil {
			logger.Warn("persist extracted relationship failed",
				"from", r.FromEntity, "to", r.ToEntity,
				"type", r.RelationshipType,
				"err", err.Error(),
			)
		}
	}
	return nil
}

// ── Prompt ────────────────────────────────────────────────────────────────────

var extractionSystem = `You are a financial knowledge-graph extraction engine.
Given a news headline and optional summary, extract any company relationships
described in the text.

Respond with ONLY a valid JSON array.  Each element must match this schema:
[
  {
    "from_entity":       "<full company name>",
    "from_ticker":       "<stock ticker or empty string>",
    "to_entity":         "<full company name>",
    "to_ticker":         "<stock ticker or empty string>",
    "relationship_type": "<one of: supplies_to | supplied_by | competes_with | subsidiary_of | parent_of | partner_of | manufactures_for | manufactured_by | regulates | regulated_by | affects_contract | affected_by_event>",
    "confidence":        <float 0.0–1.0>,
    "description":       "<one-sentence explanation>"
  }
]

Rules:
- Only include relationships that are explicitly stated or strongly implied.
- If no relationships are found, return an empty array: [].
- Confidence >= 0.7 for stated facts; 0.5–0.7 for strong implication; < 0.5 for inference.
- Do NOT invent relationships not present in the text.`

func buildExtractionPrompt(headline, summary string) (system, user string) {
	var b strings.Builder
	b.WriteString("Headline: ")
	b.WriteString(headline)
	if summary != "" {
		b.WriteString("\n\nSummary: ")
		b.WriteString(summary)
	}
	return extractionSystem, b.String()
}

// ── Response parsing ──────────────────────────────────────────────────────────

type extractedRelationship struct {
	FromEntity       string  `json:"from_entity"`
	FromTicker       string  `json:"from_ticker"`
	ToEntity         string  `json:"to_entity"`
	ToTicker         string  `json:"to_ticker"`
	RelationshipType string  `json:"relationship_type"`
	Confidence       float64 `json:"confidence"`
	Description      string  `json:"description"`
}

func parseExtractionResponse(raw string) ([]extractedRelationship, error) {
	// Strip potential markdown code fences.
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var rels []extractedRelationship
	if err := json.Unmarshal([]byte(raw), &rels); err != nil {
		return nil, fmt.Errorf("unmarshal: %w (raw: %.200s)", err, raw)
	}
	return rels, nil
}

// ── Persistence ───────────────────────────────────────────────────────────────

func (en *Enricher) persistExtractedRelationship(ctx context.Context, newsItemID string, r extractedRelationship) error {
	// Upsert source entity (auto-generated entities have only name + ticker;
	// a human can later enrich description/sector/country via `te graph review`).
	fromID, err := store.UpsertEntity(ctx, store.Entity{
		EntityType: "company",
		Name:       r.FromEntity,
		Ticker:     r.FromTicker,
	})
	if err != nil {
		return fmt.Errorf("upsert from_entity %q: %w", r.FromEntity, err)
	}

	// Upsert destination entity.
	toID, err := store.UpsertEntity(ctx, store.Entity{
		EntityType: "company",
		Name:       r.ToEntity,
		Ticker:     r.ToTicker,
	})
	if err != nil {
		return fmt.Errorf("upsert to_entity %q: %w", r.ToEntity, err)
	}

	// Upsert the relationship as auto-generated.
	conf := r.Confidence
	if _, err := store.UpsertRelationship(ctx, store.Relationship{
		FromEntityID:     fromID,
		ToEntityID:       toID,
		RelationshipType: r.RelationshipType,
		Strength:         r.Confidence,
		Description:      r.Description,
		AutoGenerated:    true,
		Confidence:       &conf,
		ValidFrom:        timePtr(time.Now()),
	}); err != nil {
		return fmt.Errorf("upsert relationship: %w", err)
	}

	// Record entity mentions only when a valid news_item UUID is provided.
	// Passing an empty string (no UUID available) skips this to avoid FK violations.
	if newsItemID != "" {
		rel := r.Confidence
		_ = store.UpsertEntityMention(ctx, fromID, newsItemID, &rel)
		_ = store.UpsertEntityMention(ctx, toID, newsItemID, &rel)
	}

	logger.Info("knowledge enrichment: new relationship extracted",
		"from", r.FromEntity, "to", r.ToEntity,
		"type", r.RelationshipType,
		"confidence", r.Confidence,
	)
	return nil
}

func timePtr(t time.Time) *time.Time { return &t }
