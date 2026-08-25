package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestTradeWithAnalystsExitMetadataJSONContract(t *testing.T) {
	filledAt := time.Date(2026, 8, 1, 14, 5, 2, 0, time.UTC)
	payload, err := json.Marshal(tradeWithAnalysts{Trade: store.Trade{
		ID: "t1", Ticker: "PSN", ExitReason: store.ExitReasonEMA9Close,
		ExitEvidence: []byte(`{"daily_close":95,"ema9":98}`), ExitFilledAt: &filledAt,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["ExitReason"] != store.ExitReasonEMA9Close {
		t.Fatalf("ExitReason contract = %#v", decoded["ExitReason"])
	}
	if _, ok := decoded["ExitEvidence"].(map[string]any); !ok {
		t.Fatalf("ExitEvidence encoded as %T, want JSON object: %s", decoded["ExitEvidence"], payload)
	}
	if decoded["ExitFilledAt"] != filledAt.Format(time.RFC3339) {
		t.Fatalf("ExitFilledAt = %#v, want RFC3339", decoded["ExitFilledAt"])
	}

	legacy, err := json.Marshal(tradeWithAnalysts{Trade: store.Trade{ID: "legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(legacy), `"ExitFilledAt":null`) {
		t.Fatalf("legacy fill time contract changed: %s", legacy)
	}
}

func TestMain(m *testing.M) {
	// Create a dummy config file so config.Load() doesn't fail.
	tmpCfg := "test_config.yaml"
	_ = os.WriteFile(tmpCfg, []byte(`engine: { mode: paper }`), 0600)
	defer os.Remove(tmpCfg)

	// Override ConfigPath temporarily to use our dummy file.
	origPath := os.Getenv("TRADE_ENGINE_CONFIG")
	os.Setenv("TRADE_ENGINE_CONFIG", tmpCfg)
	defer os.Setenv("TRADE_ENGINE_CONFIG", origPath)

	if _, err := config.Load(); err != nil {
		fmt.Printf("failed to load mock config: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// ─── queryInt ────────────────────────────────────────────────────────────────

func TestQueryInt_defaults(t *testing.T) {
	r := &http.Request{URL: &url.URL{RawQuery: ""}}
	if got := queryInt(r, "limit", 50, 200); got != 50 {
		t.Fatalf("want 50, got %d", got)
	}
}

func TestQueryInt_validValue(t *testing.T) {
	r := &http.Request{URL: &url.URL{RawQuery: "limit=30"}}
	if got := queryInt(r, "limit", 50, 200); got != 30 {
		t.Fatalf("want 30, got %d", got)
	}
}

func TestQueryInt_capAtMax(t *testing.T) {
	r := &http.Request{URL: &url.URL{RawQuery: "limit=9999"}}
	if got := queryInt(r, "limit", 50, 200); got != 200 {
		t.Fatalf("want 200 (capped), got %d", got)
	}
}

func TestQueryInt_zeroUsesDefault(t *testing.T) {
	r := &http.Request{URL: &url.URL{RawQuery: "limit=0"}}
	if got := queryInt(r, "limit", 50, 200); got != 50 {
		t.Fatalf("want 50, got %d", got)
	}
}

func TestQueryInt_negativeUsesDefault(t *testing.T) {
	r := &http.Request{URL: &url.URL{RawQuery: "limit=-5"}}
	if got := queryInt(r, "limit", 50, 200); got != 50 {
		t.Fatalf("want 50, got %d", got)
	}
}

func TestQueryInt_noMax(t *testing.T) {
	r := &http.Request{URL: &url.URL{RawQuery: "limit=9999"}}
	// max=0 means no cap
	if got := queryInt(r, "limit", 50, 0); got != 9999 {
		t.Fatalf("want 9999 (no cap), got %d", got)
	}
}

// ─── apiNotFound ─────────────────────────────────────────────────────────────

func TestApiNotFound_returns404(t *testing.T) {
	w := httptest.NewRecorder()
	apiNotFound(w, "backtest run not found")
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
	body := w.Body.String()
	if body == "" {
		t.Fatal("expected non-empty body")
	}
}

func TestORBSessionTodayRequiresOwnerForFutures(t *testing.T) {
	srv := NewServer(Config{}, nil, nil)
	for _, tc := range []struct {
		name, target string
	}{
		{"missing owner", "/api/orb/session/today?session_name=futures"},
		{"invalid owner", "/api/orb/session/today?session_name=futures&owner_scope=unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			srv.handleORBSessionToday(w, httptest.NewRequest(http.MethodGet, tc.target, nil))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// ─── isNotFound ──────────────────────────────────────────────────────────────

func TestIsNotFound(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"backtest run not found: abc-123", true},
		{"backtest run not found: ", true},
		{"connection refused", false},
		{"", false},
	}
	for _, c := range cases {
		var err error
		if c.msg != "" {
			err = errStr(c.msg)
		}
		if got := isNotFound(err); got != c.want {
			t.Errorf("isNotFound(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

func TestBacktestInitialCash(t *testing.T) {
	tests := []struct {
		name     string
		snapshot json.RawMessage
		want     float64
	}{
		{name: "current Go JSON shape", snapshot: json.RawMessage(`{"Backtest":{"InitialCash":250000}}`), want: 250000},
		{name: "legacy yaml-style shape", snapshot: json.RawMessage(`{"backtest":{"initial_cash":75000}}`), want: 75000},
		{name: "non-positive rejected", snapshot: json.RawMessage(`{"Backtest":{"InitialCash":0}}`)},
		{name: "malformed rejected", snapshot: json.RawMessage(`{"Backtest":`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backtestInitialCash(tt.snapshot); got != tt.want {
				t.Fatalf("backtestInitialCash() = %v, want %v", got, tt.want)
			}
		})
	}
}

// errStr is a minimal error type for table-driven tests.
type errStr string

func (e errStr) Error() string { return string(e) }

// ─── handleSystemConfig ──────────────────────────────────────────────────────

func TestConfigMasking(t *testing.T) {
	// Create a dummy config with a secret.
	cfg := &config.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4o",
		APIKey:   "sk-TEST-SECRET-PLEASE-IGNORE",
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	s := string(data)
	if strings.Contains(s, "sk-TEST-SECRET") {
		t.Errorf("JSON contains secret: %s", s)
	}

	// Double check that APIKey is omitted entirely (json:"-")
	if strings.Contains(s, "\"APIKey\":") || strings.Contains(s, "\"api_key\":") {
		t.Errorf("JSON contains APIKey field: %s", s)
	}
}

// ─── GET /api/system/config ──────────────────────────────────────────────────

// TestSystemConfigGet_ServesFrontendKeys locks the response contract the Settings
// page depends on: yaml-style snake_case keys with real values. Serializing
// config.Config directly emitted Go field names ("Engine","Risk"), so every
// frontend lookup (data.engine?.mode, data.risk?.confidence_threshold, …) missed
// and the page silently rendered its hardcoded defaults.
func TestSystemConfigGet_ServesFrontendKeys(t *testing.T) {
	cfg := config.Get()
	cfg.Engine.Mode = "paper"
	cfg.Engine.TickIntervalSec = 1800
	cfg.Risk.ConfidenceThreshold = 0.72
	cfg.Risk.MinRiskReward = 1.8
	cfg.Risk.EntryGates.PullbackStrengthEnabled = true
	cfg.Cost.Profile = "balanced"
	cfg.Models.Roles.Analysts = []string{"cheap", "mid", "frontier"}

	s := NewServer(Config{}, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/system/config", nil)
	rec := httptest.NewRecorder()
	s.handleSystemConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	engine, _ := got["engine"].(map[string]any)
	if engine == nil {
		t.Fatalf("response missing lowercase 'engine' key (keys: %v)", mapKeys(got))
	}
	if engine["mode"] != "paper" {
		t.Errorf("engine.mode = %v; want paper", engine["mode"])
	}
	if engine["tick_interval_sec"] != float64(1800) {
		t.Errorf("engine.tick_interval_sec = %v; want 1800", engine["tick_interval_sec"])
	}
	risk, _ := got["risk"].(map[string]any)
	if risk == nil || risk["confidence_threshold"] != 0.72 {
		t.Errorf("risk.confidence_threshold = %v; want 0.72", risk["confidence_threshold"])
	}
	if risk == nil || risk["min_risk_reward"] != 1.8 {
		t.Errorf("risk.min_risk_reward = %v; want 1.8", risk["min_risk_reward"])
	}
	strategies, _ := got["strategies"].(map[string]any)
	pullback, _ := strategies["pullback_strength"].(map[string]any)
	if pullback == nil || pullback["enabled"] != true {
		t.Errorf("strategies.pullback_strength.enabled = %v; want true", pullback["enabled"])
	}
	cost, _ := got["cost"].(map[string]any)
	if cost == nil || cost["profile"] != "balanced" {
		t.Errorf("cost.profile = %v; want balanced", cost["profile"])
	}
	models, _ := got["models"].(map[string]any)
	roles, _ := models["roles"].(map[string]any)
	if roles == nil {
		t.Fatal("response missing models.roles")
	}
	analysts, _ := roles["analysts"].([]any)
	if len(analysts) != 3 || analysts[0] != "cheap" {
		t.Errorf("models.roles.analysts = %v; want [cheap mid frontier]", analysts)
	}

	// The curated DTO must not leak config sections the Settings page doesn't
	// render — provider registries (API key env names), markets (account IDs),
	// database, or the telegram chat_id.
	body := rec.Body.String()
	for _, leak := range []string{"registry", "markets", "database", "chat_id", "api_key", "account_id"} {
		if strings.Contains(body, leak) {
			t.Errorf("response leaks %q section: %s", leak, body)
		}
	}
}

func TestNewAnalysisDetailResponseMapsStructuredEvidence(t *testing.T) {
	a := &store.AnalysisSummary{
		IndicatorsRaw:            []byte(`{"RSI":61.25,"EMA20":102.5}`),
		SelectedLens:             "momentum",
		RejectedLensesRaw:        []byte(`{"trend_following":"trend was weak"}`),
		Regime:                   "expansion",
		LensSelectionStatus:      "selected",
		TimeframeWorkflowRaw:     []byte(`{"version":2,"routing_source":"profile","mode":"unified","primary":{"timeframe":"1D","action":"WAIT"},"contexts":[{"timeframe":"1D","role":"primary_thesis","status":"included"},{"timeframe":"4H","role":"entry_timing","status":"included"}],"final_action":"WAIT","decisive_reason":"4H volume did not confirm the entry."}`),
		RelatedSignalAction:      "BUY",
		RelatedSignalStatus:      "pending",
		RelatedSignalBlockReason: "",
		EntrySetup:               "standard",
		EntrySetupEvidence:       "entry level reclaimed",
		EntryGateDecisionsRaw:    []byte(`[{"name":"momentum_anti_chase","status":"passed","reason":"extension acceptable"}]`),
	}

	got := newAnalysisDetailResponse(a)
	if string(got.Indicators) != string(a.IndicatorsRaw) {
		t.Fatalf("indicators = %s; want %s", got.Indicators, a.IndicatorsRaw)
	}
	if got.SelectedLens != "momentum" || got.RejectedLenses["trend_following"] != "trend was weak" {
		t.Fatalf("lens audit = selected %q rejected %#v", got.SelectedLens, got.RejectedLenses)
	}
	if got.TimeframeWorkflow == nil || len(got.TimeframeWorkflow.Contexts) != 2 || got.TimeframeWorkflow.DecisiveReason != "4H volume did not confirm the entry." {
		t.Fatalf("workflow = %+v", got.TimeframeWorkflow)
	}
	if len(got.EntryGateDecisions) != 1 || got.EntryGateDecisions[0].Name != "momentum_anti_chase" {
		t.Fatalf("entry gates = %+v", got.EntryGateDecisions)
	}
}

// TestSystemConfigGet_PrefersLoadedGlobal pins the fallback order: the loaded
// global (which reflects POST updates within the process) must win over the
// EngineConfig captured at server construction. The remaining branch — neither
// loaded → 503 instead of the old config.Get() panic — cannot be exercised here
// because the package-shared global cannot be safely unloaded mid-suite.
func TestSystemConfigGet_PrefersLoadedGlobal(t *testing.T) {
	if !config.Loaded() {
		t.Skip("global config unexpectedly not loaded")
	}
	other := *config.Get()
	other.Engine.Mode = "from-engine-cfg"
	s2 := NewServer(Config{EngineConfig: &other}, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/system/config", nil)
	rec := httptest.NewRecorder()
	s2.handleSystemConfig(rec, req)
	var got struct {
		Engine struct {
			Mode string `json:"mode"`
		} `json:"engine"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Global is loaded in this test binary, so it wins over EngineConfig —
	// it reflects POST updates within the process.
	if got.Engine.Mode == "from-engine-cfg" {
		t.Error("engineCfg overrode the loaded global; global must be preferred")
	}
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ─── Portfolio Handlers (No DB) ──────────────────────────────────────────────

func TestPortfolioHandlers_NoDB(t *testing.T) {
	// Since store.Connect hasn't been called, store.IsConnected() returns false.
	srv := NewServer(Config{}, nil, nil)

	// Stats still returns 500 when DB is unavailable (no fallback exists).
	t.Run("Stats", func(t *testing.T) {
		w := httptest.NewRecorder()
		srv.handlePortfolioStats(w, httptest.NewRequest("GET", "/api/portfolio/stats", nil))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "database not connected") {
			t.Errorf("expected 'database not connected' error, got %q", w.Body.String())
		}
	})

	// History and Performance now return 200 with empty/partial data when DB is
	// unavailable — the broker-direct path succeeds without a DB connection.
	t.Run("History_NoDB_Returns200", func(t *testing.T) {
		w := httptest.NewRecorder()
		srv.handlePortfolioHistory(w, httptest.NewRequest("GET", "/api/portfolio/history", nil))
		if w.Code != http.StatusOK {
			t.Errorf("expected 200 (graceful empty), got %d", w.Code)
		}
	})

	t.Run("Performance_NoDB_Returns200", func(t *testing.T) {
		w := httptest.NewRecorder()
		srv.handlePortfolioPerformance(w, httptest.NewRequest("GET", "/api/portfolio/performance", nil))
		if w.Code != http.StatusOK {
			t.Errorf("expected 200 (graceful empty), got %d", w.Code)
		}
	})
}

func TestScannerPromotionEvidenceRoute_NoDB(t *testing.T) {
	srv := NewServer(Config{}, nil, nil)
	mux := http.NewServeMux()
	srv.registerAPIHandlers(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/scanner/promotion-evidence", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(rec.Body.String(), "database not connected") {
		t.Fatalf("body = %q, want database error", rec.Body.String())
	}
}

// ─── DailyCounters Handler ───────────────────────────────────────────────────

// Without a DB, the counters endpoint should 500 with "database not connected"
// — same shape as Stats above. Validates the route is wired and the
// not-connected branch is hit before any query attempt.
func TestHandleDailyCounters_NoDB(t *testing.T) {
	srv := NewServer(Config{}, nil, nil)

	w := httptest.NewRecorder()
	srv.handlePortfolioDailyCounters(w, httptest.NewRequest("GET", "/api/portfolio/daily-counters", nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "database not connected") {
		t.Errorf("expected 'database not connected' error, got %q", w.Body.String())
	}
}

// queryInt(days, default=30, max=90) — verify the cap kicks in and the
// default applies when missing. Mirrors the existing TestQueryInt cases but
// pinned to the daily-counters limits so a future bump is caught here too.
func TestDailyCounters_DaysClamp(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", 30},
		{"days=7", 7},
		{"days=200", 90}, // capped at max
		{"days=-1", 30},  // negative → default
	}
	for _, c := range cases {
		r := &http.Request{URL: &url.URL{RawQuery: c.raw}}
		if got := queryInt(r, "days", 30, 90); got != c.want {
			t.Errorf("queryInt(%q)=%d want %d", c.raw, got, c.want)
		}
	}
}

// ─── System Status ───────────────────────────────────────────────────────────

func TestHandleSystemStatus_Structure(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/system/status", nil)

	srv := NewServer(Config{}, nil, nil)
	srv.handleSystemStatus(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Version             string  `json:"version"`
		Drawdown            float64 `json:"drawdown"`
		BuyEntriesEnabled   bool    `json:"buy_entries_enabled"`
		BuyControlAvailable bool    `json:"buy_control_available"`
		Readiness           struct {
			Reasons []string `json:"reasons"`
		} `json:"readiness"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if resp.Version == "" {
		t.Error("expected non-empty version")
	}
	// drawdown should be 0.0 when not connected
	if resp.Drawdown != 0.0 {
		t.Errorf("expected drawdown 0.0, got %f", resp.Drawdown)
	}
	if resp.BuyEntriesEnabled || resp.BuyControlAvailable {
		t.Error("new buys and their control must be unavailable without a database")
	}
	for _, reason := range resp.Readiness.Reasons {
		if reason == "new stock buys are paused" {
			t.Fatal("unavailable buy control was reported as an operator pause")
		}
	}
	if maskAccountNumber("PA123456") != "••••3456" || maskAccountNumber("123") != "123" {
		t.Error("account masking must retain only the last four characters")
	}
}

func TestHandleSystemStatus_RejectsAccountThatEntryAdmissionWouldReject(t *testing.T) {
	account := execution.BrokerAccount{
		AccountID: "account-id", AccountNumber: "account-number", Status: "ACTIVE", Currency: "USD",
		Equity: 1_000, LastEquity: 1_000, Cash: 1_000, BuyingPower: 1_000, Multiplier: 1,
		CashAvailable: true, BuyingPowerAvailable: true, MultiplierAvailable: true, LastEquityAvailable: true,
		FetchedAt: time.Now().UTC().Add(-time.Minute),
	}
	srv := NewServer(Config{}, nil, nil)
	srv.SetBrokerView(stubCache(&stubBrokerView{account: account}))

	w := httptest.NewRecorder()
	srv.handleSystemStatus(w, httptest.NewRequest(http.MethodGet, "/api/system/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var response struct {
		Readiness struct {
			BrokerAccount struct {
				Available bool `json:"available"`
			} `json:"broker_account"`
			Reasons []string `json:"reasons"`
		} `json:"readiness"`
	}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Readiness.BrokerAccount.Available {
		t.Fatal("readiness reported a mismatched broker account as available")
	}
	if !strings.Contains(strings.Join(response.Readiness.Reasons, " "), "snapshot is stale") {
		t.Fatalf("readiness reasons = %v, want stale-account failure", response.Readiness.Reasons)
	}
}

func TestParseBuyControlRequest(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		wantEnable bool
		wantSource string
		wantErr    bool
	}{
		{name: "explicit false", body: `{"enabled":false}`, wantEnable: false, wantSource: "web"},
		{name: "explicit true with source", body: `{"enabled":true,"source":"operator"}`, wantEnable: true, wantSource: "operator"},
		{name: "missing enabled", body: `{"source":"operator"}`, wantErr: true},
		{name: "unknown field", body: `{"enabled":true,"enabeld":false}`, wantErr: true},
		{name: "multiple values", body: `{"enabled":true} {"enabled":false}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotEnabled, gotSource, err := parseBuyControlRequest(strings.NewReader(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatal("parseBuyControlRequest error = nil")
				}
				return
			}
			if err != nil || gotEnabled != tc.wantEnable || gotSource != tc.wantSource {
				t.Fatalf("parseBuyControlRequest = (%t, %q, %v), want (%t, %q, nil)", gotEnabled, gotSource, err, tc.wantEnable, tc.wantSource)
			}
		})
	}
}

func TestAPIBadRequest(t *testing.T) {
	w := httptest.NewRecorder()
	apiBadRequest(w, "invalid buy control payload", errors.New("missing enabled"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if body["error"] != "invalid buy control payload" {
		t.Fatalf("error = %q", body["error"])
	}
}

func TestBuyControlOriginProtection(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/system/buys", strings.NewReader(`{"enabled":true}`))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Origin", "https://attacker.example")
	if isSameOriginOrNonBrowser(r) {
		t.Fatal("cross-origin browser POST was accepted")
	}
	r.Header.Set("Origin", "http://localhost:8080")
	if !isSameOriginOrNonBrowser(r) {
		t.Fatal("same-origin browser POST was rejected")
	}
	r.Host = "attacker.example"
	r.Header.Set("Origin", "http://attacker.example")
	if isSameOriginOrNonBrowser(r) {
		t.Fatal("DNS-rebound browser POST was accepted")
	}
	r.Host = "localhost:8080"
	r.Header.Del("Origin")
	if !isSameOriginOrNonBrowser(r) {
		t.Fatal("non-browser local POST was rejected")
	}
}

func TestHandleSystemRestart_MethodNotAllowed(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/system/restart", nil)
	r.RemoteAddr = "127.0.0.1:12345"

	srv := NewServer(Config{}, nil, nil)
	srv.handleSystemRestart(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestHandleSystemRestart_ForbiddenForNonLoopback(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/system/restart", nil)
	r.RemoteAddr = "10.0.0.2:12345"

	srv := NewServer(Config{}, nil, nil)
	srv.handleSystemRestart(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestHandleSystemRestart_OKTriggersSignal(t *testing.T) {
	orig := restartSignalFn
	defer func() { restartSignalFn = orig }()

	called := make(chan struct{}, 1)
	restartSignalFn = func() error {
		called <- struct{}{}
		return nil
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/system/restart", nil)
	r.RemoteAddr = "127.0.0.1:12345"

	srv := NewServer(Config{}, nil, nil)
	srv.handleSystemRestart(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	select {
	case <-called:
		// ok
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected restart signal to be triggered")
	}
}

// ─── countDistinctOpenPositions ──────────────────────────────────────────────

func TestCountDistinctOpenPositions(t *testing.T) {
	cases := []struct {
		name   string
		trades []store.Trade
		want   int
	}{
		{"empty", nil, 0},
		{"single", []store.Trade{{Ticker: "AAPL", Direction: "long"}}, 1},
		{
			// 8 JMIA-shape rows that all collapse to a single (ticker,direction).
			"duplicates collapse",
			[]store.Trade{
				{Ticker: "JMIA", Direction: "long"},
				{Ticker: "JMIA", Direction: "long"},
				{Ticker: "JMIA", Direction: "long"},
				{Ticker: "JMIA", Direction: "long"},
				{Ticker: "JMIA", Direction: "long"},
				{Ticker: "JMIA", Direction: "long"},
				{Ticker: "JMIA", Direction: "long"},
				{Ticker: "JMIA", Direction: "long"},
			},
			1,
		},
		{
			// Same ticker, opposite directions count as two positions.
			"long and short same ticker",
			[]store.Trade{
				{Ticker: "TSLA", Direction: "long"},
				{Ticker: "TSLA", Direction: "short"},
			},
			2,
		},
		{
			// Mix of distinct tickers and a duplicate.
			"mixed distinct",
			[]store.Trade{
				{Ticker: "AAPL", Direction: "long"},
				{Ticker: "MSFT", Direction: "long"},
				{Ticker: "AAPL", Direction: "long"},
				{Ticker: "NVDA", Direction: "short"},
			},
			3,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := countDistinctOpenPositions(c.trades); got != c.want {
				t.Errorf("countDistinctOpenPositions(%v) = %d, want %d", c.trades, got, c.want)
			}
		})
	}
}

// ─── Portfolio / Positions handlers (nil tracker, no DB) ─────────────────────

func TestHandlePortfolio_NilTracker_NoDB(t *testing.T) {
	// With no tracker and no DB connection, expect 500 with a clear error.
	srv := NewServer(Config{}, nil, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/node/portfolio", nil)
	srv.handlePortfolio(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not connected") {
		t.Errorf("expected 'not connected' in error body, got %q", w.Body.String())
	}
}

func TestPortfolioSnapshotResponse_OmitsUnavailablePositionStats(t *testing.T) {
	resp := portfolioSnapshotResponse(&store.PortfolioSnapshot{
		TotalValue: 100000, Cash: 80000, Invested: 20000,
		OpenPositions: 3, UnrealizedPnL: 125,
		PositionStatsAvailable: false,
	}, nil)
	if resp["PositionStatsAvailable"] != false {
		t.Fatalf("position stats availability = %v, want false", resp["PositionStatsAvailable"])
	}
	if resp["OpenPositions"] != nil || resp["UnrealizedPnL"] != nil {
		t.Fatalf("unavailable snapshot published position values: %+v", resp)
	}

	resp = portfolioSnapshotResponse(&store.PortfolioSnapshot{
		PositionStatsAvailable: true,
	}, nil)
	if resp["OpenPositions"] != 0 || resp["UnrealizedPnL"] != float64(0) {
		t.Fatalf("legitimate flat snapshot did not publish zeros: %+v", resp)
	}
}

func TestHandlePortfolioPositions_NilTracker_NoDB(t *testing.T) {
	// With no tracker and no DB connection, expect 500 with a clear error.
	srv := NewServer(Config{}, nil, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/portfolio/positions", nil)
	srv.handlePortfolioPositions(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not connected") {
		t.Errorf("expected 'not connected' in error body, got %q", w.Body.String())
	}
}

// ─── handleRecentSignals (no DB) ─────────────────────────────────────────────

func TestHandleRecentSignals_NoDB(t *testing.T) {
	srv := NewServer(Config{}, nil, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/signals/recent", nil)
	srv.handleRecentSignals(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 (no DB), got %d", w.Code)
	}
}

func TestHandleRecentSignals_InvalidStatus(t *testing.T) {
	srv := NewServer(Config{}, nil, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/signals/recent?status=invalid", nil)
	srv.handleRecentSignals(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid status, got %d", w.Code)
	}
}

func TestHandleRecentSignals_MalformedCursor_NoComma(t *testing.T) {
	srv := NewServer(Config{}, nil, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/signals/recent?before=notacursor", nil)
	srv.handleRecentSignals(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for no-comma cursor, got %d", w.Code)
	}
}

func TestHandleRecentSignals_MalformedCursor_BadTimestamp(t *testing.T) {
	srv := NewServer(Config{}, nil, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/signals/recent?before=notadate,someid", nil)
	srv.handleRecentSignals(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad timestamp in cursor, got %d", w.Code)
	}
}

func TestHandleRecentSignals_MalformedCursor_TrailingComma(t *testing.T) {
	srv := NewServer(Config{}, nil, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/signals/recent?before=2026-04-16T09:12:33Z,", nil)
	srv.handleRecentSignals(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for trailing-comma cursor, got %d", w.Code)
	}
}
