package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestEligibleNeighbors_And_Mentions exercises the scanner's graph-sourcing
// substrate: relationship-mention corroboration counting (idempotent),
// confidence/corroboration/strength eligibility gating, edge-type filtering
// (company + non-empty ticker only), direction reporting, and the
// ConfirmRelationship promotion path.
//
// Starts from a clean graph slate so counts and membership are deterministic.
func TestEligibleNeighbors_And_Mentions(t *testing.T) {
	ctx := context.Background()
	db := store.GetDB()

	if _, err := db.Exec(ctx,
		"TRUNCATE relationship_mentions, entity_mentions, relationships, entities CASCADE"); err != nil {
		t.Fatalf("truncate graph tables: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(ctx,
			"TRUNCATE relationship_mentions, entity_mentions, relationships, entities CASCADE")
	})

	// ── Entities ────────────────────────────────────────────────────────────
	seed := mustEntity(t, ctx, "GN Seed Corp", "company", "GN_SEED")
	trust := mustEntity(t, ctx, "GN Trusted Corp", "company", "GN_TRUST")    // seed/confirmed edge
	autoOK := mustEntity(t, ctx, "GN AutoOK Corp", "company", "GN_AUTOOK")   // auto, passes bar
	lowConf := mustEntity(t, ctx, "GN LowConf Corp", "company", "GN_LOWCNF") // auto, conf too low
	uncorr := mustEntity(t, ctx, "GN Uncorr Corp", "company", "GN_UNCORR")   // auto, too few articles
	weak := mustEntity(t, ctx, "GN Weak Corp", "company", "GN_WEAK")         // trusted but weak edge
	inbound := mustEntity(t, ctx, "GN Inbound Corp", "company", "GN_INB")    // inbound edge
	idx := mustEntity(t, ctx, "GN Index", "index", "GN_IDX")                 // non-company
	noTicker := mustEntity(t, ctx, "GN NoTicker Corp", "company", "")        // company, no ticker

	// ── Edges from/to the seed ──────────────────────────────────────────────
	// Trusted (auto_generated=false): eligible regardless of confidence/mentions.
	mustRel(t, ctx, seed, trust, "supplies_to", 0.9, false, nil)
	// Auto, confidence 0.85, strength 0.7 — needs >= minCorroboration mentions.
	relAutoOK := mustRel(t, ctx, seed, autoOK, "competes_with", 0.7, true, ptr(0.85))
	// Auto, confidence 0.60 — below minConfidence; excluded even if corroborated.
	relLowConf := mustRel(t, ctx, seed, lowConf, "partner_of", 0.7, true, ptr(0.60))
	// Auto, confidence 0.90 — high, but only one article corroborates it.
	relUncorr := mustRel(t, ctx, seed, uncorr, "invests_in", 0.7, true, ptr(0.90))
	// Trusted but strength below minEdgeStrength.
	mustRel(t, ctx, seed, weak, "sector_member", 0.4, false, nil)
	// Inbound: edge points neighbour → seed.
	mustRel(t, ctx, inbound, seed, "supplies_to", 0.85, false, nil)
	// Non-company neighbour (index) — excluded by entity_type filter.
	mustRel(t, ctx, seed, idx, "sector_member", 0.9, false, nil)
	// Company neighbour without a ticker — excluded by ticker filter.
	mustRel(t, ctx, seed, noTicker, "partner_of", 0.9, false, nil)

	// ── Corroborating mentions ──────────────────────────────────────────────
	n1 := mustNews(t, ctx, "gn-news-1")
	n2 := mustNews(t, ctx, "gn-news-2")
	// autoOK corroborated by 2 distinct articles → passes corroboration.
	mustMention(t, ctx, relAutoOK, n1)
	mustMention(t, ctx, relAutoOK, n2)
	// Idempotency: re-recording the same (rel, news) must not inflate the count.
	mustMention(t, ctx, relAutoOK, n2)
	// lowConf corroborated heavily, but confidence still gates it out.
	mustMention(t, ctx, relLowConf, n1)
	mustMention(t, ctx, relLowConf, n2)
	// uncorr corroborated by only one article.
	mustMention(t, ctx, relUncorr, n1)

	const (
		minConf     = 0.80
		minCorr     = 2
		minStrength = 0.6
	)

	got, err := store.EligibleNeighbors(ctx, "GN_SEED", minConf, minCorr, minStrength)
	if err != nil {
		t.Fatalf("EligibleNeighbors: %v", err)
	}

	byTicker := make(map[string]store.NeighborCandidate, len(got))
	for _, n := range got {
		byTicker[n.Ticker] = n
		if n.SeedTicker != "GN_SEED" {
			t.Errorf("%s: SeedTicker = %q, want GN_SEED", n.Ticker, n.SeedTicker)
		}
	}

	wantPresent := []string{"GN_TRUST", "GN_AUTOOK", "GN_INB"}
	for _, tk := range wantPresent {
		if _, ok := byTicker[tk]; !ok {
			t.Errorf("expected %s to be eligible, missing from %v", tk, keys(byTicker))
		}
	}
	wantAbsent := map[string]string{
		"GN_LOWCNF": "confidence below minConfidence",
		"GN_UNCORR": "fewer than minCorroboration mentions",
		"GN_WEAK":   "strength below minEdgeStrength",
		"GN_IDX":    "non-company entity_type",
		"":          "neighbour has no ticker",
	}
	for tk, why := range wantAbsent {
		if _, ok := byTicker[tk]; ok {
			t.Errorf("expected %q absent (%s), but it was eligible", tk, why)
		}
	}

	// Direction reporting.
	if n, ok := byTicker["GN_AUTOOK"]; ok && n.Direction != "outbound" {
		t.Errorf("GN_AUTOOK direction = %q, want outbound", n.Direction)
	}
	if n, ok := byTicker["GN_INB"]; ok && n.Direction != "inbound" {
		t.Errorf("GN_INB direction = %q, want inbound", n.Direction)
	}
	// MentionCount surfaced and not double-counted.
	if n, ok := byTicker["GN_AUTOOK"]; ok && n.MentionCount != 2 {
		t.Errorf("GN_AUTOOK MentionCount = %d, want 2 (idempotent)", n.MentionCount)
	}

	// ── ConfirmRelationship promotes an otherwise-excluded auto edge ─────────
	if err := store.ConfirmRelationship(ctx, relUncorr); err != nil {
		t.Fatalf("ConfirmRelationship: %v", err)
	}
	got2, err := store.EligibleNeighbors(ctx, "GN_SEED", minConf, minCorr, minStrength)
	if err != nil {
		t.Fatalf("EligibleNeighbors (post-confirm): %v", err)
	}
	if !contains(got2, "GN_UNCORR") {
		t.Errorf("GN_UNCORR should be eligible after ConfirmRelationship (auto_generated cleared)")
	}

	// A subsequent auto re-extraction of the now-confirmed edge must NOT demote it
	// back to unconfirmed (auto_generated stays sticky-FALSE via the ON CONFLICT AND).
	if _, err := store.UpsertRelationship(ctx, store.Relationship{
		FromEntityID: seed, ToEntityID: uncorr, RelationshipType: "invests_in",
		Strength: 0.7, AutoGenerated: true, Confidence: ptr(0.90),
	}); err != nil {
		t.Fatalf("re-extract confirmed edge: %v", err)
	}
	got3, err := store.EligibleNeighbors(ctx, "GN_SEED", minConf, minCorr, minStrength)
	if err != nil {
		t.Fatalf("EligibleNeighbors (post-reextract): %v", err)
	}
	if !contains(got3, "GN_UNCORR") {
		t.Errorf("GN_UNCORR must stay eligible after auto re-extraction (confirm must not be undone)")
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func mustEntity(t *testing.T, ctx context.Context, name, typ, ticker string) string {
	t.Helper()
	id, err := store.UpsertEntity(ctx, store.Entity{EntityType: typ, Name: name, Ticker: ticker})
	if err != nil {
		t.Fatalf("UpsertEntity(%s): %v", name, err)
	}
	return id
}

func mustRel(t *testing.T, ctx context.Context, from, to, typ string, strength float64, auto bool, conf *float64) string {
	t.Helper()
	id, err := store.UpsertRelationship(ctx, store.Relationship{
		FromEntityID: from, ToEntityID: to, RelationshipType: typ,
		Strength: strength, AutoGenerated: auto, Confidence: conf,
	})
	if err != nil {
		t.Fatalf("UpsertRelationship(%s): %v", typ, err)
	}
	return id
}

func mustNews(t *testing.T, ctx context.Context, extID string) string {
	t.Helper()
	extID = extID + "-" + time.Now().Format("150405.000")
	if _, err := store.UpsertNewsItems(ctx, []data.NewsItem{{
		ExternalID:  extID,
		Headline:    "graph neighbor test news",
		URL:         "https://example.com/" + extID,
		Source:      "test",
		PublishedAt: time.Now(),
	}}); err != nil {
		t.Fatalf("UpsertNewsItems: %v", err)
	}
	var id string
	if err := store.GetDB().QueryRow(ctx,
		`SELECT id FROM news_items WHERE external_id=$1`, extID).Scan(&id); err != nil {
		t.Fatalf("news ID lookup: %v", err)
	}
	return id
}

func mustMention(t *testing.T, ctx context.Context, relID, newsID string) {
	t.Helper()
	if err := store.RecordRelationshipMention(ctx, relID, newsID); err != nil {
		t.Fatalf("RecordRelationshipMention: %v", err)
	}
}

func ptr(f float64) *float64 { return &f }

func keys(m map[string]store.NeighborCandidate) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func contains(ns []store.NeighborCandidate, ticker string) bool {
	for _, n := range ns {
		if n.Ticker == ticker {
			return true
		}
	}
	return false
}
