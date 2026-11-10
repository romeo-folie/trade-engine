package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/snapshot"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/jev"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// fakeTypeSafe answers System One requests. verdict is "buy" (every entry
// threshold cleared), "hold" (no thesis support), or "fail" (HTTP 500).
type fakeTypeSafe struct {
	verdict string
	delay   time.Duration
	calls   atomic.Int32
}

func (f *fakeTypeSafe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			return
		}
	}
	if f.verdict == "fail" {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"boom"}`))
		return
	}
	var req struct {
		Questions map[string]jev.Question `json:"questions"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &req)
	answers := map[string]any{}
	choice := func(pick string, probs map[string]float64) map[string]any {
		return map[string]any{"type": "choice", "choice": pick, "confidence": probs[pick], "probabilities": probs}
	}
	for name := range req.Questions {
		switch name {
		case jev.QRegime:
			answers[name] = choice("trending", map[string]float64{"trending": 0.8, "ranging": 0.1, "transitional": 0.1})
		case jev.QAdverse:
			answers[name] = map[string]any{"type": "noul", "noul": 0.1}
		case jev.QThesis:
			if f.verdict == "buy" {
				answers[name] = choice(jev.ThesisStrong, map[string]float64{jev.ThesisStrong: 0.7, jev.ThesisModerate: 0.2, jev.ThesisWeak: 0.1})
			} else {
				answers[name] = choice(jev.ThesisNone, map[string]float64{jev.ThesisNone: 0.8, jev.ThesisWeak: 0.2})
			}
		case jev.QLens:
			answers[name] = choice("momentum", map[string]float64{"momentum": 0.9, jev.OptNone: 0.1})
		case jev.QTiming:
			answers[name] = choice(jev.TimingEnterNow, map[string]float64{jev.TimingEnterNow: 0.8, jev.TimingWait: 0.2})
		case jev.QExit:
			answers[name] = choice(jev.ExitHold, map[string]float64{jev.ExitHold: 0.9, jev.ExitNow: 0.1})
		case jev.QShortThesis:
			if f.verdict == "short" {
				answers[name] = choice(jev.ThesisStrong, map[string]float64{jev.ThesisStrong: 0.75, jev.ThesisModerate: 0.2, jev.ThesisWeak: 0.05})
			} else {
				answers[name] = choice(jev.ThesisNone, map[string]float64{jev.ThesisNone: 0.9, jev.ThesisWeak: 0.1})
			}
		case jev.QShortAdverse:
			answers[name] = map[string]any{"type": "noul", "noul": 0.05}
		case jev.QShortLens:
			answers[name] = choice("momentum", map[string]float64{"momentum": 0.85, jev.OptNone: 0.15})
		case jev.QShortTiming:
			answers[name] = choice(jev.TimingEnterNow, map[string]float64{jev.TimingEnterNow: 0.8, jev.TimingWait: 0.2})
		case jev.QCover:
			answers[name] = choice(jev.CoverHold, map[string]float64{jev.CoverHold: 0.8, jev.CoverNow: 0.2})
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev-test-2026", "answers": answers,
		"usage": map[string]int{"input_tokens": 1500, "output_tokens": 0}})
}

func jevTestEngine(t *testing.T, authority string, srvURL string, analysts []analysis.ModelProvider) *Engine {
	t.Helper()
	e := snapshotTestEngine(analysts, nil)
	e.cfg.Risk.ConfidenceThreshold = 0.55
	e.cfg.Risk.MinRiskReward = 1.5
	e.cfg.Analysis.DecisionMode = "jev"
	e.cfg.Analysis.Jev = config.JevConfig{
		Authority: authority, Model: "jev-test", APIKey: "ts-SECRET-KEY", BaseURL: srvURL,
		TimeoutSec: 2, ShadowGraceMs: 150,
		Policy: config.JevPolicyConfig{MinThesisSupport: 0.6, MaxAdverse: 0.4, MinLensProbability: 0.5,
			MinTimingProbability: 0.55, ExitThreshold: 0.6, StopATRMultiple: 2, TargetRMultiple: 2, SizePct: 2},
	}
	ev, err := newJevEvaluator(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.jevEvaluator = ev
	return e
}

func analysisProvider(t *testing.T, analysisID string) string {
	t.Helper()
	var provider string
	if err := store.GetDB().QueryRow(context.Background(),
		`SELECT model_provider FROM analyses WHERE id = $1`, analysisID).Scan(&provider); err != nil {
		t.Fatalf("analysis %s: %v", analysisID, err)
	}
	return provider
}

func TestJevAuthoritativeIsSoleDecisionMaker(t *testing.T) {
	requireEngineDB(t)
	ts := &fakeTypeSafe{verdict: "hold"}
	srv := httptest.NewServer(ts)
	defer srv.Close()
	analyst := &snapshotFakeProvider{name: "a", resp: holdResponse(0.7), available: true}
	e := jevTestEngine(t, config.JevAuthorityAuthoritative, srv.URL, []analysis.ModelProvider{analyst})
	ticker := uniqueTicker("JEVA")
	candles, set := snapshotTestCandles(80)

	e.runAnalysis(context.Background(), ticker, "1d", "momentum", []string{"momentum"}, candles, set, "", "", "", "", false)

	if len(analyst.seen) != 0 || ts.calls.Load() != 1 {
		t.Fatalf("analyst calls %d, jev calls %d: Jev must be the only decision maker", len(analyst.seen), ts.calls.Load())
	}
	snap, calls := latestSnapshotForTicker(t, ticker)
	if snap.Outcome != store.DecisionOutcomeCompleted || snap.Workflow != snapshot.WorkflowJev ||
		snap.PolicyVersion != jev.PolicyVersion || snap.PromptVersion != jev.QuestionSetVersion {
		t.Fatalf("snapshot = %s/%s/%s/%s (%s)", snap.Outcome, snap.Workflow, snap.PolicyVersion, snap.PromptVersion, snap.OutcomeReason)
	}
	if len(calls) != 1 || calls[0].Stage != snapshot.StageJev || calls[0].ResolvedModel != "jev-test-2026" ||
		calls[0].InputTokens != 1500 || !strings.Contains(string(calls[0].Extra), jev.ReasonThesis) {
		t.Fatalf("stage calls = %+v", calls)
	}
	if strings.Contains(calls[0].RawPrompt, "SECRET") || strings.Contains(string(snap.EffectiveConfig), "SECRET") {
		t.Fatal("TypeSafe key leaked into the snapshot")
	}
	var final snapshot.FinalOutput
	if err := json.Unmarshal(snap.FinalOutput, &final); err != nil {
		t.Fatal(err)
	}
	if final.Evaluator != snapshot.WorkflowJev || final.Response.Action != analysis.ActionHold ||
		!strings.Contains(string(final.PolicyTrace), jev.ReasonThesis) {
		t.Fatalf("final output = %+v", final)
	}
	if got := analysisProvider(t, snap.AnalysisID); got != jev.ProviderName {
		t.Fatalf("analysis provider = %q, want %q", got, jev.ProviderName)
	}
	if e.analysisSourceNode(analysis.AnalyzeResult{}) != jevSourceNode {
		t.Fatal("authoritative Jev decisions must be attributed to the jev node")
	}
}

func TestJevAuthoritativeBuyFlowsThroughEngineSignalPath(t *testing.T) {
	requireEngineDB(t)
	ts := &fakeTypeSafe{verdict: "buy"}
	srv := httptest.NewServer(ts)
	defer srv.Close()
	e := jevTestEngine(t, config.JevAuthorityAuthoritative, srv.URL, nil)
	ticker := uniqueTicker("JEVB")
	candles, set := snapshotTestCandles(80)

	e.runAnalysis(context.Background(), ticker, "1d", "momentum", []string{"momentum"}, candles, set, "", "", "", "", false)

	snap, _ := latestSnapshotForTicker(t, ticker)
	var final snapshot.FinalOutput
	if err := json.Unmarshal(snap.FinalOutput, &final); err != nil {
		t.Fatal(err)
	}
	entry := candles[len(candles)-1].Close
	if final.Response.Action != analysis.ActionBuy || final.Response.SelectedStrategy != "momentum" ||
		final.LensAudit.SelectionStatus == analysis.LensSelectionInvalid ||
		final.Response.StopLoss >= entry || final.Response.TakeProfit <= entry {
		t.Fatalf("final output = %+v (entry %v)", final, entry)
	}
	// The BUY reached signal generation, where the engine's own risk checks
	// and entry gates (not Jev) decide whether it executes.
	var n int
	if err := store.GetDB().QueryRow(context.Background(),
		`SELECT count(*) FROM signals WHERE ticker = $1`, ticker).Scan(&n); err != nil || n != 1 {
		t.Fatalf("signals for %s = %d (%v), want 1", ticker, n, err)
	}
}

func TestJevAuthoritativeFailureIsAnAnalysisFailure(t *testing.T) {
	requireEngineDB(t)
	ts := &fakeTypeSafe{verdict: "fail"}
	srv := httptest.NewServer(ts)
	defer srv.Close()
	analyst := &snapshotFakeProvider{name: "a", resp: holdResponse(0.7), available: true}
	e := jevTestEngine(t, config.JevAuthorityAuthoritative, srv.URL, []analysis.ModelProvider{analyst})
	ticker := uniqueTicker("JEVF")
	candles, set := snapshotTestCandles(80)

	e.runAnalysis(context.Background(), ticker, "1d", "momentum", []string{"momentum"}, candles, set, "", "", "", "", false)

	snap, calls := latestSnapshotForTicker(t, ticker)
	if snap.Outcome != store.DecisionOutcomeFailed || snap.AnalysisID != "" || len(analyst.seen) != 0 {
		t.Fatalf("snapshot %s analysis %q analyst calls %d: no fallback model may decide", snap.Outcome, snap.AnalysisID, len(analyst.seen))
	}
	if len(calls) != 1 || calls[0].Stage != snapshot.StageJev || !strings.Contains(calls[0].Error, "typesafe API 500") || calls[0].RawPrompt == "" {
		t.Fatalf("failed Jev call not recorded: %+v", calls)
	}
	var audit jevAudit
	if err := json.Unmarshal(calls[0].Extra, &audit); err != nil ||
		audit.Trace.PolicyVersion != jev.PolicyVersion || audit.Trace.QuestionSetVersion != jev.QuestionSetVersion {
		t.Fatalf("failed call must still record the versions it was asked under: %+v (%v)", audit, err)
	}
}

func TestJevShadowRecordsButNeverExecutes(t *testing.T) {
	requireEngineDB(t)
	ts := &fakeTypeSafe{verdict: "buy"}
	srv := httptest.NewServer(ts)
	defer srv.Close()
	a := &snapshotFakeProvider{name: "a", resp: holdResponse(0.7), available: true}
	b := &snapshotFakeProvider{name: "b", resp: holdResponse(0.72), available: true}
	e := jevTestEngine(t, config.JevAuthorityShadow, srv.URL, []analysis.ModelProvider{a, b})
	ticker := uniqueTicker("JEVS")
	candles, set := snapshotTestCandles(80)

	e.runAnalysis(context.Background(), ticker, "1d", "momentum", []string{"momentum"}, candles, set, "", "", "", "", false)

	snap, calls := latestSnapshotForTicker(t, ticker)
	if snap.Workflow != snapshot.WorkflowEnsemble || snap.PolicyVersion != analysis.ResolutionPolicyVersion {
		t.Fatalf("shadow mode must keep the baseline identity: %s/%s", snap.Workflow, snap.PolicyVersion)
	}
	var final snapshot.FinalOutput
	if err := json.Unmarshal(snap.FinalOutput, &final); err != nil {
		t.Fatal(err)
	}
	if final.Response.Action != analysis.ActionHold || final.Evaluator != snapshot.WorkflowEnsemble {
		t.Fatalf("executed decision must be the baseline HOLD: %+v", final)
	}
	var shadowCall *store.DecisionStageCall
	for i := range calls {
		if calls[i].Stage == snapshot.StageJevShadow {
			shadowCall = &calls[i]
		}
	}
	if shadowCall == nil {
		t.Fatalf("no jev_shadow stage call: %+v", calls)
	}
	var audit jevAudit
	if err := json.Unmarshal(shadowCall.Extra, &audit); err != nil {
		t.Fatal(err)
	}
	var shadowResp analysis.AnalysisResponse
	if err := json.Unmarshal(shadowCall.ParsedResponse, &shadowResp); err != nil {
		t.Fatal(err)
	}
	if audit.ExecutedAction != analysis.ActionHold || audit.Agrees == nil || *audit.Agrees || shadowResp.Action != analysis.ActionBuy {
		t.Fatalf("shadow audit = %+v, shadow decision %s", audit, shadowResp.Action)
	}
	var n int
	if err := store.GetDB().QueryRow(context.Background(),
		`SELECT count(*) FROM signals WHERE ticker = $1`, ticker).Scan(&n); err != nil || n != 0 {
		t.Fatalf("shadow Jev BUY produced %d signals (%v): shadow must never execute", n, err)
	}
	if got := analysisProvider(t, snap.AnalysisID); got == jev.ProviderName {
		t.Fatal("the persisted analysis in shadow mode must be the baseline's")
	}
}

func TestJevShadowCannotMeaningfullyDelayTheBaseline(t *testing.T) {
	requireEngineDB(t)
	ts := &fakeTypeSafe{verdict: "buy", delay: time.Second}
	srv := httptest.NewServer(ts)
	defer srv.Close()
	a := &snapshotFakeProvider{name: "a", resp: holdResponse(0.7), available: true}
	b := &snapshotFakeProvider{name: "b", resp: holdResponse(0.72), available: true}
	e := jevTestEngine(t, config.JevAuthorityShadow, srv.URL, []analysis.ModelProvider{a, b})
	ticker := uniqueTicker("JEVD")
	candles, set := snapshotTestCandles(80)

	start := time.Now()
	e.runAnalysis(context.Background(), ticker, "1d", "momentum", []string{"momentum"}, candles, set, "", "", "", "", false)
	if elapsed := time.Since(start); elapsed > 800*time.Millisecond {
		t.Fatalf("baseline analysis took %v with a slow shadow; grace is 150ms", elapsed)
	}
	snap, calls := latestSnapshotForTicker(t, ticker)
	if snap.Outcome != store.DecisionOutcomeCompleted {
		t.Fatalf("outcome = %s", snap.Outcome)
	}
	var shadowCall *store.DecisionStageCall
	for i := range calls {
		if calls[i].Stage == snapshot.StageJevShadow {
			shadowCall = &calls[i]
		}
	}
	if shadowCall == nil || !strings.Contains(shadowCall.Error, "grace period") {
		t.Fatalf("timed-out shadow not recorded: %+v", calls)
	}
}

func TestJevExitOnlyUnheldMakesNoCall(t *testing.T) {
	requireEngineDB(t)
	ts := &fakeTypeSafe{verdict: "buy"}
	srv := httptest.NewServer(ts)
	defer srv.Close()
	e := jevTestEngine(t, config.JevAuthorityAuthoritative, srv.URL, nil)
	ticker := uniqueTicker("JEVX")
	candles, set := snapshotTestCandles(80)
	// SELL-only strategies short-circuit before analysis when no position is
	// held; call the evaluator path directly to pin Jev's own guard.
	req := analysis.AnalysisRequest{Ticker: ticker, Strategy: "sell_only", Strategies: []string{"sell_only"},
		RecentCandles: []analysis.CandleSnapshot{{Close: candles[len(candles)-1].Close}}, Indicators: analysis.IndicatorSnapshot{ATR: set.ATR}}
	result, trace, err := e.analyzeWithJev(context.Background(), req)
	if err != nil || ts.calls.Load() != 0 || result.Response.Action != analysis.ActionHold ||
		trace == nil || trace.ReasonCodes[0] != jev.ReasonExitOnlyNotHeld || len(result.AnalystResults) != 0 {
		t.Fatalf("result %+v trace %+v err %v calls %d", result, trace, err, ts.calls.Load())
	}
}

func TestCheckJevStartupGuards(t *testing.T) {
	e := snapshotTestEngine(nil, nil)
	e.cfg.Analysis.DecisionMode = "jev"
	e.cfg.Analysis.Jev.Authority = config.JevAuthorityAuthoritative
	e.jevInitErr = context.DeadlineExceeded
	if err := e.checkJevStartup(); err == nil {
		t.Fatal("a missing evaluator must refuse to start")
	}
	e.jevEvaluator = &jev.Evaluator{}
	e.cfg.Markets.Stocks.Enabled, e.cfg.Markets.Stocks.Paper = true, false
	if err := e.checkJevStartup(); err == nil || !strings.Contains(err.Error(), "allow_live") {
		t.Fatalf("live authoritative Jev without allow_live: %v", err)
	}
	e.cfg.Analysis.Jev.AllowLive = true
	if err := e.checkJevStartup(); err != nil {
		t.Fatalf("allow_live opt-in: %v", err)
	}
	e.cfg.Analysis.Jev.AllowLive = false
	e.cfg.Markets.Stocks.Paper = true
	if err := e.checkJevStartup(); err != nil {
		t.Fatalf("paper authoritative Jev: %v", err)
	}
	e.cfg.Markets.Stocks.Paper = false
	e.cfg.Analysis.Jev.Authority = config.JevAuthorityShadow
	if err := e.checkJevStartup(); err != nil {
		t.Fatalf("shadow on live needs no opt-in (it never executes): %v", err)
	}
}

func TestDecisionMakerAvailabilityWithoutAnalysts(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	e.cfg.Analysis.DecisionMode = "jev"
	e.cfg.Analysis.Jev.Authority = config.JevAuthorityAuthoritative
	if e.decisionMakerAvailable() {
		t.Fatal("authoritative Jev without an evaluator is unavailable")
	}
	e.jevEvaluator = &jev.Evaluator{}
	if !e.decisionMakerAvailable() {
		t.Fatal("authoritative Jev needs no analysts")
	}
	e.cfg.Analysis.DecisionMode = "ensemble"
	e.analyzer = analysis.NewAnalyzerWithProviders(config.ModelsConfig{}, nil, nil, nil)
	if e.decisionMakerAvailable() {
		t.Fatal("an analyzer with no analysts is unavailable (and must not panic)")
	}
}

func enableShorts(e *Engine) {
	e.cfg.Risk.Shorts.Enabled = true
	e.cfg.Risk.Shorts.SizePct = 1.5
	e.cfg.Analysis.Jev.Policy.ShortMinThesisSupport = 0.65
	e.cfg.Analysis.Jev.Policy.ShortMaxAdverse = 0.35
	e.cfg.Analysis.Jev.Policy.CoverThreshold = 0.6
}

// With shorts enabled but short execution not yet built (#599 PR 3), Jev's
// SHORT decision is recorded under policy v2 and never becomes a signal or
// a broker order.
func TestJevV2ShortIsRecordedButNeverExecuted(t *testing.T) {
	requireEngineDB(t)
	ts := &fakeTypeSafe{verdict: "short"}
	srv := httptest.NewServer(ts)
	defer srv.Close()
	e := jevTestEngine(t, config.JevAuthorityAuthoritative, srv.URL, nil)
	enableShorts(e)
	ev, err := newJevEvaluator(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.jevEvaluator = ev
	ticker := uniqueTicker("JEVSH")
	candles, set := snapshotTestCandles(80)

	e.runAnalysis(context.Background(), ticker, "1d", "momentum", []string{"momentum"}, candles, set, "", "", "", "", false)

	snap, calls := latestSnapshotForTicker(t, ticker)
	if snap.PolicyVersion != jev.PolicyVersionV2 || snap.PromptVersion != jev.QuestionSetVersionV2 {
		t.Fatalf("snapshot versions = %s/%s, want v2", snap.PolicyVersion, snap.PromptVersion)
	}
	var final snapshot.FinalOutput
	if err := json.Unmarshal(snap.FinalOutput, &final); err != nil {
		t.Fatal(err)
	}
	entry := candles[len(candles)-1].Close
	if final.Response.Action != analysis.ActionShort || final.Response.StopLoss <= entry || final.Response.TakeProfit >= entry ||
		final.LensAudit.SelectionStatus == analysis.LensSelectionInvalid {
		t.Fatalf("final output = %+v (entry %v)", final, entry)
	}
	if !strings.Contains(string(snap.EffectiveConfig), `"shorts"`) {
		t.Fatal("effective config must record the shorts configuration")
	}
	var req struct {
		Questions map[string]any `json:"questions"`
	}
	if len(calls) != 1 || json.Unmarshal([]byte(calls[0].RawPrompt), &req) != nil {
		t.Fatalf("stage calls = %+v", calls)
	}
	if _, ok := req.Questions[jev.QShortThesis]; !ok {
		t.Fatal("v2 request must ask the short-side questions")
	}
	var n int
	if err := store.GetDB().QueryRow(context.Background(), `SELECT count(*) FROM signals WHERE ticker = $1`, ticker).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a SHORT produced %d signals (%v) before short execution exists", n, err)
	}
}

// With shorts disabled, v1 runs unchanged: no short questions are asked.
func TestJevShortsDisabledAsksV1Only(t *testing.T) {
	requireEngineDB(t)
	ts := &fakeTypeSafe{verdict: "hold"}
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen)
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		ts.ServeHTTP(w, r)
	}))
	defer srv.Close()
	e := jevTestEngine(t, config.JevAuthorityAuthoritative, srv.URL, nil)
	ticker := uniqueTicker("JEVV1")
	candles, set := snapshotTestCandles(80)
	e.runAnalysis(context.Background(), ticker, "1d", "momentum", []string{"momentum"}, candles, set, "", "", "", "", false)
	qs, _ := seen["questions"].(map[string]any)
	if _, ok := qs[jev.QShortThesis]; ok || len(qs) != 5 {
		t.Fatalf("shorts disabled must ask exactly the 5 v1 questions, got %v", qs)
	}
	snap, _ := latestSnapshotForTicker(t, ticker)
	if snap.PolicyVersion != jev.PolicyVersion {
		t.Fatalf("policy version = %s, want v1", snap.PolicyVersion)
	}
}

// An exit-only (sell_only) row holding a short must reach Jev's cover
// decision once shorts are enabled; the pre-model ownership gate must not
// treat a short-only holding as "no position" (#599).
func TestJevV2ExitOnlyRowWithHeldShortIsEvaluatedForCover(t *testing.T) {
	requireEngineDB(t)
	ctx := context.Background()
	for _, shortsOn := range []bool{true, false} {
		var seen map[string]any
		ts := &fakeTypeSafe{verdict: "short"}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &seen)
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			ts.ServeHTTP(w, r)
		}))
		e := jevTestEngine(t, config.JevAuthorityAuthoritative, srv.URL, nil)
		if shortsOn {
			enableShorts(e)
			ev, err := newJevEvaluator(e.cfg)
			if err != nil {
				t.Fatal(err)
			}
			e.jevEvaluator = ev
		}
		ticker := uniqueTicker("JEVXS")
		if _, err := store.InsertTrade(ctx, store.Trade{Ticker: ticker, Direction: "short", Strategy: "sell_only",
			EntryPrice: 140, Quantity: 3, StopLoss: 146, TakeProfit: 128, Status: "open"}); err != nil {
			t.Fatal(err)
		}
		candles, set := snapshotTestCandles(80)
		e.runAnalysis(ctx, ticker, "1d", "sell_only", []string{"sell_only"}, candles, set, "", "", "", "", false)
		srv.Close()

		if !shortsOn {
			if ts.calls.Load() != 0 {
				t.Fatal("with shorts disabled the exit-only gate must still skip a short-only holding")
			}
			continue
		}
		if ts.calls.Load() != 1 {
			t.Fatalf("held short on an exit-only row was not evaluated (calls=%d)", ts.calls.Load())
		}
		qs, _ := seen["questions"].(map[string]any)
		if _, ok := qs[jev.QCover]; !ok || len(qs) != 2 {
			t.Fatalf("expected the cover question set, got %v", qs)
		}
		snap, _ := latestSnapshotForTicker(t, ticker)
		var final snapshot.FinalOutput
		if err := json.Unmarshal(snap.FinalOutput, &final); err != nil {
			t.Fatal(err)
		}
		if final.Response.Action != analysis.ActionHold || !strings.Contains(string(final.PolicyTrace), jev.ReasonHoldShort) {
			t.Fatalf("final output = %+v trace %s", final.Response, final.PolicyTrace)
		}
	}
}
