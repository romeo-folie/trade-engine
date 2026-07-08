package scanner

import (
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// pickSympathyTrigger: sentiment floor filters, strongest |sentiment| wins,
// freshest wins ties, and no qualifying article returns false.
func TestPickSympathyTrigger(t *testing.T) {
	now := time.Date(2026, 7, 8, 15, 0, 0, 0, time.UTC)
	news := []data.NewsItem{
		{Headline: "weak", SentimentScore: 0.10, PublishedAt: now.Add(-1 * time.Hour)},
		{Headline: "strong-neg", SentimentScore: -0.80, PublishedAt: now.Add(-3 * time.Hour)},
		{Headline: "mid-pos", SentimentScore: 0.50, PublishedAt: now.Add(-30 * time.Minute)},
	}
	trig, ok := pickSympathyTrigger("NVDA", news, now, 0.35)
	if !ok {
		t.Fatal("expected a trigger")
	}
	if trig.Headline != "strong-neg" || trig.Sentiment != -0.80 {
		t.Errorf("strongest |sentiment| must win: %+v", trig)
	}
	if trig.AgeHours < 2.9 || trig.AgeHours > 3.1 {
		t.Errorf("AgeHours = %v, want ~3", trig.AgeHours)
	}

	// Tie on |sentiment| → freshest wins.
	tie := []data.NewsItem{
		{Headline: "older", SentimentScore: 0.6, PublishedAt: now.Add(-4 * time.Hour)},
		{Headline: "newer", SentimentScore: -0.6, PublishedAt: now.Add(-1 * time.Hour)},
	}
	trig, _ = pickSympathyTrigger("NVDA", tie, now, 0.35)
	if trig.Headline != "newer" {
		t.Errorf("freshest must win ties: %+v", trig)
	}

	if _, ok := pickSympathyTrigger("NVDA", []data.NewsItem{{SentimentScore: 0.2, PublishedAt: now}}, now, 0.35); ok {
		t.Error("sub-floor sentiment must not trigger")
	}
}

// sympathyPhrase renders trigger + relationship, labels sentiment, and
// truncates very long headlines.
func TestSympathyPhrase(t *testing.T) {
	n := store.NeighborCandidate{SeedTicker: "NVDA", RelationshipType: "supplies_to", Direction: "outbound", Ticker: "SMCI"}
	got := sympathyPhrase(sympathyTrigger{
		Seed: "NVDA", Headline: "Nvidia beats on data-center revenue", Sentiment: 0.82, AgeHours: 2.2,
	}, n)
	for _, want := range []string{
		"SYMPATHY TRIGGER — NVDA news 2h ago",
		"POSITIVE 0.82",
		`"Nvidia beats on data-center revenue"`,
		"NVDA (on watchlist) supplies to this company",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("phrase missing %q\ngot: %s", want, got)
		}
	}

	long := sympathyPhrase(sympathyTrigger{Seed: "NVDA", Headline: strings.Repeat("x", 300), Sentiment: -0.5}, n)
	if !strings.Contains(long, "NEGATIVE") || !strings.Contains(long, "…") {
		t.Errorf("long negative phrase wrong: %s", long)
	}
}

// Cooldown: fired pairs are suppressed until pruned after the window elapses.
func TestSympathyCooldown(t *testing.T) {
	s := &MarketScanner{}
	now := time.Date(2026, 7, 8, 15, 0, 0, 0, time.UTC)

	if s.sympathyOnCooldown("NVDA", "SMCI") {
		t.Error("fresh pair must not start on cooldown")
	}
	s.markSympathyFired("NVDA", "SMCI", now)
	if !s.sympathyOnCooldown("nvda", "smci") {
		t.Error("cooldown must be case-insensitive")
	}

	// Not yet elapsed → survives pruning.
	s.pruneSympathyCooldowns(now.Add(23*time.Hour), 24*time.Hour)
	if !s.sympathyOnCooldown("NVDA", "SMCI") {
		t.Error("pair pruned before cooldown elapsed")
	}
	// Elapsed → pruned.
	s.pruneSympathyCooldowns(now.Add(25*time.Hour), 24*time.Hour)
	if s.sympathyOnCooldown("NVDA", "SMCI") {
		t.Error("pair must be prunable after cooldown")
	}
}

// The swing prompt must render the sympathy trigger and carry the spillover
// reasoning instructions (direction / magnitude / timing).
func TestBuildSwingPrompt_SympathyInstructions(t *testing.T) {
	cand := SwingCandidate{
		Ticker: "SMCI", Price: 42.0, Sources: []string{"news_sympathy"},
		GraphContext: `SYMPATHY TRIGGER — NVDA news 2h ago (POSITIVE 0.82): "Nvidia beats". Relationship: NVDA (on watchlist) supplies to this company.`,
	}
	sys, user := BuildSwingPrompt(cand, 0.65)
	if !strings.Contains(user, "SYMPATHY TRIGGER — NVDA news 2h ago") {
		t.Errorf("user message missing sympathy trigger:\n%s", user)
	}
	if !strings.Contains(sys, `begins with "SYMPATHY TRIGGER"`) {
		t.Error("system prompt missing the sympathy instruction block")
	}
	for _, want := range []string{"direction", "magnitude", "timing"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing spillover dimension %q", want)
		}
	}
}
