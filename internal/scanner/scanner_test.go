package scanner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

type capturedScannerEvents struct {
	payloads []events.ScannerPromotionOutcomePayload
}

func (b *capturedScannerEvents) Publish(evType events.Type, payload any) {
	if evType != events.ScannerPromotionOutcome {
		return
	}
	if outcome, ok := payload.(events.ScannerPromotionOutcomePayload); ok {
		b.payloads = append(b.payloads, outcome)
	}
}

func TestCandidateTTLDaysUsesSourcePolicy(t *testing.T) {
	scanner := &MarketScanner{Cfg: config.ScannerConfig{
		TTLDays:        3,
		WeeklyLeader:   config.WeeklyLeaderConfig{TTLDays: 10},
		PotentIndustry: config.PotentIndustryConfig{TTLDays: 5},
	}}
	if got := scanner.candidateTTLDays(SwingCandidate{Sources: []string{"mover"}}); got != 3 {
		t.Fatalf("ordinary TTL = %d, want 3", got)
	}
	if got := scanner.candidateTTLDays(SwingCandidate{Sources: []string{"weekly_leader"}}); got != 10 {
		t.Fatalf("weekly TTL = %d, want 10", got)
	}
	if got := scanner.candidateTTLDays(SwingCandidate{Sources: []string{"potent_industry"}}); got != 5 {
		t.Fatalf("industry TTL = %d, want 5", got)
	}
	if got := scanner.candidateTTLDays(SwingCandidate{Sources: []string{"weekly_leader", "potent_industry"}}); got != 10 {
		t.Fatalf("multi-source TTL = %d, want longest source TTL 10", got)
	}
}

func TestFetchCandidatesKeepsMoverWithPartialIEXVolume(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1beta1/screener/stocks/movers":
			_, _ = w.Write([]byte(`{"gainers":[{"symbol":"LIQUID","percent_change":8,"change":1,"price":20},{"symbol":"IEX_RICH","percent_change":5,"change":1,"price":20}],"losers":[]}`))
		case "/v2/stocks/snapshots":
			// IEX's partial volume must not control the top-N cutoff. A lower-gap
			// name with heavy IEX activity cannot displace LIQUID's larger gap.
			_, _ = w.Write([]byte(`{"LIQUID":{"dailyBar":{"v":1000}},"IEX_RICH":{"dailyBar":{"v":1000000}}}`))
		case "/v1beta1/screener/stocks/most-actives":
			_, _ = w.Write([]byte(`{"most_actives":[]}`))
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	scanner := &MarketScanner{
		AlpacaClient: data.NewAlpacaClient("key", "secret", server.URL),
		Cfg: config.ScannerConfig{Universe: config.UniverseConfig{
			MinPrice: 5, MinMedianDollarVolume: 1_000_000,
		}, TopMovers: 1},
	}
	candidates, err := scanner.fetchCandidates(context.Background())
	if err != nil {
		t.Fatalf("fetch candidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Ticker != "LIQUID" {
		t.Fatalf("partial-IEX mover was filtered: %+v", candidates)
	}
	if candidates[0].DollarVolume != 0 || candidates[0].Volume == 0 {
		t.Fatalf("IEX volume leaked into queue liquidity: %+v", candidates[0])
	}
}

func TestFetchCandidatesKeepsIntradayActiveBelowCompletedSessionFloor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1beta1/screener/stocks/movers":
			_, _ = w.Write([]byte(`{"gainers":[],"losers":[]}`))
		case "/v1beta1/screener/stocks/most-actives":
			// Both current-session notionals are below the completed-session
			// median floor, but they are still valid most-active observations.
			_, _ = w.Write([]byte(`{"most_actives":[{"symbol":"SKIP","price":4,"volume":10},{"symbol":"EARLY","price":10,"volume":10,"trade_count":2},{"symbol":"LATER","price":20,"volume":20,"trade_count":3}]}`))
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()
	scanner := &MarketScanner{
		AlpacaClient: data.NewAlpacaClient("key", "secret", server.URL),
		Cfg: config.ScannerConfig{Universe: config.UniverseConfig{
			MinPrice: 5, MinMedianDollarVolume: 1_000_000,
		}, TopActives: 2},
	}
	candidates, err := scanner.fetchCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].Ticker != "EARLY" || candidates[1].Ticker != "LATER" {
		t.Fatalf("intraday active candidates were filtered: %+v", candidates)
	}
	for _, candidate := range candidates {
		if candidate.DollarVolume != 0 || candidate.SourceScores["active"] <= 0 {
			t.Fatalf("active candidate leaked intraday liquidity into queue fields: %+v", candidate)
		}
	}
	if candidates[0].SourceScores["active"] != 1 || candidates[0].SourceScores["active"] <= candidates[1].SourceScores["active"] {
		t.Fatalf("provider ordinal scores = %+v, want rank one above rank two", candidates)
	}
}

func TestScannerCandidateStrategiesUsesSourceDefaultWithoutLLMScore(t *testing.T) {
	for _, test := range []struct {
		sources []string
		want    string
	}{
		{sources: []string{"mover"}, want: "momentum"},
		{sources: []string{"graph_neighbor"}, want: "news_sentiment"},
		{sources: []string{"weekly_leader"}, want: "trend_following"},
	} {
		strategies := scannerCandidateStrategies(SwingCandidate{Sources: test.sources}, SwingScoreResult{})
		if len(strategies) == 0 || strategies[0] != test.want {
			t.Fatalf("sources %v strategies=%v, want primary %q", test.sources, strategies, test.want)
		}
	}
}

func TestParseSwingResult_AcceptsLowercaseAdd(t *testing.T) {
	raw := `{"action":"add","conviction":0.72,"setup_type":"news_catalyst","strategy":"news_sentiment","timeframes":["1D"],"reasoning":"earnings beat"}`
	got := parseSwingResult(raw, analysis.AnalysisResponse{})
	if got.Action != "add" || got.Conviction != 0.72 || got.SetupType != "news_catalyst" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestParseSwingResult_AcceptsMarkdownFencedAdd(t *testing.T) {
	raw := "```json\n{\"action\":\"add\",\"conviction\":0.65,\"setup_type\":\"news_catalyst\",\"strategy\":\"news_sentiment\",\"timeframes\":[\"1D\"],\"reasoning\":\"x\"}\n```"
	got := parseSwingResult(raw, analysis.AnalysisResponse{})
	if got.Action != "add" || got.Conviction != 0.65 {
		t.Fatalf("fenced add not parsed: %+v", got)
	}
}

// Some models return "ACTION": "ADD" in uppercase. The generic extractor
// normalizes that to ActionAdd and accepts it; this parser must too or the
// candidate is silently downgraded to skip.
func TestParseSwingResult_AcceptsUppercaseAddAction(t *testing.T) {
	raw := `{"action":"ADD","conviction":0.70,"setup_type":"trend","strategy":"news_sentiment","timeframes":["1D"],"reasoning":"strong momentum"}`
	got := parseSwingResult(raw, analysis.AnalysisResponse{})
	if got.Action != "add" {
		t.Fatalf("uppercase ADD not normalized: action=%q", got.Action)
	}
	if got.Conviction != 0.70 {
		t.Fatalf("conviction lost on uppercase parse: %v", got.Conviction)
	}
}

// Some responses wrap the JSON in prose ("Here is my analysis: { ... }").
// The generic extractor narrows to the outermost braces; this parser must too.
func TestParseSwingResult_NarrowsToOutermostBraces(t *testing.T) {
	raw := `Here is my analysis: {"action":"add","conviction":0.80,"setup_type":"breakout","strategy":"momentum","timeframes":["1D"],"reasoning":"gap and go"} — end of response.`
	got := parseSwingResult(raw, analysis.AnalysisResponse{})
	if got.Action != "add" || got.Conviction != 0.80 {
		t.Fatalf("prose-wrapped JSON not parsed: %+v", got)
	}
}

// When the local parse fails but the generic extractor already normalized
// Action to ADD, the fallback must preserve that action rather than hardcode
// a silent skip. Conviction inevitably becomes 0 here (AnalysisResponse has
// no "conviction" field), but preserving the action at least stops the
// wholesale loss when the fallback path is hit.
func TestParseSwingResult_FallbackPreservesAddFromExtractor(t *testing.T) {
	got := parseSwingResult("not json at all", analysis.AnalysisResponse{
		Action:    analysis.ActionAdd,
		Reasoning: "r",
	})
	if got.Action != "add" {
		t.Fatalf("fallback dropped ADD to %q", got.Action)
	}
}

func TestParseSwingResult_FallbackDefaultsToSkip(t *testing.T) {
	got := parseSwingResult("not json at all", analysis.AnalysisResponse{
		Action: analysis.ActionHold, // anything not ADD
	})
	if got.Action != "skip" {
		t.Fatalf("fallback should default to skip, got %q", got.Action)
	}
}

func TestExpiredScannerResidencyAllowsReadmissionBesideAdditiveContext(t *testing.T) {
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Minute)
	item := store.WatchlistItem{
		Ticker: "REENTER", ScannerOrigin: true, ExpiresAt: &expired,
		DiscoveryContexts: []store.WatchlistDiscoveryContext{{Key: "earnings_scheduled:2026-08-24"}},
	}
	if !expiredScannerResidency(item, now) {
		t.Fatal("expired scanner origin with a live additive context was not recognized for readmission")
	}
	existing := map[string]struct{}{}
	if !expiredScannerResidency(item, now) {
		existing[normalizePromotionKey(item.Ticker)] = struct{}{}
	}
	if got := filterActiveCandidates([]SwingCandidate{{Ticker: item.Ticker}}, existing); len(got) != 1 {
		t.Fatalf("expired scanner origin was still excluded from normal admission: %+v", got)
	}
	earningsTakeover := store.WatchlistItem{Ticker: "REENTER_EARNINGS", Source: "earnings_watchlist", ScannerOrigin: true}
	if !expiredScannerResidency(earningsTakeover, now) {
		t.Fatal("scanner-origin earnings takeover with cleared expiry was not recognized for readmission")
	}
}

func TestScannerDiscoveryNewsRestoresPersistedNews(t *testing.T) {
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	discovery := &tradeprofile.DiscoveryContext{
		Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"mover"}, Thesis: "mover news",
		EvidenceBySource: map[string]json.RawMessage{
			"mover": json.RawMessage(`{"recent_news":[{"Headline":"Prior catalyst","Sentiment":"POSITIVE","PublishedAt":"2026-08-21T11:00:00Z"}]}`),
		},
	}
	news := scannerDiscoveryNews(discovery)
	if len(news) != 1 || news[0].Headline != "Prior catalyst" || !news[0].PublishedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("persisted news = %#v", news)
	}
	candidate := SwingCandidate{Ticker: "NEWS", Sources: []string{"mover"}}
	restoreActiveRefreshNews(&candidate, store.WatchlistItem{DiscoveryContext: discovery})
	if len(candidate.News) != 1 || candidate.News[0].Headline != "Prior catalyst" {
		t.Fatalf("provider-disabled refresh lost persisted news: %#v", candidate.News)
	}
}

func TestPublishRemovedPromotionOutcomesReportsEachSelectedDuplicate(t *testing.T) {
	bus := &capturedScannerEvents{}
	scanner := &MarketScanner{Bus: bus}
	before := []SwingCandidate{
		{Ticker: "HELD", Sources: []string{"mover"}, promotionSourceRanks: map[string]int{"mover": 1}},
		{Ticker: "KEEP", Sources: []string{"active"}, promotionSourceRanks: map[string]int{"active": 1}},
	}
	scanner.publishRemovedPromotionOutcomes(
		"scan-1", before, before[1:], "open_position", "ticker already has an open position",
	)
	if len(bus.payloads) != 1 {
		t.Fatalf("outcomes=%+v, want one duplicate outcome", bus.payloads)
	}
	outcome := bus.payloads[0]
	if outcome.Ticker != "HELD" || outcome.Outcome != "not_admitted" ||
		outcome.ReasonCode != "open_position" || len(outcome.Sources) != 1 ||
		outcome.Sources[0].Rank != 1 {
		t.Fatalf("duplicate outcome=%+v", outcome)
	}
}

func TestPublishPromotionClaimLossOutcomeIsSourceScoped(t *testing.T) {
	bus := &capturedScannerEvents{}
	scanner := &MarketScanner{Bus: bus}
	candidate := SwingCandidate{
		Ticker: "MERGED", Sources: []string{"mover", "weekly_leader"},
		SourceScores:         map[string]float64{"mover": .9, "weekly_leader": .8},
		promotionSourceRanks: map[string]int{"mover": 1, "weekly_leader": 2},
	}
	scanner.publishPromotionClaimLossOutcome("scan-1", candidate, "weekly_leader", store.ErrScannerSourcePromotionClaimLost)
	if len(bus.payloads) != 1 {
		t.Fatalf("outcomes=%+v, want one claim-loss outcome", bus.payloads)
	}
	outcome := bus.payloads[0]
	if outcome.ReasonCode != "source_claim_lost" || len(outcome.Sources) != 1 ||
		outcome.Sources[0].Source != "weekly_leader" || outcome.Sources[0].Rank != 2 {
		t.Fatalf("claim-loss outcome=%+v", outcome)
	}
}

func TestPromotionClaimLossFallbackRequiresRemainingSelectedSource(t *testing.T) {
	ordinary := SwingCandidate{Ticker: "MERGED", Sources: []string{"mover"}}
	candidate := SwingCandidate{
		Ticker:                 "MERGED",
		Sources:                []string{"mover", "weekly_leader"},
		WeeklyLeaderRunID:      "weekly-run",
		WeeklyLeaderClaimToken: "weekly-token",
		weeklyLeaderFallback:   &ordinary,
		promotionSourceRanks:   map[string]int{"mover": 1, "weekly_leader": 2},
	}
	fallback, ok := promotionClaimLossFallback(candidate, "weekly_leader")
	if !ok || len(fallback.Sources) != 1 || fallback.Sources[0] != "mover" {
		t.Fatalf("fallback=%+v, ok=%t; want selected mover fallback", fallback, ok)
	}
	if _, ok := promotionClaimLossFallback(SwingCandidate{
		Ticker: "ONLY", Sources: []string{"weekly_leader"},
		promotionSourceRanks: map[string]int{"weekly_leader": 1},
	}, "weekly_leader"); ok {
		t.Fatal("single-source claim loss must not produce a fallback")
	}
}
