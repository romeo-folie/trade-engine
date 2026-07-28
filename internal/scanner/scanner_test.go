package scanner

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
)

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
