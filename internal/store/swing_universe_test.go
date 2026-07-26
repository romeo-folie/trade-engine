package store

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTaxonomyStatusAt(t *testing.T) {
	asOf := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	effectiveAt := asOf.Add(-30 * 24 * time.Hour)

	if got := taxonomyStatusAt("known", effectiveAt, asOf, 31*24*time.Hour); got != "known" {
		t.Fatalf("fresh status = %q, want known", got)
	}
	if got := taxonomyStatusAt("known", effectiveAt, asOf, 29*24*time.Hour); got != "stale" {
		t.Fatalf("old status = %q, want stale", got)
	}
	if got := taxonomyStatusAt("unavailable", effectiveAt, asOf, time.Hour); got != "unavailable" {
		t.Fatalf("unavailable status = %q", got)
	}
	if got := taxonomyStatusAt("known", effectiveAt, asOf, 0); got != "known" {
		t.Fatalf("disabled max age status = %q", got)
	}
}

func TestNormalizeTaxonomyMakesMissingEvidenceExplicit(t *testing.T) {
	sector, industry, status := normalizeTaxonomy("", "", "known")
	if sector != UnknownTaxonomy || industry != UnknownTaxonomy || status != "unknown" {
		t.Fatalf("missing known taxonomy = %q/%q/%q", sector, industry, status)
	}

	sector, industry, status = normalizeTaxonomy("Technology", "Software", "unavailable")
	if sector != UnknownTaxonomy || industry != UnknownTaxonomy || status != "unavailable" {
		t.Fatalf("unavailable taxonomy = %q/%q/%q", sector, industry, status)
	}

	sector, industry, status = normalizeTaxonomy("Technology", "Software", "stale")
	if sector != "Technology" || industry != "Software" || status != "stale" {
		t.Fatalf("stale taxonomy = %q/%q/%q", sector, industry, status)
	}

	sector, industry, status = normalizeTaxonomy("", "Semiconductors", "known")
	if sector != UnknownTaxonomy || industry != "Semiconductors" || status != "known" {
		t.Fatalf("industry-only taxonomy = %q/%q/%q", sector, industry, status)
	}
}

func TestNonNilJSONPreservesExplicitEvidence(t *testing.T) {
	if got := string(nonNilJSON(nil)); got != "{}" {
		t.Fatalf("nil JSON = %q", got)
	}
	evidence := json.RawMessage(`{"score":0}`)
	if got := string(nonNilJSON(evidence)); got != string(evidence) {
		t.Fatalf("evidence = %q", got)
	}
}
