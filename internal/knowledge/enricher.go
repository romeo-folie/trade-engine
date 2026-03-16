package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// maxEnricherConcurrency caps simultaneous LLM extraction calls.
// Keep this low to avoid bursty provider RPM spikes.
const maxEnricherConcurrency = 2

// validRelationshipTypes is the canonical set of relationship_type enum values.
// Must be kept in sync with the Postgres relationship_type enum in migrations/.
// The extraction prompt's "one of:" list is generated from this map (sorted) so
// the two never drift.
var validRelationshipTypes = map[string]bool{
	"supplies_to":       true,
	"supplied_by":       true,
	"competes_with":     true,
	"subsidiary_of":     true,
	"parent_of":         true,
	"partner_of":        true,
	"sector_member":     true,
	"manufactures_for":  true,
	"manufactured_by":   true,
	"regulates":         true,
	"regulated_by":      true,
	"affects_contract":  true,
	"affected_by_event": true,
	"invests_in":        true,
	"invested_in_by":    true,
}

// relationshipTypeList returns a sorted, pipe-delimited string of all valid
// relationship types, used to build the extraction prompt.
// Derived from validRelationshipTypes so the two never drift.
func relationshipTypeList() string {
	types := make([]string, 0, len(validRelationshipTypes))
	for t := range validRelationshipTypes {
		types = append(types, t)
	}
	sort.Strings(types)
	return strings.Join(types, " | ")
}

// allowedRelationshipTypesOnce builds the prompt fragment once at startup.
var allowedRelationshipTypesOnce = sync.OnceValue(relationshipTypeList)

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

	rateMu      sync.Mutex
	minInterval time.Duration
	lastCall    time.Time
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
	rpm := cfg.Markets.News.ExtractionRPM
	if rpm <= 0 {
		rpm = 20
	}
	return &Enricher{
		cfg:         cfg,
		callFn:      callFn,
		sem:         make(chan struct{}, maxEnricherConcurrency),
		minInterval: time.Minute / time.Duration(rpm),
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

	en.acquireRateLimit(ctx)
	raw, err := en.callFn(ctx, system, user)
	if err != nil {
		if isRateLimitErr(err) {
			en.applyBackoff()
		}
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

// buildExtractionSystemPrompt builds the extraction system prompt, injecting the
// allowed relationship types from validRelationshipTypes so the prompt and the
// validator can never drift.  Called once at package init via the
// extractionSystem package-level var.
func buildExtractionSystemPrompt() string {
	return `You are a financial knowledge-graph extraction engine.
Given a news headline and optional summary, extract any company relationships
described in the text.

Respond with ONLY a valid JSON array.  Each element must match this schema:
[
  {
    "from_entity":       "<full company name>",
    "from_ticker":       "<stock ticker or empty string>",
    "to_entity":         "<full company name>",
    "to_ticker":         "<stock ticker or empty string>",
    "relationship_type": "<one of: ` + allowedRelationshipTypesOnce() + `>",
    "confidence":        <float 0.0–1.0>,
    "description":       "<one-sentence explanation>"
  }
]

Rules:
- Only include relationships that are explicitly stated or strongly implied.
- If no relationships are found, return an empty array: [].
- Confidence >= 0.7 for stated facts; 0.5–0.7 for strong implication; < 0.5 for inference.
- Do NOT invent relationships not present in the text.`
}

var extractionSystem = buildExtractionSystemPrompt()

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

func (en *Enricher) acquireRateLimit(ctx context.Context) {
	en.rateMu.Lock()
	wait := time.Until(en.lastCall.Add(en.minInterval))
	en.rateMu.Unlock()
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
	en.rateMu.Lock()
	en.lastCall = time.Now().Add(100 * time.Millisecond) // small jitter against burst alignment
	en.rateMu.Unlock()
}

func (en *Enricher) applyBackoff() {
	en.rateMu.Lock()
	if en.minInterval < 2*time.Second {
		en.lastCall = time.Now().Add(2 * time.Second)
	} else {
		en.lastCall = time.Now().Add(en.minInterval * 2)
	}
	en.rateMu.Unlock()
}

func isRateLimitErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "429") || strings.Contains(s, "rate limit") || strings.Contains(s, "rate_limit")
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
	raw = sanitizeJSONPayload(raw)
	if raw == "" {
		return nil, nil
	}

	// Expected path: JSON array.
	var rels []extractedRelationship
	if err := json.Unmarshal([]byte(raw), &rels); err == nil {
		return rels, nil
	}

	// Tolerate single object payloads.
	var single extractedRelationship
	if err := json.Unmarshal([]byte(raw), &single); err == nil {
		return []extractedRelationship{single}, nil
	}

	// Last-resort: parse line-delimited or concatenated objects.
	dec := json.NewDecoder(strings.NewReader(raw))
	out := make([]extractedRelationship, 0, 4)
	for {
		var item extractedRelationship
		if err := dec.Decode(&item); err != nil {
			break
		}
		out = append(out, item)
	}
	if len(out) > 0 {
		return out, nil
	}

	return nil, fmt.Errorf("unmarshal extraction payload (raw: %.200s)", raw)
}

func sanitizeJSONPayload(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	// Sometimes models prepend prose before JSON; keep from first JSON token.
	if i := strings.IndexFunc(raw, func(r rune) bool { return r == '[' || r == '{' }); i > 0 {
		raw = raw[i:]
	}
	return strings.TrimSpace(raw)
}

// ── Persistence ───────────────────────────────────────────────────────────────

func (en *Enricher) persistExtractedRelationship(ctx context.Context, newsItemID string, r extractedRelationship) error {
	r.FromEntity = normalizeEntityName(r.FromEntity)
	r.ToEntity = normalizeEntityName(r.ToEntity)
	r.FromTicker = normalizeTicker(r.FromTicker)
	r.ToTicker = normalizeTicker(r.ToTicker)
	r.RelationshipType = strings.TrimSpace(r.RelationshipType)
	if r.RelationshipType == "" {
		return fmt.Errorf("empty relationship_type")
	}
	if !validRelationshipTypes[r.RelationshipType] {
		logger.Warn("skipping relationship: unknown type not in enum",
			"type", r.RelationshipType,
			"from", r.FromEntity,
			"to", r.ToEntity,
		)
		return nil
	}
	if r.FromEntity == "" || r.ToEntity == "" {
		return fmt.Errorf("empty entity name(s)")
	}

	fromID, err := resolveEntityID(ctx, r.FromEntity, r.FromTicker)
	if err != nil {
		return fmt.Errorf("upsert from_entity %q: %w", r.FromEntity, err)
	}
	toID, err := resolveEntityID(ctx, r.ToEntity, r.ToTicker)
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

func resolveEntityID(ctx context.Context, name, ticker string) (string, error) {
	if ticker != "" {
		existing, err := store.GetEntityByTicker(ctx, ticker)
		if err == nil && existing != nil {
			return existing.ID, nil
		}
	}
	return store.UpsertEntity(ctx, store.Entity{EntityType: "company", Name: name, Ticker: ticker})
}

func normalizeTicker(s string) string {
	s = strings.TrimSpace(strings.ToUpper(s))
	return strings.Trim(s, " ")
}

func normalizeEntityName(s string) string {
	s = html.UnescapeString(strings.TrimSpace(s))
	if s == "" {
		return s
	}
	s = strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) })
	s = strings.Join(strings.Fields(s), " ")

	// light canonicalization to reduce fragmentation from legal suffix variance
	for _, suffix := range []string{", Inc.", " Inc.", ", Inc", " Inc", ", Corporation", " Corporation", ", Corp.", " Corp.", ", Corp", " Corp"} {
		if strings.HasSuffix(s, suffix) {
			s = strings.TrimSuffix(s, suffix)
			break
		}
	}
	return strings.TrimSpace(s)
}
