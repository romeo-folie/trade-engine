package scanner

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestCompactScannerTextTruncatesAtUTF8RuneBoundary(t *testing.T) {
	const limit = 282
	// The byte budget after reserving the ellipsis ends inside the euro sign.
	value := strings.Repeat("a", 277) + "€ trailing"

	got := compactScannerText(value, limit)
	if !utf8.ValidString(got) {
		t.Fatalf("compactScannerText returned invalid UTF-8: %q", got)
	}
	if len(got) > limit {
		t.Fatalf("compactScannerText length = %d, want at most %d", len(got), limit)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("compactScannerText = %q, want truncation ellipsis", got)
	}
}

func TestBuildScannerTradeProfileUsesSourceOwnedMenus(t *testing.T) {
	tests := []struct {
		name          string
		candidate     SwingCandidate
		score         SwingScoreResult
		wantLenses    []string
		wantPrimary   string
		wantEntry     string
		wantRecommend string
	}{
		{
			name:       "political disclosure is daily trend momentum",
			candidate:  SwingCandidate{Ticker: "GOV", Sources: []string{"political_disclosure"}},
			score:      SwingScoreResult{Strategy: "weekly_leader"},
			wantLenses: []string{"trend_following", "momentum"}, wantPrimary: "1D",
			wantRecommend: "trend_following",
		},
		{
			name: "weekly leader permits qualified pullback and scorer reorders only within menu",
			candidate: SwingCandidate{
				Ticker: "LEAD", Sources: []string{"mover", "weekly_leader"},
				WeeklyLeaderEvidence: "rank 2, composite score 0.930", PullbackQualified: true,
			},
			score:      SwingScoreResult{Strategy: "momentum", SetupType: "momentum_breakout", Reasoning: "Continuation."},
			wantLenses: []string{"momentum", "trend_following", "pullback_strength"}, wantPrimary: "1D",
			wantRecommend: "momentum",
		},
		{
			name: "premarket uses event timing",
			candidate: SwingCandidate{Ticker: "GAP", Sources: []string{"premarket_episodic"},
				PremarketEpisodicEvidence: "gap +12%; catalyst confirmed"},
			score:      SwingScoreResult{Strategy: "news_sentiment"},
			wantLenses: []string{"news_sentiment", "momentum"}, wantPrimary: "4H", wantEntry: "1H",
			wantRecommend: "news_sentiment",
		},
		{
			name: "news sympathy uses news momentum and excludes source label",
			candidate: SwingCandidate{Ticker: "PEER", Sources: []string{"news_sympathy"},
				GraphContext: "SYMPATHY TRIGGER — seed news."},
			score:      SwingScoreResult{Strategy: "news_sympathy"},
			wantLenses: []string{"news_sentiment", "momentum"}, wantPrimary: "4H",
			wantRecommend: "news_sentiment",
		},
		{
			name:       "ordinary mover uses momentum news",
			candidate:  SwingCandidate{Ticker: "MOVE", Sources: []string{"mover", "active"}},
			score:      SwingScoreResult{Strategy: "mean_reversion"},
			wantLenses: []string{"momentum", "news_sentiment"}, wantPrimary: "4H",
			wantRecommend: "momentum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			discovery, profile, err := buildScannerTradeProfile(tt.candidate, tt.score)
			if err != nil {
				t.Fatalf("buildScannerTradeProfile: %v", err)
			}
			if discovery == nil || profile == nil {
				t.Fatal("profile pair is missing")
			}
			if !reflect.DeepEqual(profile.AdmissibleLenses, tt.wantLenses) ||
				profile.PrimaryTimeframe != tt.wantPrimary ||
				profile.EntryTimingTimeframe != tt.wantEntry ||
				profile.RecommendedLens != tt.wantRecommend {
				t.Fatalf("profile = %+v", profile)
			}
			for _, source := range discovery.Sources {
				if _, ok := discovery.EvidenceBySource[source]; !ok {
					t.Fatalf("missing evidence for %q: %+v", source, discovery)
				}
				for _, lens := range profile.AdmissibleLenses {
					if lens == source {
						t.Fatalf("source %q leaked into lenses %v", source, profile.AdmissibleLenses)
					}
				}
			}
		})
	}
}

func TestBuildScannerTradeProfileCapturesStructuredEvidence(t *testing.T) {
	published := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	candidate := SwingCandidate{
		Ticker: "NVDA", Price: 180, ChangePct: 5.4, Volume: 2_000_000,
		Sources:              []string{"mover", "weekly_leader", "potent_industry", "premarket_episodic"},
		SourceScores:         map[string]float64{"mover": 0.2, "weekly_leader": .91, "potent_industry": .82, "premarket_episodic": .75},
		WeeklyLeaderEvidence: "rank 1", PotentIndustryEvidence: "semiconductors broad strong",
		PremarketEpisodicEvidence: "gap +5%; catalyst confirmed", WeeklyLeaderRunID: "weekly-run",
		PotentIndustryRunID: "industry-run", PotentIndustryGroupType: "industry", PotentIndustryGroupKey: "Semiconductors",
		PremarketEpisodicRunID: "premarket-run",
		News:                   []SwingNewsItem{{Headline: "Demand remains strong", Sentiment: "POSITIVE", PublishedAt: published}},
	}
	discovery, profile, err := buildScannerTradeProfile(candidate, SwingScoreResult{Strategy: "news_sentiment"})
	if err != nil {
		t.Fatalf("buildScannerTradeProfile: %v", err)
	}
	if profile.PrimaryTimeframe != "1D" || profile.RecommendedLens != "trend_following" {
		t.Fatalf("weekly priority must keep daily source-owned profile: %+v", profile)
	}
	var mover, weekly, industry, premarket map[string]any
	if err := json.Unmarshal(discovery.EvidenceBySource["mover"], &mover); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(discovery.EvidenceBySource["weekly_leader"], &weekly); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(discovery.EvidenceBySource["potent_industry"], &industry); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(discovery.EvidenceBySource["premarket_episodic"], &premarket); err != nil {
		t.Fatal(err)
	}
	if mover["market_observation"] == nil || mover["recent_news"] == nil || weekly["leader_evidence"] != "rank 1" ||
		industry["industry_evidence"] != "semiconductors broad strong" || premarket["episodic_evidence"] != "gap +5%; catalyst confirmed" {
		t.Fatalf("structured evidence = mover:%+v weekly:%+v industry:%+v premarket:%+v", mover, weekly, industry, premarket)
	}
}

func TestScanRecorderFallbackReplacesProfilePair(t *testing.T) {
	recorder := &scanRecorder{candidates: make(map[string]*store.ScannerCandidateSnapshot)}
	original := SwingCandidate{Ticker: "AAA", Sources: []string{"mover", "weekly_leader"}, WeeklyLeaderEvidence: "rank 3"}
	fallback := SwingCandidate{Ticker: "AAA", Sources: []string{"mover"}, News: []SwingNewsItem{{Headline: "Fresh catalyst"}}}
	recorder.register([]SwingCandidate{original})
	originalDiscovery, originalProfile, err := buildScannerTradeProfile(original, SwingScoreResult{Strategy: "trend_following"})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.setTradeProfile(original, originalDiscovery, originalProfile); err != nil {
		t.Fatal(err)
	}
	recorder.fallbackScored(fallback, SwingScoreResult{Action: "add", Strategy: "news_sentiment"}, nil, .55)
	fallbackDiscovery, fallbackProfile, err := buildScannerTradeProfile(fallback, SwingScoreResult{Strategy: "news_sentiment"})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.setTradeProfile(fallback, fallbackDiscovery, fallbackProfile); err != nil {
		t.Fatal(err)
	}
	snapshot := recorder.candidates["AAA"]
	if !reflect.DeepEqual(snapshot.Sources, []string{"mover"}) ||
		snapshot.DiscoveryContext == nil ||
		reflect.DeepEqual(snapshot.DiscoveryContext.Sources, []string{"mover", "weekly_leader"}) ||
		snapshot.ExecutionProfile == nil || snapshot.ExecutionProfile.RecommendedLens != "news_sentiment" {
		t.Fatalf("fallback did not replace the profile pair: %+v", snapshot)
	}
}
