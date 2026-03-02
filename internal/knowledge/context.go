package knowledge

import (
	"context"
	"fmt"
	"strings"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// GraphContext is the structured knowledge-graph block injected into analysis prompts.
type GraphContext struct {
	// Entity is the primary entity for the ticker being analysed.
	// Nil if the ticker has no entry in the knowledge graph.
	Entity *store.Entity

	// DirectRelationships are the 1-hop edges from/to the entity, enriched with
	// the related entity's metadata.
	DirectRelationships []store.EntityRelationship

	// PortfolioOverlap lists tickers of related entities that have open positions
	// in the portfolio, with their approximate current value.
	PortfolioOverlap []PortfolioPosition

	// IsEmpty reports whether the context holds any meaningful data.
	IsEmpty bool
}

// PortfolioPosition describes an open position in a related ticker.
type PortfolioPosition struct {
	Ticker       string
	RelationKind string // e.g. "competes_with", "supplies_to"
	CurrentValue float64
}

// BuildContext constructs a GraphContext for the given ticker.
//
// openPositions is a map of ticker → current position value (USD); pass nil if
// portfolio data isn't available.  The function never returns an error for a
// missing ticker — it returns an empty GraphContext instead.
func BuildContext(ctx context.Context, ticker string, openPositions map[string]float64) (GraphContext, error) {
	entity, err := store.GetEntityByTicker(ctx, ticker)
	if err != nil {
		return GraphContext{IsEmpty: true}, fmt.Errorf("knowledge BuildContext: %w", err)
	}
	if entity == nil {
		return GraphContext{IsEmpty: true}, nil
	}

	rels, err := store.GetRelationshipsForEntity(ctx, entity.ID)
	if err != nil {
		return GraphContext{IsEmpty: true}, fmt.Errorf("knowledge BuildContext relationships: %w", err)
	}

	// Build portfolio overlap list.
	var overlap []PortfolioPosition
	if len(openPositions) > 0 {
		for _, r := range rels {
			relTicker := r.RelatedEntity.Ticker
			if relTicker == "" {
				continue
			}
			if val, ok := openPositions[relTicker]; ok && val != 0 {
				overlap = append(overlap, PortfolioPosition{
					Ticker:       relTicker,
					RelationKind: r.RelationshipType,
					CurrentValue: val,
				})
			}
		}
	}

	return GraphContext{
		Entity:              entity,
		DirectRelationships: rels,
		PortfolioOverlap:    overlap,
		IsEmpty:             len(rels) == 0,
	}, nil
}

// Format renders the GraphContext as a markdown-ish text block suitable for
// injection into an LLM prompt.  Returns a placeholder string when IsEmpty.
func (gc GraphContext) Format() string {
	if gc.IsEmpty || gc.Entity == nil {
		return "No relationship data available.\n"
	}

	var b strings.Builder
	e := gc.Entity
	b.WriteString(fmt.Sprintf("**%s** (%s | %s | %s)\n",
		e.Name, e.Ticker, e.Sector, e.Country))
	if e.Description != "" {
		b.WriteString(fmt.Sprintf("%s\n", e.Description))
	}

	if len(gc.DirectRelationships) > 0 {
		b.WriteString("\nRelationships:\n")
		for _, r := range gc.DirectRelationships {
			re := r.RelatedEntity
			dirSymbol := "→"
			if r.Direction == "inbound" {
				dirSymbol = "←"
			}
			line := fmt.Sprintf("  %s %s %s (%s)",
				relationLabel(r.RelationshipType), dirSymbol, re.Name, re.Ticker)
			if r.Description != "" {
				line += ": " + r.Description
			}
			b.WriteString(line + "\n")
		}
	}

	if len(gc.PortfolioOverlap) > 0 {
		b.WriteString("\nPortfolio overlap (related open positions):\n")
		for _, p := range gc.PortfolioOverlap {
			b.WriteString(fmt.Sprintf("  %s [%s] — current value $%.2f\n",
				p.Ticker, p.RelationKind, p.CurrentValue))
		}
	}

	return b.String()
}

// relationLabel returns a human-readable label for a DB relationship type.
func relationLabel(rt string) string {
	labels := map[string]string{
		"supplies_to":        "supplies_to",
		"supplied_by":        "supplied_by",
		"competes_with":      "competes_with",
		"subsidiary_of":      "subsidiary_of",
		"parent_of":          "parent_of",
		"partner_of":         "partner_of",
		"sector_member":      "sector",
		"manufactures_for":   "manufactures_for",
		"manufactured_by":    "manufactured_by",
		"regulates":          "regulates",
		"regulated_by":       "regulated_by",
		"affects_contract":   "affects_contract",
		"affected_by_event":  "affected_by_event",
	}
	if l, ok := labels[rt]; ok {
		return l
	}
	return rt
}
