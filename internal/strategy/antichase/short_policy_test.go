package antichase

import (
	"strings"
	"testing"
	"time"
)

// validShortSnapshot is a neutral short candidate: modest decline, nothing
// extended, exhausted, or near the 52-week low.
func validShortSnapshot() Snapshot {
	return Snapshot{
		SchemaVersion: PolicyVersion, EvidenceStatus: "available", CompletedSessions: TrueHighSessions,
		HistoryScope: "true_52w", ReferencePrice: 100, EMA20: 105, ATR14: 2,
		EMAExtensionATR: -1, RSI14: 40, StochK: 40, BBUpper: 110, BBLower: 95, VolumeRatio: 1,
		Return3SessionPct: -1, PriorLow52Week: 80, PriorLowProximity: 100.0 / 80.0,
		Side: SideShort,
	}
}

func TestEvaluateShortBlocksMirroredCombinations(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name string
		edit func(*Snapshot)
		want string
	}{
		{"stale bearish catalyst selloff", func(s *Snapshot) {
			at := now.Add(-25 * time.Hour)
			s.CatalystAt, s.CatalystAgeHours = &at, 25
			s.Return3SessionPct, s.VolumeRatio = -8, .75
		}, "stale_catalyst_selloff"},
		{"extended near the 52-week low", func(s *Snapshot) {
			s.PriorLowProximity, s.EMAExtensionATR, s.VolumeRatio = 1.04, -2, .75
		}, "prior_low_extension"},
		{"capitulation (oversold RSI)", func(s *Snapshot) { s.RSI14, s.Return3SessionPct, s.VolumeRatio = 30, -5, .75 }, "momentum_capitulation"},
		{"capitulation (below lower band)", func(s *Snapshot) {
			s.ReferencePrice, s.Return3SessionPct, s.VolumeRatio = 94, -5, .75
		}, "momentum_capitulation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validShortSnapshot()
			tc.edit(&s)
			got := Evaluate(s, DefaultPolicy())
			if got.Status != "blocked" || !strings.Contains(got.Reason, tc.want) {
				t.Fatalf("decision = %+v, want blocked %s", got, tc.want)
			}
		})
	}
}

func TestEvaluateShortPassesWhereOnlyTheLongWouldBlock(t *testing.T) {
	// A long-side exhaustion (overbought, rallying) is irrelevant to a short.
	s := validShortSnapshot()
	s.RSI14, s.StochK, s.Return3SessionPct, s.VolumeRatio = 80, 90, 6, .5
	if got := Evaluate(s, DefaultPolicy()); got.Status != "passed" {
		t.Fatalf("short of an overbought rally = %+v, want passed", got)
	}
	// And the long policy is unchanged: the same snapshot evaluated as a long
	// blocks on momentum exhaustion.
	s.Side = ""
	if got := Evaluate(s, DefaultPolicy()); got.Status != "blocked" || !strings.Contains(got.Reason, "momentum_exhaustion") {
		t.Fatalf("long evaluation changed: %+v", got)
	}
}

func TestEvaluateShortFailsClosedWithoutLowerBand(t *testing.T) {
	s := validShortSnapshot()
	s.BBLower = 0
	if got := Evaluate(s, DefaultPolicy()); got.Status != "unavailable_block" {
		t.Fatalf("missing lower band = %+v, want unavailable_block", got)
	}
}
