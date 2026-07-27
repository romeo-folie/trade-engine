package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestRenderPremarketEpisodicDistinguishesCatalystAndRemoval(t *testing.T) {
	var out bytes.Buffer
	run := store.PremarketEpisodicRun{
		ID: "run-1", SessionDate: time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
		CheckpointAt: time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC),
		Feed:         "sip", EntitlementStatus: "available",
		EligibleUniverseCount: 100, ShortlistedCount: 2,
		EvaluatedCount: 2, QualifiedCount: 1,
	}
	score, gap, rvol := .9, 5.0, 4.0
	rank := 1
	catalyst, _ := json.Marshal(map[string]any{"headline": "Material contract announced"})
	err := renderPremarketEpisodicReport(
		&out, run.SessionDate, run.SessionDate, false,
		[]store.PremarketEpisodicRun{run},
		[]store.PremarketEpisodicCandidate{
			{
				RunID: "run-1", Ticker: "GOOD", Outcome: "qualified",
				Score: &score, Rank: &rank, GapPct: &gap,
				TimeAdjustedRVOL: &rvol, CatalystStatus: "confirmed",
				CatalystReference: catalyst, PromotionStatus: "pending",
			},
			{
				RunID: "run-1", Ticker: "WIDE", Outcome: "not_qualified",
				RemovalStage: "spread", FailureReason: "exceeds threshold",
				CatalystStatus:  "confirmed_absent",
				PromotionStatus: "not_requested",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"GOOD", "Material contract announced", "WIDE",
		"removed: spread", "confirmed_absent",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q:\n%s", want, text)
		}
	}
}
