package main

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// The standalone `te web` server previously loaded the engine config and threw
// the result away, leaving web.Config.EngineConfig nil. internal/web derives the
// pipeline snapshot from that field, so a nil config silently downgraded a
// specialist-mode or ensemble deployment to the generic single-analyst fallback
// graph in the dashboard.
func TestWebServerConfigCarriesEngineConfig(t *testing.T) {
	cfg := config.Defaults()
	cfg.Analysis.SpecialistModeEnabled = true

	got := webServerConfig("8080", cfg)

	if got.EngineConfig == nil {
		t.Fatal("EngineConfig is nil — the pipeline snapshot would fall back to the single-analyst graph")
	}
	if got.EngineConfig != cfg {
		t.Errorf("EngineConfig = %p, want the loaded config %p", got.EngineConfig, cfg)
	}
	if got.Port != "8080" {
		t.Errorf("Port = %q, want %q", got.Port, "8080")
	}
	if got.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want %q", got.Host, "127.0.0.1")
	}
}

// A failed config load must still produce a usable server rather than panicking;
// the dashboard degrades to the fallback graph but stays reachable.
func TestWebServerConfigToleratesNilEngineConfig(t *testing.T) {
	got := webServerConfig("9090", nil)

	if got.EngineConfig != nil {
		t.Errorf("EngineConfig = %v, want nil", got.EngineConfig)
	}
	wantOrigins := []string{"http://localhost:9090", "http://127.0.0.1:9090"}
	if len(got.AllowedOrigins) != len(wantOrigins) {
		t.Fatalf("AllowedOrigins = %v, want %v", got.AllowedOrigins, wantOrigins)
	}
	for i, want := range wantOrigins {
		if got.AllowedOrigins[i] != want {
			t.Errorf("AllowedOrigins[%d] = %q, want %q", i, got.AllowedOrigins[i], want)
		}
	}
}
