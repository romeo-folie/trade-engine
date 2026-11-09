package jev

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

func entryRequest() analysis.AnalysisRequest {
	return analysis.AnalysisRequest{
		Ticker: "AAPL", Timeframe: "1d", Strategy: "momentum",
		ExecutionProfile: &tradeprofile.ExecutionProfile{
			Version: 1, RecommendedLens: "momentum", AdmissibleLenses: []string{"momentum", "trend_following", "news_sentiment"},
			PrimaryTimeframe: "1d", ScorerRationale: "test",
		},
		DecisionCutoff: time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC),
		RecentCandles:  []analysis.CandleSnapshot{{Close: 100}},
		Indicators:     analysis.IndicatorSnapshot{ATR: 2.5},
	}
}

func heldRequest() analysis.AnalysisRequest {
	req := entryRequest()
	req.Portfolio.OpenPositions = []analysis.PositionSummary{{Ticker: "AAPL", Side: "long", Quantity: 10}}
	return req
}

func choice(pick string, probs map[string]float64) Answer {
	return Answer{Type: TypeChoice, Choice: pick, Confidence: fptr(probs[pick]), Probabilities: probs}
}

// qualifyingAnswers clear every entry threshold.
func qualifyingAnswers() map[string]Answer {
	return map[string]Answer{
		QThesis:  choice(ThesisStrong, map[string]float64{ThesisStrong: 0.6, ThesisModerate: 0.25, ThesisWeak: 0.1, ThesisNone: 0.05, OptInsufficient: 0}),
		QAdverse: {Type: TypeNoul, Noul: fptr(0.1)},
		QLens:    choice("momentum", map[string]float64{"momentum": 0.7, "trend_following": 0.2, "news_sentiment": 0.05, OptNone: 0.05, OptInsufficient: 0}),
		QTiming:  choice(TimingEnterNow, map[string]float64{TimingEnterNow: 0.75, TimingWait: 0.2, OptInsufficient: 0.05}),
		QRegime:  choice("trending", map[string]float64{"trending": 0.8, "ranging": 0.1, "transitional": 0.1}),
	}
}

func TestResolveQualifiedEntryDerivesDeterministicPlan(t *testing.T) {
	req := entryRequest()
	resp, trace, err := Resolve(req, qualifyingAnswers(), DefaultPolicyConfig(), 1.5)
	if err != nil {
		t.Fatal(err)
	}
	// stop = 100 - 2*2.5 = 95; risk 5; R = max(2.0, 1.5) = 2 → target 110.
	if resp.Action != analysis.ActionBuy || resp.StopLoss != 95 || resp.TakeProfit != 110 || resp.SizePct != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	// confidence = (0.6 + 0.25) * (1 - 0.1) = 0.765
	if resp.Confidence != 0.765 || resp.SelectedStrategy != "momentum" || resp.Regime != "trending" {
		t.Fatalf("resp = %+v", resp)
	}
	if !reflect.DeepEqual(trace.ReasonCodes, []string{ReasonEntry}) || trace.PolicyVersion != PolicyVersion {
		t.Fatalf("trace = %+v", trace)
	}
	// The engine's final lens audit must accept the profile-bound BUY as-is.
	before := resp
	audit := analysis.ValidateFinalLensSelection(&resp, req, false)
	if resp.Action != analysis.ActionBuy || audit.SelectionStatus == analysis.LensSelectionInvalid ||
		!reflect.DeepEqual(resp.RejectedStrategies, before.RejectedStrategies) {
		t.Fatalf("lens audit rejected Jev BUY: %+v → %+v (%+v)", before, resp, audit)
	}
	if !strings.HasPrefix(resp.Reasoning, "["+PolicyVersion+"]") {
		t.Fatalf("reasoning should be a policy trace, got %q", resp.Reasoning)
	}
}

func TestResolveMinRiskRewardFloorsTarget(t *testing.T) {
	resp, _, err := Resolve(entryRequest(), qualifyingAnswers(), DefaultPolicyConfig(), 3)
	if err != nil || resp.TakeProfit != 115 { // 100 + 3*5
		t.Fatalf("target = %v (err %v), want 115 from the 3R floor", resp.TakeProfit, err)
	}
}

func TestResolveAbstentionsAndBlocks(t *testing.T) {
	cfg := DefaultPolicyConfig()
	cases := map[string]struct {
		mutate func(map[string]Answer, *analysis.AnalysisRequest)
		action analysis.Action
		code   string
	}{
		"insufficient thesis": {func(a map[string]Answer, _ *analysis.AnalysisRequest) {
			a[QThesis] = choice(OptInsufficient, map[string]float64{OptInsufficient: 0.9, ThesisStrong: 0.1})
		}, analysis.ActionHold, ReasonInsufficient},
		"insufficient timing": {func(a map[string]Answer, _ *analysis.AnalysisRequest) {
			a[QTiming] = choice(OptInsufficient, map[string]float64{OptInsufficient: 0.6, TimingEnterNow: 0.4})
		}, analysis.ActionHold, ReasonInsufficient},
		"adverse evidence": {func(a map[string]Answer, _ *analysis.AnalysisRequest) {
			a[QAdverse] = Answer{Type: TypeNoul, Noul: fptr(0.55)}
		}, analysis.ActionHold, ReasonAdverse},
		"weak thesis": {func(a map[string]Answer, _ *analysis.AnalysisRequest) {
			a[QThesis] = choice(ThesisWeak, map[string]float64{ThesisStrong: 0.2, ThesisModerate: 0.2, ThesisWeak: 0.5, ThesisNone: 0.1})
		}, analysis.ActionHold, ReasonThesis},
		"no lens": {func(a map[string]Answer, _ *analysis.AnalysisRequest) {
			a[QLens] = choice(OptNone, map[string]float64{OptNone: 0.6, "momentum": 0.4})
		}, analysis.ActionHold, ReasonNoLens},
		"low lens probability": {func(a map[string]Answer, _ *analysis.AnalysisRequest) {
			a[QLens] = choice("momentum", map[string]float64{"momentum": 0.4, "trend_following": 0.35, OptNone: 0.25})
		}, analysis.ActionHold, ReasonLensLow},
		"timing wait": {func(a map[string]Answer, _ *analysis.AnalysisRequest) {
			a[QTiming] = choice(TimingWait, map[string]float64{TimingWait: 0.7, TimingEnterNow: 0.3})
		}, analysis.ActionWait, ReasonTimingWait},
		"low timing probability": {func(a map[string]Answer, _ *analysis.AnalysisRequest) {
			a[QTiming] = choice(TimingEnterNow, map[string]float64{TimingEnterNow: 0.5, TimingWait: 0.45, OptInsufficient: 0.05})
		}, analysis.ActionWait, ReasonTimingLow},
		"no atr":          {func(_ map[string]Answer, r *analysis.AnalysisRequest) { r.Indicators.ATR = 0 }, analysis.ActionHold, ReasonPlanUnavailable},
		"no price":        {func(_ map[string]Answer, r *analysis.AnalysisRequest) { r.RecentCandles = nil }, analysis.ActionHold, ReasonPlanUnavailable},
		"stop below zero": {func(_ map[string]Answer, r *analysis.AnalysisRequest) { r.Indicators.ATR = 60 }, analysis.ActionHold, ReasonPlanUnavailable},
	}
	for name, tc := range cases {
		req := entryRequest()
		answers := qualifyingAnswers()
		tc.mutate(answers, &req)
		resp, trace, err := Resolve(req, answers, cfg, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if resp.Action != tc.action || len(trace.ReasonCodes) != 1 || trace.ReasonCodes[0] != tc.code {
			t.Errorf("%s: action %s codes %v, want %s %s", name, resp.Action, trace.ReasonCodes, tc.action, tc.code)
		}
		if resp.Action != analysis.ActionBuy && (resp.SizePct != 0 || resp.StopLoss != 0 || resp.TakeProfit != 0) {
			t.Errorf("%s: non-BUY must not carry a plan: %+v", name, resp)
		}
		if resp.DecisiveReason == "" {
			t.Errorf("%s: decisive reason missing", name)
		}
	}
}

func TestResolveHeldPositionAsksOnlyExitAndNeverPyramids(t *testing.T) {
	req := heldRequest()
	qs := Questions(req)
	if _, ok := qs[QThesis]; ok {
		t.Fatal("held positions must not be asked entry questions")
	}
	if _, ok := qs[QExit]; !ok {
		t.Fatal("held positions must be asked the exit question")
	}
	base := map[string]Answer{
		QAdverse: {Type: TypeNoul, Noul: fptr(0.7)},
		QRegime:  choice("ranging", map[string]float64{"trending": 0.1, "ranging": 0.8, "transitional": 0.1}),
	}
	for _, tc := range []struct {
		exit   Answer
		action analysis.Action
		code   string
	}{
		{choice(ExitNow, map[string]float64{ExitNow: 0.7, ExitHold: 0.3}), analysis.ActionSell, ReasonExit},
		{choice(ExitHold, map[string]float64{ExitNow: 0.4, ExitHold: 0.6}), analysis.ActionHold, ReasonHoldPosition},
		{choice(OptInsufficient, map[string]float64{OptInsufficient: 0.5, ExitNow: 0.45, ExitHold: 0.05}), analysis.ActionHold, ReasonExitInsufficient},
	} {
		answers := map[string]Answer{QAdverse: base[QAdverse], QRegime: base[QRegime], QExit: tc.exit}
		resp, trace, err := Resolve(req, answers, DefaultPolicyConfig(), 0)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Action != tc.action || trace.ReasonCodes[0] != tc.code || resp.SizePct != 0 {
			t.Errorf("exit %s: got %s %v", tc.exit.Choice, resp.Action, trace.ReasonCodes)
		}
	}
}

func TestResolveRejectsMalformedAnswersAndBadConfig(t *testing.T) {
	answers := qualifyingAnswers()
	delete(answers, QTiming)
	if _, _, err := Resolve(entryRequest(), answers, DefaultPolicyConfig(), 0); !errors.Is(err, ErrMalformed) {
		t.Fatalf("missing answer err = %v", err)
	}
	// An unknown lens (one Jev was never offered) is malformed, not a choice.
	answers = qualifyingAnswers()
	answers[QLens] = choice("mean_reversion", map[string]float64{"mean_reversion": 0.9, "momentum": 0.1})
	if _, _, err := Resolve(entryRequest(), answers, DefaultPolicyConfig(), 0); !errors.Is(err, ErrMalformed) {
		t.Fatalf("unoffered lens err = %v", err)
	}
	bad := DefaultPolicyConfig()
	bad.MaxAdverse = 1.5
	if _, _, err := Resolve(entryRequest(), qualifyingAnswers(), bad, 0); err == nil {
		t.Fatal("out-of-range threshold must be rejected")
	}
}

func TestResolveIsDeterministic(t *testing.T) {
	first, firstTrace, _ := Resolve(entryRequest(), qualifyingAnswers(), DefaultPolicyConfig(), 1.5)
	for i := 0; i < 50; i++ {
		got, trace, _ := Resolve(entryRequest(), qualifyingAnswers(), DefaultPolicyConfig(), 1.5)
		if !reflect.DeepEqual(got, first) || !reflect.DeepEqual(trace, firstTrace) {
			t.Fatal("same answers produced a different decision")
		}
	}
}

func TestQuestionsOfferExactlyTheAdmissibleLenses(t *testing.T) {
	opts, _ := Questions(entryRequest())[QLens].Criteria.(map[string]string)
	for _, want := range []string{"momentum", "trend_following", "news_sentiment", OptNone, OptInsufficient} {
		if _, ok := opts[want]; !ok {
			t.Fatalf("lens options %v lack %s", opts, want)
		}
	}
	if len(opts) != 5 {
		t.Fatalf("lens options %v include lenses outside the profile", opts)
	}
}

func exitOnlyRequest(held bool) analysis.AnalysisRequest {
	req := analysis.AnalysisRequest{Ticker: "XYZ", Timeframe: "1d", Strategy: "sell_only", Strategies: []string{"sell_only"},
		RecentCandles: []analysis.CandleSnapshot{{Close: 10}}, Indicators: analysis.IndicatorSnapshot{ATR: 0.5}}
	if held {
		req.Portfolio.OpenPositions = []analysis.PositionSummary{{Ticker: "XYZ", Side: "long", Quantity: 5}}
	}
	return req
}

func TestExitOnlyStrategyNeverEnters(t *testing.T) {
	// Not held: no call, HOLD, and no entry question exists to answer.
	req := exitOnlyRequest(false)
	if NeedsCall(req) || len(Questions(req)) != 0 {
		t.Fatal("an unheld exit-only row must not call Jev")
	}
	resp, trace, err := Resolve(req, nil, DefaultPolicyConfig(), 0)
	if err != nil || resp.Action != analysis.ActionHold || trace.ReasonCodes[0] != ReasonExitOnlyNotHeld {
		t.Fatalf("resp %+v trace %+v err %v", resp, trace, err)
	}
	// Even answers that would qualify an entry cannot produce a BUY.
	if resp, _, _ := Resolve(req, qualifyingAnswers(), DefaultPolicyConfig(), 0); resp.Action == analysis.ActionBuy {
		t.Fatal("exit-only row produced a BUY")
	}
	// Held: only exit and regime are asked, and SELL is reachable.
	held := exitOnlyRequest(true)
	qs := Questions(held)
	if _, ok := qs[QThesis]; ok || len(qs) != 2 {
		t.Fatalf("held exit-only questions = %v", qs)
	}
	answers := map[string]Answer{
		QExit:   choice(ExitNow, map[string]float64{ExitNow: 0.8, ExitHold: 0.2}),
		QRegime: choice("ranging", map[string]float64{"ranging": 1}),
	}
	if resp, _, err := Resolve(held, answers, DefaultPolicyConfig(), 0); err != nil || resp.Action != analysis.ActionSell {
		t.Fatalf("held exit-only resp %+v err %v", resp, err)
	}
}

func TestEvaluatorSkipsCallForUnheldExitOnly(t *testing.T) {
	// A client pointed at nothing proves no request is made.
	c, _ := NewClient(ClientConfig{BaseURL: "http://127.0.0.1:1", APIKey: "k", Model: "m"})
	d, err := (&Evaluator{Client: c, Policy: DefaultPolicyConfig()}).Evaluate(context.Background(), exitOnlyRequest(false))
	if err != nil || d.Response.Action != analysis.ActionHold || d.Meta.Provider != "" {
		t.Fatalf("decision %+v err %v", d, err)
	}
}

func TestResolveConfidenceFloorBlocksRiskRejectedEntries(t *testing.T) {
	answers := qualifyingAnswers()
	// support 0.65 clears min 0.6; confidence 0.65*(1-0.35)=0.4225 < 0.55.
	answers[QThesis] = choice(ThesisModerate, map[string]float64{ThesisStrong: 0.2, ThesisModerate: 0.45, ThesisWeak: 0.3, ThesisNone: 0.05})
	answers[QAdverse] = Answer{Type: TypeNoul, Noul: fptr(0.35)}
	resp, trace, err := Resolve(entryRequest(), answers, DefaultPolicyConfig(), 0)
	if err != nil || resp.Action != analysis.ActionHold || trace.ReasonCodes[0] != ReasonLowConfidence || resp.SelectedStrategy != OptNone {
		t.Fatalf("resp %+v trace %+v err %v", resp, trace, err)
	}
}

func TestResolveThresholdBoundaries(t *testing.T) {
	cfg := DefaultPolicyConfig()
	// Adverse exactly at the limit is tolerated (blocks only when greater).
	answers := qualifyingAnswers()
	answers[QAdverse] = Answer{Type: TypeNoul, Noul: fptr(cfg.MaxAdverse)}
	answers[QThesis] = choice(ThesisStrong, map[string]float64{ThesisStrong: 1})
	if resp, _, _ := Resolve(entryRequest(), answers, cfg, 0); resp.Action != analysis.ActionBuy {
		t.Fatalf("adverse at limit: %s", resp.Action)
	}
	// Exit probability exactly at the threshold exits.
	held := heldRequest()
	exit := map[string]Answer{
		QExit:   choice(ExitNow, map[string]float64{ExitNow: cfg.ExitThreshold, ExitHold: 1 - cfg.ExitThreshold}),
		QRegime: choice("trending", map[string]float64{"trending": 1}),
	}
	if resp, _, _ := Resolve(held, exit, cfg, 0); resp.Action != analysis.ActionSell {
		t.Fatalf("exit at threshold: %s", resp.Action)
	}
}

func TestEntryPriceUsesEntryTimingTimeframe(t *testing.T) {
	req := entryRequest()
	req.TimeframeContexts = []analysis.TimeframeContext{{Timeframe: "1h", Role: "entry_timing", RecentCandles: []analysis.CandleSnapshot{{Close: 102}}}}
	resp, trace, err := Resolve(req, qualifyingAnswers(), DefaultPolicyConfig(), 0)
	if err != nil || trace.Values["entry"] != 102 || resp.StopLoss != 97 {
		t.Fatalf("entry %v stop %v err %v", trace.Values["entry"], resp.StopLoss, err)
	}
}

// TestBracketPreservesRiskRewardFloorAfterRounding reproduces the rounding
// case that fell under 2R, then checks the property across many prices with
// the risk manager's own formula (reward/risk >= min, strict).
func TestBracketPreservesRiskRewardFloorAfterRounding(t *testing.T) {
	check := func(entry, atr, floor float64) {
		req := entryRequest()
		req.RecentCandles = []analysis.CandleSnapshot{{Close: entry}}
		req.Indicators.ATR = atr
		cfg := DefaultPolicyConfig()
		resp, _, err := Resolve(req, qualifyingAnswers(), cfg, floor)
		if err != nil || resp.Action != analysis.ActionBuy {
			t.Fatalf("entry %v atr %v: %+v %v", entry, atr, resp, err)
		}
		rr := (resp.TakeProfit - entry) / (entry - resp.StopLoss)
		if rr < math.Max(cfg.TargetRMultiple, floor) {
			t.Fatalf("entry %v atr %v floor %v: realised R:R %.10f below floor (stop %v target %v)",
				entry, atr, floor, rr, resp.StopLoss, resp.TakeProfit)
		}
	}
	check(100.01, 2.333333, 2) // the reported case
	for i := 0; i < 2000; i++ {
		entry := 5 + float64(i)*0.37331
		atr := 0.05 + float64(i%97)*0.013579
		if entry-2*atr <= 0 {
			continue
		}
		check(entry, atr, 2+float64(i%5)*0.25)
	}
}
