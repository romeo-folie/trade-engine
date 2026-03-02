// Package knowledge maintains the entity–relationship graph used to enrich
// LLM analysis prompts with structured company, sector, and supply-chain context.
//
// Architecture:
//   - store/entities.go + store/relationships.go own all DB I/O.
//   - knowledge/graph.go provides a thin facade that callers can import without
//     depending on the store package directly.
//   - knowledge/context.go builds formatted text blocks for prompt injection.
//   - knowledge/enricher.go auto-extracts relationships from news articles.
package knowledge

import (
	"context"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// ── Re-exports ────────────────────────────────────────────────────────────────
// Re-export store types so callers don't need two imports.

// Entity is an alias for store.Entity.
type Entity = store.Entity

// Relationship is an alias for store.Relationship.
type Relationship = store.Relationship

// EntityRelationship is an alias for store.EntityRelationship.
type EntityRelationship = store.EntityRelationship

// ── Graph facade ──────────────────────────────────────────────────────────────

// UpsertEntity adds or updates an entity in the graph.
func UpsertEntity(ctx context.Context, e Entity) (string, error) {
	return store.UpsertEntity(ctx, e)
}

// UpsertRelationship adds or refreshes an edge in the graph.
func UpsertRelationship(ctx context.Context, r Relationship) (string, error) {
	return store.UpsertRelationship(ctx, r)
}

// DeleteRelationship removes an edge by its UUID.
func DeleteRelationship(ctx context.Context, id string) error {
	return store.DeleteRelationship(ctx, id)
}

// EntityByTicker looks up an entity by stock ticker (case-insensitive).
// Returns nil if not found.
func EntityByTicker(ctx context.Context, ticker string) (*Entity, error) {
	return store.GetEntityByTicker(ctx, ticker)
}

// EntityByID looks up an entity by UUID.
func EntityByID(ctx context.Context, id string) (*Entity, error) {
	return store.GetEntityByID(ctx, id)
}

// ListEntities returns all entities, optionally filtered by type ("" = all).
func ListEntities(ctx context.Context, entityType string) ([]Entity, error) {
	return store.ListEntities(ctx, entityType)
}

// RelatedEntities returns 1-hop relationships for the entity with the given ticker.
// Returns nil, nil if the ticker is not in the graph.
func RelatedEntities(ctx context.Context, ticker string) ([]EntityRelationship, error) {
	entity, err := store.GetEntityByTicker(ctx, ticker)
	if err != nil || entity == nil {
		return nil, err
	}
	return store.GetRelationshipsForEntity(ctx, entity.ID)
}

// RelatedEntities2Hop returns 1- and 2-hop relationships for the given ticker.
// Returns nil, nil if the ticker is not in the graph.
func RelatedEntities2Hop(ctx context.Context, ticker string) ([]EntityRelationship, error) {
	entity, err := store.GetEntityByTicker(ctx, ticker)
	if err != nil || entity == nil {
		return nil, err
	}
	return store.Get2HopRelationships(ctx, entity.ID)
}

// PendingAutoRelationships returns auto-generated edges awaiting human review.
func PendingAutoRelationships(ctx context.Context) ([]EntityRelationship, error) {
	return store.GetPendingAutoRelationships(ctx)
}

// RecordMention links an entity to a news item.
func RecordMention(ctx context.Context, entityID, newsItemID string, relevance *float64) error {
	return store.UpsertEntityMention(ctx, entityID, newsItemID, relevance)
}
