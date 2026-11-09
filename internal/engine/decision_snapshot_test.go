package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/snapshot"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/cost"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/indicators"
	"github.com/romeo-folie/trade-engine/internal/portfolio"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// snapshotFakeProvider is a deterministic analyst/arbiter for the end-to-end
// snapshot tests. It records the request's cutoff to prove one clock is used.
type snapshotFakeProvider struct {
	name      string
	resp      analysis.AnalysisResponse
	err       error
	available bool
	seen      []time.Time
}

func (f *snapshotFakeProvider) Analyze(_ context.Context, req analysis.AnalysisRequest) (analysis.AnalysisResponse, analysis.ModelCallMeta, error) {
	f.seen = append(f.seen, req.DecisionCutoff)
	if f.err != nil {
		return analysis.AnalysisResponse{}, analysis.ModelCallMeta{}, f.err
	}
	return f.resp, analysis.ModelCallMeta{
		Provider: f.name, Model: f.name + "-model", ResolvedModel: f.name + "-model-2026",
		RawPrompt: "prompt for " + req.Ticker, RawResponse: "raw " + f.name,
		InputTokens: 10, OutputTokens: 5, LatencyMs: 3,
		RequestParams: map[string]any{"temperature": 0.2},
	}, nil
}
func (f *snapshotFakeProvider) ProviderName() string { return f.name }
func (f *snapshotFakeProvider) ModelName() string    { return f.name + "-model" }
func (f *snapshotFakeProvider) IsAvailable() bool    { return f.available }

func holdResponse(conf float64) analysis.AnalysisResponse {
	return analysis.AnalysisResponse{Action: analysis.ActionHold, Confidence: conf,
		Reasoning: "no setup", DecisiveReason: "No catalyst."}
}

func requireEngineDB(t *testing.T) {
	t.Helper()
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; engine DB-backed tests skipped")
	}
}

func snapshotTestEngine(analysts []analysis.ModelProvider, arbiter analysis.ModelProvider) *Engine {
	cfg := &config.Config{}
	cfg.Models.Consensus.Enabled = true
	cfg.Models.Consensus.FastPathConfidence = 0.5
	cfg.Models.Consensus.FastPathMaxSpread = 0.5
	// Credentials must never reach a snapshot.
	cfg.Models.Analysts = []config.ModelConfig{
		{Provider: "a", Model: "a-model", APIKey: "sk-live-SECRET-A", APIKeyEnv: "A_KEY"},
		{Provider: "b", Model: "b-model", APIKey: "sk-live-SECRET-B", APIKeyEnv: "B_KEY"},
	}
	cfg.Embeddings.OllamaBaseURL = "http://user:SECRET-PASS@ollama.internal"
	bus := events.New()
	return &Engine{
		cfg:         cfg,
		analyzer:    analysis.NewAnalyzerWithProviders(cfg.Models, analysts, arbiter, bus),
		portfolio:   portfolio.NewTracker(time.Hour),
		bus:         bus,
		costTracker: cost.NewTracker(),
		tickerState: map[string]*TickerState{},
	}
}

func snapshotTestCandles(n int) ([]data.Candle, *indicators.IndicatorSet) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	candles := make([]data.Candle, n)
	for i := range candles {
		p := 100 + float64(i)*0.5
		candles[i] = data.Candle{Timestamp: start.AddDate(0, 0, i), Open: p, High: p + 1, Low: p - 1, Close: p + 0.25, Volume: 1_000_000}
	}
	set, err := indicators.Compute(candles)
	if err != nil {
		panic(err)
	}
	return candles, set
}

func latestSnapshotForTicker(t *testing.T, ticker string) (*store.DecisionSnapshot, []store.DecisionStageCall) {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := store.GetDB().QueryRow(ctx,
		`SELECT id FROM decision_snapshots WHERE ticker = $1 ORDER BY created_at DESC LIMIT 1`, ticker,
	).Scan(&id); err != nil {
		t.Fatalf("no decision snapshot for %s: %v", ticker, err)
	}
	snap, err := store.GetDecisionSnapshot(ctx, id)
	if err != nil || snap == nil {
		t.Fatalf("GetDecisionSnapshot: %v", err)
	}
	calls, err := store.GetDecisionStageCalls(ctx, id)
	if err != nil {
		t.Fatalf("GetDecisionStageCalls: %v", err)
	}
	return snap, calls
}

func uniqueTicker(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()%1_000_000_000)
}

func TestDecisionSnapshotCompletedEnsembleEndToEnd(t *testing.T) {
	requireEngineDB(t)
	a := &snapshotFakeProvider{name: "a", resp: holdResponse(0.7), available: true}
	b := &snapshotFakeProvider{name: "b", resp: holdResponse(0.72), available: true}
	e := snapshotTestEngine([]analysis.ModelProvider{a, b}, nil)
	ticker := uniqueTicker("SNPC")
	candles, set := snapshotTestCandles(80)

	e.runAnalysis(context.Background(), ticker, "1d", "momentum", []string{"momentum"}, candles, set, "", "", "", "", false)

	snap, calls := latestSnapshotForTicker(t, ticker)
	if snap.Outcome != store.DecisionOutcomeCompleted || snap.CompletedAt == nil {
		t.Fatalf("outcome = %s (%s), want completed", snap.Outcome, snap.OutcomeReason)
	}
	if snap.AnalysisID == "" {
		t.Fatal("completed snapshot must link the persisted analysis row")
	}
	var analysisAction string
	if err := store.GetDB().QueryRow(context.Background(),
		`SELECT action::text FROM analyses WHERE id = $1`, snap.AnalysisID).Scan(&analysisAction); err != nil {
		t.Fatalf("linked analysis missing: %v", err)
	}
	var final snapshot.FinalOutput
	if err := json.Unmarshal(snap.FinalOutput, &final); err != nil {
		t.Fatal(err)
	}
	if string(final.Response.Action) != analysisAction || !final.FastPathed {
		t.Fatalf("final output %+v disagrees with analysis action %s", final, analysisAction)
	}
	if snap.Workflow != snapshot.WorkflowEnsemble || snap.PolicyVersion != analysis.ResolutionPolicyVersion {
		t.Fatalf("identity = %s/%s", snap.Workflow, snap.PolicyVersion)
	}
	if snap.InputHash == "" || snap.DecisionHash == "" || snap.OutputHash == "" || snap.InputHash == snap.DecisionHash {
		t.Fatalf("hashes not populated distinctly: %+v", snap)
	}

	// One clock: every provider saw the snapshot's cutoff.
	var inputs snapshot.Inputs
	if err := json.Unmarshal(snap.Inputs, &inputs); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*snapshotFakeProvider{a, b} {
		if len(p.seen) != 1 || !p.seen[0].Equal(inputs.DecisionCutoff) {
			t.Fatalf("%s saw cutoff %v, snapshot recorded %v", p.name, p.seen, inputs.DecisionCutoff)
		}
	}
	if len(inputs.RecentCandles) == 0 || inputs.RecentCandles[0].Close != candles[len(candles)-1].Close {
		t.Fatal("snapshot must hold the bars supplied to the prompt, newest first")
	}

	if len(calls) != 2 || calls[0].Provider != "a" || calls[1].Provider != "b" ||
		calls[0].ResolvedModel != "a-model-2026" || calls[0].Error != "" || len(calls[0].ParsedResponse) == 0 {
		t.Fatalf("stage calls = %+v", calls)
	}

	// Optional blocks that were not supplied are explicit, not silently empty.
	var missing []snapshot.MissingBlock
	if err := json.Unmarshal(snap.Missing, &missing); err != nil {
		t.Fatal(err)
	}
	blocks := map[string]bool{}
	for _, m := range missing {
		blocks[m.Block] = true
	}
	for _, want := range []string{"similar_analyses", "performance_context", "timing_snapshot"} {
		if !blocks[want] {
			t.Fatalf("missing blocks %v lack %s", missing, want)
		}
	}

	// No credential material in the effective configuration.
	for _, secret := range []string{"SECRET", "A_KEY", "B_KEY", "ollama.internal"} {
		if strings.Contains(string(snap.EffectiveConfig), secret) {
			t.Fatalf("effective config leaked %q: %s", secret, snap.EffectiveConfig)
		}
	}
}

func TestDecisionSnapshotFailedAnalysisKeepsPartialEvidence(t *testing.T) {
	requireEngineDB(t)
	ok := &snapshotFakeProvider{name: "a", resp: holdResponse(0.7), available: true}
	bad := &snapshotFakeProvider{name: "b", err: errors.New("provider 503: overloaded"), available: true}
	e := snapshotTestEngine([]analysis.ModelProvider{ok, bad}, nil)
	ticker := uniqueTicker("SNPF")
	candles, set := snapshotTestCandles(80)

	e.runAnalysis(context.Background(), ticker, "1d", "momentum", []string{"momentum"}, candles, set, "", "", "", "", false)

	snap, calls := latestSnapshotForTicker(t, ticker)
	if snap.Outcome != store.DecisionOutcomeFailed || !strings.Contains(snap.OutcomeReason, "analysis_failed") {
		t.Fatalf("outcome = %s (%s), want failed", snap.Outcome, snap.OutcomeReason)
	}
	if snap.AnalysisID != "" || len(snap.FinalOutput) != 0 {
		t.Fatal("a failed decision has no analysis row or final output")
	}
	if len(calls) != 2 || calls[0].RawResponse != "raw a" || calls[1].Error != "provider 503: overloaded" {
		t.Fatalf("partial stage calls not preserved: %+v", calls)
	}
}

func TestDecisionSnapshotShortCircuitWhenModelUnavailable(t *testing.T) {
	requireEngineDB(t)
	off := &snapshotFakeProvider{name: "a", available: false}
	e := snapshotTestEngine([]analysis.ModelProvider{off}, nil)
	ticker := uniqueTicker("SNPS")
	candles, set := snapshotTestCandles(80)

	e.runAnalysis(context.Background(), ticker, "1d", "momentum", []string{"momentum"}, candles, set, "", "", "", "", false)

	snap, calls := latestSnapshotForTicker(t, ticker)
	if snap.Outcome != store.DecisionOutcomeShortCircuited || snap.OutcomeReason != "model_unavailable" || snap.CompletedAt == nil {
		t.Fatalf("snapshot = %+v", snap)
	}
	if len(calls) != 0 || len(off.seen) != 0 {
		t.Fatal("a short-circuited decision makes no model call")
	}
}

func TestDecisionSnapshotRecordsArbiterOrdering(t *testing.T) {
	requireEngineDB(t)
	a := &snapshotFakeProvider{name: "a", resp: analysis.AnalysisResponse{Action: analysis.ActionWait, Confidence: 0.6,
		Reasoning: "wait", DecisiveReason: "Price is extended above resistance."}, available: true}
	b := &snapshotFakeProvider{name: "b", resp: holdResponse(0.65), available: true}
	arb := &snapshotFakeProvider{name: "arb", resp: holdResponse(0.6), available: true}
	e := snapshotTestEngine([]analysis.ModelProvider{a, b}, arb)
	ticker := uniqueTicker("SNPA")
	candles, set := snapshotTestCandles(80)

	e.runAnalysis(context.Background(), ticker, "1d", "momentum", []string{"momentum"}, candles, set, "", "", "", "", false)

	snap, calls := latestSnapshotForTicker(t, ticker)
	if snap.Outcome != store.DecisionOutcomeCompleted {
		t.Fatalf("outcome = %s (%s)", snap.Outcome, snap.OutcomeReason)
	}
	if len(calls) != 3 || calls[2].Stage != snapshot.StageArbiter {
		t.Fatalf("expected two analysts then the arbiter, got %+v", calls)
	}
	var extra snapshot.ArbiterExtra
	if err := json.Unmarshal(calls[2].Extra, &extra); err != nil {
		t.Fatal(err)
	}
	if extra.PolicyVersion != analysis.ResolutionPolicyVersion || len(extra.Ordering) != 2 {
		t.Fatalf("arbiter extra = %+v", extra)
	}
	var final snapshot.FinalOutput
	if err := json.Unmarshal(snap.FinalOutput, &final); err != nil {
		t.Fatal(err)
	}
	if !final.Arbitrated || final.FastPathed {
		t.Fatalf("final output = %+v, want arbitrated", final)
	}
}

func TestDecisionSnapshotStartupRecoveryAbandonsOnlyStalePending(t *testing.T) {
	requireEngineDB(t)
	ctx := context.Background()
	e := snapshotTestEngine(nil, nil)
	insert := func(ticker string) string {
		id, err := e.insertDecisionSnapshot(ctx, snapshot.Inputs{Ticker: ticker, Timeframe: "1d",
			DecisionCutoff: time.Now().UTC()}, nil, "", store.DecisionOutcomePending, "")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	stale := insert(uniqueTicker("SNPR"))

	// Recovery evaluated as if the process restarted after the threshold:
	// the row is now stale and is settled as abandoned.
	recoverDecisionSnapshots(ctx, time.Now().Add(staleDecisionSnapshotAge+time.Minute))
	got, _ := store.GetDecisionSnapshot(ctx, stale)
	if got.Outcome != store.DecisionOutcomeAbandoned || got.OutcomeReason == "" {
		t.Fatalf("stale pending snapshot = %s", got.Outcome)
	}
	// With the real clock, a recent pending row (an in-flight decision of a
	// concurrently running process) must survive.
	fresh := insert(uniqueTicker("SNPN"))
	recoverDecisionSnapshots(ctx, time.Now())
	if got, _ := store.GetDecisionSnapshot(ctx, fresh); got.Outcome != store.DecisionOutcomePending {
		t.Fatalf("a recent pending snapshot must survive recovery, got %s", got.Outcome)
	}

	// A late completion for an abandoned snapshot is refused, not applied.
	if err := store.CompleteDecisionSnapshot(ctx, stale, store.DecisionCompletion{
		Outcome: store.DecisionOutcomeFailed, OutcomeReason: "late",
	}); !errors.Is(err, store.ErrDecisionSnapshotNotPending) {
		t.Fatalf("late completion err = %v", err)
	}
}
