package prompts

import (
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/strategy/pullback"
)

func TestPullbackContextReachesGeneralistAndSpecialistPrompts(t *testing.T) {
	context := &pullback.PromptContext{
		Qualified: true, SignalSession: time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC),
		SignalPrice: 120, SessionOpen: 119, SessionLow: 117,
		Return1M: 12.5, Return3M: 37.5, Return6M: 64.5, EMA9: 118, EMA21: 114,
		ConsolidationSessions:  40,
		ConsolidationStart:     time.Date(2026, time.June, 20, 0, 0, 0, 0, time.UTC),
		ConsolidationEnd:       time.Date(2026, time.August, 13, 0, 0, 0, 0, time.UTC),
		ConsolidationHigherLow: true, ConsolidationRangeNarrower: true,
		EarlyRangePct: 3.1, RecentRangePct: 1.2,
		AVWAPHigh: 118.5, AVWAPHighAnchor: time.Date(2026, time.June, 25, 0, 0, 0, 0, time.UTC),
		AVWAPLow: 117.9, AVWAPLowAnchor: time.Date(2026, time.July, 2, 0, 0, 0, 0, time.UTC),
		AVWAPDefinition:   "volume-weighted average from the named swing",
		HorizontalSupport: 118, SupportTolerancePct: 1.5,
		StackedAVWAP: 118.5, StackedCompanion: 118, StackedCompanionKind: "EMA9", Support: 118.5,
		FlushedIntoSupport: true, PositiveReclaim: true,
		StopLoss: 117, StopWidthPct: 2.5, MaxStopWidthPct: 4,
	}

	system, user := BuildStockPromptWithDiscoveryContext(
		"NVDA", []string{"pullback_strength"}, "1d", nil, Indicators{}, nil, Portfolio{},
		"", nil, "", false, nil, context,
	)
	for _, want := range []string{
		"STRATEGY GUIDANCE — pullback_strength", "deterministic entry gate remains authoritative",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("generalist system missing %q:\n%s", want, system)
		}
	}
	for _, want := range []string{
		"### PULLBACK STRENGTH (DETERMINISTIC DAILY EVALUATION)", "Qualified: true", "anchor 2026-06-25",
		"selected AVWAP=$118.50 + EMA9=$118.00", "Low-of-day stop: $117.00 (2.50% risk; maximum 4.00%)",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("generalist user prompt missing %q:\n%s", want, user)
		}
	}

	_, specialistUser := BuildRolePromptWithDiscoveryContext(
		"technical", "NVDA", []string{"pullback_strength"}, "1d", nil, Indicators{}, nil, Portfolio{},
		"", nil, "", nil, context,
	)
	if !strings.Contains(specialistUser, "### PULLBACK STRENGTH (DETERMINISTIC DAILY EVALUATION)") {
		t.Fatalf("specialist prompt did not receive deterministic pullback context:\n%s", specialistUser)
	}
}

func TestPullbackContextDoesNotDescribeFailedCheckAsQualified(t *testing.T) {
	context := &pullback.PromptContext{Qualified: false, FailedCheck: "reclaim", Reason: "no positive reclaim"}
	_, user := BuildStockPromptWithDiscoveryContext(
		"NVDA", []string{"pullback_strength"}, "1d", nil, Indicators{}, nil, Portfolio{},
		"", nil, "", false, nil, context,
	)
	if !strings.Contains(user, "Qualified: false | failed check: reclaim") || !strings.Contains(user, "Evaluator outcome: no positive reclaim") {
		t.Fatalf("failed deterministic context not rendered faithfully:\n%s", user)
	}
}
