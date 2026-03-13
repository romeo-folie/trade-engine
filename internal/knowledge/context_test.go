package knowledge

import (
	"strings"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// ── GraphContext.IsEmpty ──────────────────────────────────────────────────────

func TestGraphContext_Format_Empty(t *testing.T) {
	gc := GraphContext{IsEmpty: true}
	out := gc.Format()
	if !strings.Contains(out, "No relationship data") {
		t.Errorf("expected placeholder, got %q", out)
	}
}

func TestGraphContext_Format_NilEntity(t *testing.T) {
	gc := GraphContext{IsEmpty: false, Entity: nil}
	out := gc.Format()
	if !strings.Contains(out, "No relationship data") {
		t.Errorf("expected placeholder for nil entity, got %q", out)
	}
}

// ── GraphContext.Format ───────────────────────────────────────────────────────

func TestGraphContext_Format_BasicEntity(t *testing.T) {
	gc := GraphContext{
		Entity: &store.Entity{
			Name:    "Apple Inc.",
			Ticker:  "AAPL",
			Sector:  "Technology",
			Country: "US",
		},
		IsEmpty: false,
	}
	out := gc.Format()
	if !strings.Contains(out, "Apple Inc.") {
		t.Errorf("expected entity name in output, got %q", out)
	}
	if !strings.Contains(out, "AAPL") {
		t.Errorf("expected ticker in output, got %q", out)
	}
}

func TestGraphContext_Format_WithDescription(t *testing.T) {
	gc := GraphContext{
		Entity: &store.Entity{
			Name:        "Apple Inc.",
			Ticker:      "AAPL",
			Sector:      "Technology",
			Country:     "US",
			Description: "Consumer electronics maker",
		},
		IsEmpty: false,
	}
	out := gc.Format()
	if !strings.Contains(out, "Consumer electronics maker") {
		t.Errorf("expected description in output, got %q", out)
	}
}

func TestGraphContext_Format_WithRelationships(t *testing.T) {
	gc := GraphContext{
		Entity: &store.Entity{Name: "Apple Inc.", Ticker: "AAPL", Sector: "Tech", Country: "US"},
		DirectRelationships: []store.EntityRelationship{
			{
				Relationship:  store.Relationship{RelationshipType: "supplies_to"},
				Direction:     "outbound",
				RelatedEntity: store.Entity{Name: "Samsung", Ticker: "SSNLF"},
			},
			{
				Relationship:  store.Relationship{RelationshipType: "competes_with", Description: "mobile OS rivalry"},
				Direction:     "inbound",
				RelatedEntity: store.Entity{Name: "Google", Ticker: "GOOGL"},
			},
		},
		IsEmpty: false,
	}
	out := gc.Format()
	if !strings.Contains(out, "Relationships:") {
		t.Errorf("expected Relationships section, got %q", out)
	}
	if !strings.Contains(out, "Samsung") {
		t.Errorf("expected Samsung in output")
	}
	if !strings.Contains(out, "mobile OS rivalry") {
		t.Errorf("expected relationship description in output")
	}
	// inbound relationship should use ← symbol
	if !strings.Contains(out, "←") {
		t.Errorf("expected ← for inbound relationship")
	}
	// outbound relationship should use → symbol
	if !strings.Contains(out, "→") {
		t.Errorf("expected → for outbound relationship")
	}
}

func TestGraphContext_Format_WithPortfolioOverlap(t *testing.T) {
	gc := GraphContext{
		Entity: &store.Entity{Name: "Apple Inc.", Ticker: "AAPL", Sector: "Tech", Country: "US"},
		PortfolioOverlap: []PortfolioPosition{
			{Ticker: "MSFT", RelationKind: "competes_with", CurrentValue: 5000.00},
		},
		IsEmpty: false,
	}
	out := gc.Format()
	if !strings.Contains(out, "Portfolio overlap") {
		t.Errorf("expected portfolio overlap section, got %q", out)
	}
	if !strings.Contains(out, "MSFT") {
		t.Errorf("expected MSFT in portfolio overlap")
	}
	if !strings.Contains(out, "5000.00") {
		t.Errorf("expected current value in portfolio overlap")
	}
}

// ── relationLabel ─────────────────────────────────────────────────────────────

func TestRelationLabel_KnownTypes(t *testing.T) {
	cases := []struct{ rt, want string }{
		{"supplies_to", "supplies_to"},
		{"competes_with", "competes_with"},
		{"subsidiary_of", "subsidiary_of"},
		{"sector_member", "sector"},
	}
	for _, c := range cases {
		got := relationLabel(c.rt)
		if got != c.want {
			t.Errorf("relationLabel(%q) = %q, want %q", c.rt, got, c.want)
		}
	}
}

func TestRelationLabel_Unknown(t *testing.T) {
	got := relationLabel("some_custom_type")
	if got != "some_custom_type" {
		t.Errorf("unknown type should pass through, got %q", got)
	}
}
