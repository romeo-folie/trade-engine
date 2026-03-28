package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

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
