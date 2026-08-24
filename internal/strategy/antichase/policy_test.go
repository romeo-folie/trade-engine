package antichase

import (
	"strings"
	"testing"
	"time"
)

func validSnapshot() Snapshot {
	return Snapshot{
		SchemaVersion: PolicyVersion, EvidenceStatus: "available", CompletedSessions: TrueHighSessions,
		HistoryScope: "true_52w", ReferencePrice: 100, EMA20: 95, ATR14: 2,
		EMAExtensionATR: 1, RSI14: 60, StochK: 60, BBUpper: 105, VolumeRatio: 1,
		Return3SessionPct: 1, PriorHigh52Week: 110, PriorHighProximity: 100.0 / 110.0,
	}
}

func TestEvaluateBlocksApprovedCombinations(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name string
		edit func(*Snapshot)
		want string
	}{
		{"stale catalyst runup", func(s *Snapshot) {
			at := now.Add(-25 * time.Hour)
			s.CatalystAt = &at
			s.CatalystAgeHours = 25
			s.Return3SessionPct = 8
			s.VolumeRatio = .75
		}, "stale_catalyst_runup"},
		{"prior high extension", func(s *Snapshot) { s.PriorHighProximity = .95; s.EMAExtensionATR = 2; s.VolumeRatio = .75 }, "prior_high_extension"},
		{"momentum exhaustion", func(s *Snapshot) { s.RSI14 = 70; s.Return3SessionPct = 5; s.VolumeRatio = .75 }, "momentum_exhaustion"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSnapshot()
			tc.edit(&s)
			got := Evaluate(s, DefaultPolicy())
			if got.Status != "blocked" || !strings.Contains(got.Reason, tc.want) {
				t.Fatalf("decision = %+v, want blocked %s", got, tc.want)
			}
		})
	}
}

func TestEvaluateLimitedHistorySkipsOnlyPriorHighRule(t *testing.T) {
	s := validSnapshot()
	s.CompletedSessions = 252
	s.HistoryScope = "limited_history"
	s.PriorHighProximity = 1
	s.EMAExtensionATR = 3
	s.VolumeRatio = .5
	got := Evaluate(s, DefaultPolicy())
	if got.Status != "passed" {
		t.Fatalf("limited history should not impersonate a true 52-week high: %+v", got)
	}
}

func TestEvaluateFailsClosedForMissingCoreEvidence(t *testing.T) {
	s := validSnapshot()
	s.CompletedSessions = MinimumCompletedSessions - 1
	got := Evaluate(s, DefaultPolicy())
	if got.Status != "unavailable_block" {
		t.Fatalf("decision = %+v, want unavailable_block", got)
	}
}
