package snapshot

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/strategy/antichase"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

func sampleRequest() analysis.AnalysisRequest {
	t0 := time.Date(2026, 3, 2, 15, 0, 0, 0, time.UTC)
	return analysis.AnalysisRequest{
		Ticker: "AAPL", Timeframe: "1d", Strategy: "momentum", Strategies: []string{"momentum", "news_sentiment"},
		DecisionCutoff: t0,
		RecentCandles:  []analysis.CandleSnapshot{{Timestamp: t0, Close: 101.25}, {Timestamp: t0.Add(-24 * time.Hour), Close: 100}},
		News: []analysis.NewsSnippet{
			{Headline: "older", PublishedAt: t0.Add(-5 * time.Hour), ExternalID: "n1"},
			{Headline: "tie-b", PublishedAt: t0.Add(-1 * time.Hour), ExternalID: "n3"},
			{Headline: "tie-a", PublishedAt: t0.Add(-1 * time.Hour), ExternalID: "n2"},
		},
		Portfolio: analysis.PortfolioContext{CashKnown: true, CashAvailable: 1000, OpenPositions: []analysis.PositionSummary{
			{Ticker: "MSFT", Side: "long"}, {Ticker: "AMZN", Side: "long"},
		}},
	}
}

func inputHash(t *testing.T, req analysis.AnalysisRequest) string {
	t.Helper()
	Canonicalize(&req)
	_, h, err := Encode(InputsFromRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestCanonicalizeMakesInputHashOrderIndependent(t *testing.T) {
	a := sampleRequest()
	b := sampleRequest()
	b.News[0], b.News[2] = b.News[2], b.News[0]
	b.Portfolio.OpenPositions[0], b.Portfolio.OpenPositions[1] = b.Portfolio.OpenPositions[1], b.Portfolio.OpenPositions[0]
	if inputHash(t, a) != inputHash(t, b) {
		t.Fatal("reordered news/positions must canonicalize to the same input hash")
	}

	req := sampleRequest()
	original := append([]analysis.NewsSnippet(nil), req.News...)
	callerNews := req.News
	Canonicalize(&req)
	if got := []string{req.News[0].ExternalID, req.News[1].ExternalID, req.News[2].ExternalID}; !reflect.DeepEqual(got, []string{"n2", "n3", "n1"}) {
		t.Fatalf("canonical news order = %v, want newest first with id tie-break", got)
	}
	if !reflect.DeepEqual(callerNews, original) {
		t.Fatal("Canonicalize must not reorder the caller's slice")
	}
	if req.Portfolio.OpenPositions[0].Ticker != "AMZN" {
		t.Fatalf("positions not sorted: %+v", req.Portfolio.OpenPositions)
	}
}

func TestInputHashDistinguishesChangedInputs(t *testing.T) {
	base := inputHash(t, sampleRequest())
	mutations := map[string]func(*analysis.AnalysisRequest){
		"revised bar":     func(r *analysis.AnalysisRequest) { r.RecentCandles[0].Close = 101.26 },
		"rescored news":   func(r *analysis.AnalysisRequest) { r.News[0].Sentiment = "bearish" },
		"later cutoff":    func(r *analysis.AnalysisRequest) { r.DecisionCutoff = r.DecisionCutoff.Add(time.Minute) },
		"portfolio cash":  func(r *analysis.AnalysisRequest) { r.Portfolio.CashAvailable = 999 },
		"lens menu order": func(r *analysis.AnalysisRequest) { r.Strategies = []string{"news_sentiment", "momentum"} },
	}
	for name, mutate := range mutations {
		req := sampleRequest()
		mutate(&req)
		if inputHash(t, req) == base {
			t.Errorf("%s: input hash unchanged", name)
		}
	}
	// Same instant, different location: identical identity.
	req := sampleRequest()
	req.DecisionCutoff = req.DecisionCutoff.In(time.FixedZone("EST", -5*3600))
	if inputHash(t, req) != base {
		t.Error("cutoff location must not affect the input hash")
	}
}

func TestDecisionHashSeparatesPolicyFromInputs(t *testing.T) {
	base := DecisionHash(1, "in", "policy/v1", "prompts/v1", "cfg")
	for name, h := range map[string]string{
		"schema": DecisionHash(2, "in", "policy/v1", "prompts/v1", "cfg"),
		"inputs": DecisionHash(1, "in2", "policy/v1", "prompts/v1", "cfg"),
		"policy": DecisionHash(1, "in", "policy/v2", "prompts/v1", "cfg"),
		"prompt": DecisionHash(1, "in", "policy/v1", "prompts/v2", "cfg"),
		"config": DecisionHash(1, "in", "policy/v1", "prompts/v1", "cfg2"),
	} {
		if h == base {
			t.Errorf("changing %s did not change the decision hash", name)
		}
	}
	if DecisionHash(1, "in", "policy/v1", "prompts/v1", "cfg") != base {
		t.Error("decision hash is not deterministic")
	}
}

var credentialName = regexp.MustCompile(`(?i)(api_?key|secret|password|credential|token$|base_?url|webhook|account|dsn)`)

// TestEffectiveConfigIsCredentialFree walks the projection type itself, so a
// future field that could carry a credential fails here before it ships.
func TestEffectiveConfigIsCredentialFree(t *testing.T) {
	var walk func(reflect.Type, string)
	seen := map[reflect.Type]bool{}
	walk = func(typ reflect.Type, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] || typ == reflect.TypeOf(time.Time{}) {
			return
		}
		seen[typ] = true
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if credentialName.MatchString(f.Name) {
				t.Errorf("effective config field %s.%s looks credential-bearing", path, f.Name)
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	walk(reflect.TypeOf(EffectiveConfig{}), "EffectiveConfig")

	cfg := &config.Config{}
	cfg.Models.Analysts = []config.ModelConfig{{Provider: "anthropic", Model: "m", APIKey: "sk-SECRET", APIKeyEnv: "ENV_SECRET"}}
	cfg.Models.Registry = config.ModelRegistry{"x": {ModelConfig: config.ModelConfig{Provider: "google", Model: "g", APIKey: "AIza-SECRET"}, Fallback: "y"}}
	cfg.Embeddings.APIKey, cfg.Embeddings.OllamaBaseURL = "emb-SECRET", "http://SECRET@host"
	cfg.Embeddings.SimilarAnalysesEnabled = true
	raw, _, err := Encode(NewEffectiveConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SECRET") {
		t.Fatalf("credential leaked into effective config: %s", raw)
	}
	if !strings.Contains(string(raw), `"fallback":"y"`) {
		t.Fatalf("decision-relevant settings missing: %s", raw)
	}
}

func TestStageCallsMapAnalystsSpecialistsAndArbiter(t *testing.T) {
	result := analysis.AnalyzeResult{
		AnalystResults: []analysis.AnalystResult{
			{Provider: "a", Model: "am", Response: analysis.AnalysisResponse{Action: analysis.ActionBuy},
				Meta: analysis.ModelCallMeta{RawPrompt: "p", RawResponse: "r", ResolvedModel: "am-1", RequestParams: map[string]any{"temperature": 0.1}}},
			{Provider: "b", Model: "bm", Role: "risk_manager", Err: errors.New("boom")},
		},
		Arbiter: &analysis.ArbiterResult{
			Final: analysis.AnalysisResponse{Action: analysis.ActionHold}, PolicyVersion: analysis.ResolutionPolicyVersion,
			Ordering: []analysis.ArbiterSlot{{Label: "Model-A", Provider: "a"}},
			Meta:     analysis.ModelCallMeta{Provider: "arb", Model: "arbm"},
		},
	}
	calls, err := StageCalls(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[0].Stage != StageAnalyst || calls[0].ResolvedModel != "am-1" || string(calls[0].RequestParams) != `{"temperature":0.1}` || len(calls[0].ParsedResponse) == 0 {
		t.Fatalf("analyst call = %+v", calls[0])
	}
	if calls[1].Stage != StageSpecialist || calls[1].Role != "risk_manager" || calls[1].Ordinal != 1 || calls[1].Error != "boom" || calls[1].ParsedResponse != nil {
		t.Fatalf("specialist call = %+v", calls[1])
	}
	var extra ArbiterExtra
	if err := json.Unmarshal(calls[2].Extra, &extra); err != nil || calls[2].Stage != StageArbiter || calls[2].Provider != "arb" || len(extra.Ordering) != 1 {
		t.Fatalf("arbiter call = %+v extra %+v err %v", calls[2], extra, err)
	}
}

func TestPromptTemplateVersionIsSet(t *testing.T) {
	if prompts.TemplateVersion == "" {
		t.Fatal("prompt template version must be set")
	}
}

// TestEncodeInputsIsZoneIndependent: pgx scans timestamptz into time.Local,
// so the same instants must hash identically whatever zone they carry,
// including times nested inside the timing snapshot.
func TestEncodeInputsIsZoneIndependent(t *testing.T) {
	build := func(loc *time.Location) analysis.AnalysisRequest {
		req := sampleRequest()
		for i := range req.RecentCandles {
			req.RecentCandles[i].Timestamp = req.RecentCandles[i].Timestamp.In(loc)
		}
		for i := range req.News {
			req.News[i].PublishedAt = req.News[i].PublishedAt.In(loc)
			req.News[i].IngestedAt = req.News[i].PublishedAt.Add(time.Minute)
		}
		catalyst := req.DecisionCutoff.Add(-2 * time.Hour).In(loc)
		req.Indicators.TimingSnapshot = &antichase.Snapshot{
			SchemaVersion: 1, Ticker: "AAPL", ObservedAt: req.DecisionCutoff.In(loc), CatalystAt: &catalyst,
		}
		req.DiscoveryContext = &tradeprofile.DiscoveryContext{Version: 1, Sources: []string{"earnings"},
			EvidenceBySource: map[string]json.RawMessage{"earnings": json.RawMessage(`{"date": "2026-03-01"}`)}}
		Canonicalize(&req)
		return req
	}
	encode := func(req analysis.AnalysisRequest) (json.RawMessage, string) {
		raw, h, err := EncodeInputs(InputsFromRequest(req))
		if err != nil {
			t.Fatal(err)
		}
		return raw, h
	}
	utcRaw, utcHash := encode(build(time.UTC))
	estReq := build(time.FixedZone("EST", -5*3600))
	_, estHash := encode(estReq)
	if utcHash != estHash {
		t.Fatal("identical instants in another zone produced a different input hash")
	}
	if strings.Contains(string(utcRaw), "-05:00") {
		t.Fatalf("encoded inputs retain a local offset: %s", utcRaw)
	}
	if estReq.RecentCandles[0].Timestamp.Location().String() != "EST" {
		t.Fatal("EncodeInputs must not modify the caller's request")
	}
	// Changing a nested instant still changes the identity.
	moved := build(time.UTC)
	moved.Indicators.TimingSnapshot.ObservedAt = moved.Indicators.TimingSnapshot.ObservedAt.Add(time.Second)
	if _, h := encode(moved); h == utcHash {
		t.Fatal("nested time change did not change the input hash")
	}
}

func TestBuildDecisionCompletionNeverLeavesPendingOnEncodeFailure(t *testing.T) {
	// Covered in the engine package; here we pin that Encode reports NaN,
	// the realistic unencodable value from a malformed model response.
	if _, _, err := Encode(FinalOutput{Response: analysis.AnalysisResponse{Confidence: math.NaN()}}); err == nil {
		t.Fatal("NaN must be reported as an encoding error")
	}
}
