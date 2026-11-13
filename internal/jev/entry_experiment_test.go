package jev

import (
	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/strategy/antichase"
	"math"
	"reflect"
	"testing"
)

func TestEntryExperimentPlansMatchPolicy(t *testing.T) {
	req := entryRequest()
	req.TimeframeContexts = []analysis.TimeframeContext{{Timeframe: "1h", Role: "supporting", RecentCandles: []analysis.CandleSnapshot{{Close: 103}}}}
	cfg := DefaultPolicyConfig()
	state, questions, err := EntryExperiment(req, cfg, 2.5, v2)
	if err != nil {
		t.Fatal(err)
	}
	if state.ExecutionTimeframe != "1d" || state.TimingEvidenceTimeframe != "1h" {
		t.Fatalf("timeframes: %+v", state)
	}
	long, _, err := ResolveWith(req, withShort(qualifyingAnswers(), false), cfg, 2.5, v2)
	if err != nil {
		t.Fatal(err)
	}
	short, _, err := ResolveWith(req, withShort(noLongAnswers(), true), cfg, 2.5, v2)
	if err != nil {
		t.Fatal(err)
	}
	for i, out := range []analysis.AnalysisResponse{long, short} {
		p := state.CandidatePlans[i]
		if p.Entry != 100 || p.Stop != out.StopLoss || p.Target != out.TakeProfit || p.Status != "mechanically_valid" {
			t.Fatalf("plan=%+v output=%+v", p, out)
		}
	}
	if questions["entry_blocker"].Type != TypeChoice || questions["short_entry_blocker"].Type != TypeChoice {
		t.Fatal("blocker questions missing")
	}
	if !reflect.DeepEqual(state.Inputs, BuildStateWith(req, v2).Inputs) {
		t.Fatal("experiment changed frozen inputs")
	}
	if QuestionsWith(req, v2)[QTiming].Instructions == questions[QTiming].Instructions {
		t.Fatal("timing wording unchanged")
	}
}

func TestEntryExperimentUsesOnlyNonemptyExecutionContext(t *testing.T) {
	req := entryRequest()
	req.TimeframeContexts = []analysis.TimeframeContext{{Timeframe: "1h", Role: "entry_timing"}}
	state, _, err := EntryExperiment(req, DefaultPolicyConfig(), 0, v2)
	if err != nil {
		t.Fatal(err)
	}
	if state.ExecutionTimeframe != "1d" || state.CandidatePlans[0].Entry != 100 {
		t.Fatalf("empty context mislabels price: %+v", state)
	}
	req.TimeframeContexts[0].RecentCandles = []analysis.CandleSnapshot{{Close: 102, Timestamp: req.DecisionCutoff}}
	state, _, err = EntryExperiment(req, DefaultPolicyConfig(), 0, v2)
	if err != nil {
		t.Fatal(err)
	}
	if state.ExecutionTimeframe != "1h" || state.CandidatePlans[0].Entry != 102 || !state.CandidatePlans[0].PriceTimestamp.Equal(req.DecisionCutoff) {
		t.Fatalf("explicit execution context lost: %+v", state)
	}
}

func TestEntryExperimentMissingEvidenceIsVisible(t *testing.T) {
	req := entryRequest()
	req.Indicators.TimingSnapshot = &antichase.Snapshot{EvidenceStatus: "unavailable"}
	state, _, err := EntryExperiment(req, DefaultPolicyConfig(), 0, v2)
	if err != nil {
		t.Fatal(err)
	}
	if state.CandidatePlans[0].Status != "mechanically_valid" || state.CandidatePlans[0].CurrentMarkStatus != "unavailable" {
		t.Fatalf("missing mark hidden: %+v", state)
	}
	req.Indicators.ATR = 0
	state, _, err = EntryExperiment(req, DefaultPolicyConfig(), 0, v2)
	if err != nil {
		t.Fatal(err)
	}
	if state.CandidatePlans[0].Status != "unavailable" || state.CandidatePlans[0].Reason == "" {
		t.Fatalf("invalid plan: %+v", state)
	}
	if _, _, err := EntryExperiment(req, DefaultPolicyConfig(), math.NaN(), v2); err == nil {
		t.Fatal("NaN floor accepted")
	}
	_, questions, err := EntryExperiment(heldRequest(), DefaultPolicyConfig(), 0, v2)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := questions["entry_blocker"]; ok {
		t.Fatal("held position got entry experiment")
	}
}
