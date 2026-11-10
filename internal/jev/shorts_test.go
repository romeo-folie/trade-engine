package jev

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/analysis"
)

var v2 = Options{Shorts: true}

// noLongAnswers: the long side does not qualify (no thesis support).
func noLongAnswers() map[string]Answer {
	a := qualifyingAnswers()
	a[QThesis] = choice(ThesisNone, map[string]float64{ThesisNone: 0.8, ThesisWeak: 0.2})
	return a
}

func withShort(a map[string]Answer, qualifying bool) map[string]Answer {
	if qualifying {
		a[QShortThesis] = choice(ThesisStrong, map[string]float64{ThesisStrong: 0.7, ThesisModerate: 0.2, ThesisWeak: 0.1})
	} else {
		a[QShortThesis] = choice(ThesisNone, map[string]float64{ThesisNone: 0.9, ThesisWeak: 0.1})
	}
	a[QShortAdverse] = Answer{Type: TypeNoul, Noul: fptr(0.1)}
	a[QShortLens] = choice("trend_following", map[string]float64{"trend_following": 0.8, "momentum": 0.15, OptNone: 0.05})
	a[QShortTiming] = choice(TimingEnterNow, map[string]float64{TimingEnterNow: 0.8, TimingWait: 0.2})
	return a
}

func TestV2DisabledIsExactlyV1(t *testing.T) {
	req := entryRequest()
	if !reflect.DeepEqual(QuestionsWith(req, Options{}), Questions(req)) {
		t.Fatal("shorts disabled must ask exactly the v1 questions")
	}
	a, b := Options{}.Versions()
	if a != QuestionSetVersion || b != PolicyVersion {
		t.Fatalf("disabled versions = %s/%s", a, b)
	}
	r1, t1, e1 := Resolve(req, qualifyingAnswers(), DefaultPolicyConfig(), 1.5)
	r2, t2, e2 := ResolveWith(req, qualifyingAnswers(), DefaultPolicyConfig(), 1.5, Options{})
	if !reflect.DeepEqual(r1, r2) || !reflect.DeepEqual(t1, t2) || e1 != e2 {
		t.Fatal("ResolveWith(disabled) must equal Resolve")
	}
}

func TestV2AsksBothSidesForUnheldTickers(t *testing.T) {
	qs := QuestionsWith(entryRequest(), v2)
	for _, q := range []string{QThesis, QAdverse, QLens, QTiming, QRegime, QShortThesis, QShortAdverse, QShortLens, QShortTiming} {
		if _, ok := qs[q]; !ok {
			t.Fatalf("v2 entry questions lack %s", q)
		}
	}
	// pullback_strength is a long setup and is never a short lens.
	req := entryRequest()
	req.ExecutionProfile.AdmissibleLenses = []string{"pullback_strength", "momentum"}
	opts := QuestionsWith(req, v2)[QShortLens].Criteria.(map[string]string)
	if _, ok := opts["pullback_strength"]; ok {
		t.Fatal("pullback_strength offered as a short lens")
	}
	if _, ok := opts["momentum"]; !ok {
		t.Fatal("momentum must be a short lens option")
	}
}

func TestV2QualifyingShortHasInvertedPlanAndPassesLensAudit(t *testing.T) {
	req := entryRequest() // entry 100, ATR 2.5
	resp, trace, err := ResolveWith(req, withShort(noLongAnswers(), true), DefaultPolicyConfig(), 1.5, v2)
	if err != nil {
		t.Fatal(err)
	}
	// stop = 100 + 2*2.5 = 105; risk 5; R = 2 → target 90.
	if resp.Action != analysis.ActionShort || resp.StopLoss != 105 || resp.TakeProfit != 90 || resp.SizePct != 1.5 {
		t.Fatalf("short = %+v", resp)
	}
	if trace.PolicyVersion != PolicyVersionV2 || trace.QuestionSetVersion != QuestionSetVersionV2 || trace.SelectedLens != "trend_following" {
		t.Fatalf("trace = %+v", trace)
	}
	audit := analysis.ValidateFinalLensSelection(&resp, req, false)
	if resp.Action != analysis.ActionShort || audit.SelectionStatus == analysis.LensSelectionInvalid {
		t.Fatalf("lens audit rejected a valid SHORT: %+v (%+v)", resp, audit)
	}
}

func TestV2ShortLensAuditSuppressesInvalidShort(t *testing.T) {
	req := entryRequest()
	resp, _, _ := ResolveWith(req, withShort(noLongAnswers(), true), DefaultPolicyConfig(), 1.5, v2)
	resp.RejectedStrategies = map[string]string{} // incomplete coverage
	analysis.ValidateFinalLensSelection(&resp, req, false)
	if resp.Action != analysis.ActionHold || resp.SizePct != 0 || resp.StopLoss != 0 {
		t.Fatalf("invalid SHORT must be suppressed to a plan-free HOLD: %+v", resp)
	}
}

func TestV2ShortTargetKeepsRiskRewardFloorAfterRounding(t *testing.T) {
	shorts := 0
	defer func() {
		if shorts < 1500 {
			t.Fatalf("only %d/2000 cases produced a short; the property was not exercised", shorts)
		}
	}()
	for i := 0; i < 2000; i++ {
		entry := 6 + float64(i)*0.37331
		atr := 0.05 + float64(i%97)*0.013579
		floor := 2 + float64(i%5)*0.25
		req := entryRequest()
		req.RecentCandles = []analysis.CandleSnapshot{{Close: entry}}
		req.Indicators.ATR = atr
		resp, _, err := ResolveWith(req, withShort(noLongAnswers(), true), DefaultPolicyConfig(), floor, v2)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Action != analysis.ActionShort {
			continue // e.g. target would fall to zero: plan unavailable
		}
		shorts++
		rr := (entry - resp.TakeProfit) / (resp.StopLoss - entry)
		if resp.StopLoss <= entry || resp.TakeProfit >= entry || rr < math.Max(2, floor) {
			t.Fatalf("entry %v atr %v: stop %v target %v rr %.10f", entry, atr, resp.StopLoss, resp.TakeProfit, rr)
		}
	}
}

func TestV2DirectionComposition(t *testing.T) {
	cfg := DefaultPolicyConfig()
	cases := map[string]struct {
		answers map[string]Answer
		action  analysis.Action
		code    string
	}{
		"long only":    {withShort(qualifyingAnswers(), false), analysis.ActionBuy, "long:" + ReasonEntry},
		"short only":   {withShort(noLongAnswers(), true), analysis.ActionShort, "short:" + ReasonShortEntry},
		"both qualify": {withShort(qualifyingAnswers(), true), analysis.ActionHold, ReasonConflicting},
		"neither":      {withShort(noLongAnswers(), false), analysis.ActionHold, "short:" + ReasonThesis},
		"long waits, short qualifies": {func() map[string]Answer {
			a := withShort(qualifyingAnswers(), true)
			a[QTiming] = choice(TimingWait, map[string]float64{TimingWait: 0.7, TimingEnterNow: 0.3})
			return a
		}(), analysis.ActionHold, ReasonConflicting},
		"short waits, long absent": {func() map[string]Answer {
			a := withShort(noLongAnswers(), true)
			a[QShortTiming] = choice(TimingWait, map[string]float64{TimingWait: 0.7, TimingEnterNow: 0.3})
			return a
		}(), analysis.ActionWait, "short:" + ReasonTimingWait},
	}
	for name, tc := range cases {
		resp, trace, err := ResolveWith(entryRequest(), tc.answers, cfg, 1.5, v2)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found := false
		for _, c := range trace.ReasonCodes {
			if c == tc.code {
				found = true
			}
		}
		if resp.Action != tc.action || !found {
			t.Errorf("%s: action %s codes %v, want %s with %s", name, resp.Action, trace.ReasonCodes, tc.action, tc.code)
		}
		if resp.Action == analysis.ActionHold && (resp.SizePct != 0 || resp.StopLoss != 0) {
			t.Errorf("%s: HOLD carries a plan: %+v", name, resp)
		}
	}
}

func shortHeldRequest() analysis.AnalysisRequest {
	req := entryRequest()
	req.Portfolio.OpenPositions = []analysis.PositionSummary{{Ticker: "AAPL", Side: "short", Quantity: 10}}
	return req
}

func TestV2HeldShortAsksOnlyCoverAndNeverAdds(t *testing.T) {
	req := shortHeldRequest()
	qs := QuestionsWith(req, v2)
	if len(qs) != 2 || qs[QCover].Type != TypeChoice || qs[QRegime].Type != TypeChoice {
		t.Fatalf("held short questions = %v", qs)
	}
	regime := choice("trending", map[string]float64{"trending": 1})
	for _, tc := range []struct {
		cover  Answer
		action analysis.Action
		code   string
	}{
		{choice(CoverNow, map[string]float64{CoverNow: 0.7, CoverHold: 0.3}), analysis.ActionCover, ReasonCoverSignal},
		{choice(CoverHold, map[string]float64{CoverNow: 0.3, CoverHold: 0.7}), analysis.ActionHold, ReasonHoldShort},
		{choice(OptInsufficient, map[string]float64{OptInsufficient: 0.6, CoverNow: 0.4}), analysis.ActionHold, ReasonCoverInsufficient},
	} {
		resp, trace, err := ResolveWith(req, map[string]Answer{QCover: tc.cover, QRegime: regime}, DefaultPolicyConfig(), 1.5, v2)
		if err != nil || resp.Action != tc.action || trace.ReasonCodes[0] != tc.code || resp.SizePct != 0 {
			t.Errorf("cover %s: %+v %v %v", tc.cover.Choice, resp, trace.ReasonCodes, err)
		}
	}
}

func TestV2HeldLongNeverFlipsToShort(t *testing.T) {
	req := heldRequest()
	qs := QuestionsWith(req, v2)
	if _, ok := qs[QShortThesis]; ok {
		t.Fatal("a held long must not be asked short-entry questions (no flips)")
	}
	answers := map[string]Answer{
		QExit:   choice(ExitNow, map[string]float64{ExitNow: 0.8, ExitHold: 0.2}),
		QRegime: choice("ranging", map[string]float64{"ranging": 1}),
	}
	resp, trace, err := ResolveWith(req, answers, DefaultPolicyConfig(), 1.5, v2)
	if err != nil || resp.Action != analysis.ActionSell || trace.PolicyVersion != PolicyVersionV2 {
		t.Fatalf("held long: %+v %+v %v", resp, trace, err)
	}
}

func TestV2BothSidesHeldIsAnError(t *testing.T) {
	req := entryRequest()
	req.Portfolio.OpenPositions = []analysis.PositionSummary{
		{Ticker: "AAPL", Side: "long", Quantity: 5}, {Ticker: "AAPL", Side: "short", Quantity: 5},
	}
	if _, _, err := ResolveWith(req, nil, DefaultPolicyConfig(), 1.5, v2); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("both sides held must be refused: %v", err)
	}
}

func TestV2ExitOnlyRowNeverOpensEitherSide(t *testing.T) {
	req := exitOnlyRequest(false)
	if NeedsCallWith(req, v2) || len(QuestionsWith(req, v2)) != 0 {
		t.Fatal("an unheld exit-only row must not call Jev under v2 either")
	}
	resp, trace, err := ResolveWith(req, nil, DefaultPolicyConfig(), 1.5, v2)
	if err != nil || resp.Action != analysis.ActionHold || trace.PolicyVersion != PolicyVersionV2 {
		t.Fatalf("exit-only: %+v %+v %v", resp, trace, err)
	}
}

func TestV2ValidatesShortThresholds(t *testing.T) {
	cfg := DefaultPolicyConfig()
	cfg.ShortSizePct = 0
	if _, _, err := ResolveWith(entryRequest(), withShort(noLongAnswers(), true), cfg, 1.5, v2); err == nil {
		t.Fatal("a zero short size must be rejected under v2")
	}
	// v1 does not require short fields.
	if _, _, err := ResolveWith(entryRequest(), qualifyingAnswers(), cfg, 1.5, Options{}); err != nil {
		t.Fatalf("v1 must not validate short fields: %v", err)
	}
}

// TestV2ExitOnlyRowStillManagesAHeldShort: an exit-only (sell_only) row
// holding a short must still be asked a valid cover question set.
func TestV2ExitOnlyRowStillManagesAHeldShort(t *testing.T) {
	req := exitOnlyRequest(false)
	req.Portfolio.OpenPositions = []analysis.PositionSummary{{Ticker: req.Ticker, Side: "short", Quantity: 3}}
	if !NeedsCallWith(req, v2) {
		t.Fatal("a held short on an exit-only row needs a cover decision")
	}
	qs := QuestionsWith(req, v2)
	if len(qs) != 2 || qs[QRegime].Type != TypeChoice || qs[QCover].Type != TypeChoice {
		t.Fatalf("questions = %+v", qs)
	}
	answers := map[string]Answer{
		QRegime: choice("ranging", map[string]float64{"ranging": 1}),
		QCover:  choice(CoverNow, map[string]float64{CoverNow: 0.8, CoverHold: 0.2}),
	}
	if err := ValidateAnswers(qs, answers); err != nil {
		t.Fatalf("valid cover answers rejected: %v", err)
	}
	resp, _, err := ResolveWith(req, answers, DefaultPolicyConfig(), 1.5, v2)
	if err != nil || resp.Action != analysis.ActionCover {
		t.Fatalf("resp = %+v, err %v", resp, err)
	}
}
