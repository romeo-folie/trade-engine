package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

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
		Version  string  `json:"version"`
		Drawdown float64 `json:"drawdown"`
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
