package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsEngineMode(t *testing.T) {
	cfg := Defaults()
	if cfg.Engine.Mode != "manual" {
		t.Errorf("default engine.mode = %q, want %q", cfg.Engine.Mode, "manual")
	}
}

func TestDefaultsTickInterval(t *testing.T) {
	cfg := Defaults()
	if cfg.Engine.TickIntervalSec != 300 {
		t.Errorf("default tick_interval_sec = %d, want 300", cfg.Engine.TickIntervalSec)
	}
}

func TestDefaultsRiskThreshold(t *testing.T) {
	cfg := Defaults()
	if cfg.Risk.ConfidenceThreshold != 0.65 {
		t.Errorf("default confidence_threshold = %f, want 0.65", cfg.Risk.ConfidenceThreshold)
	}
}

func TestSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := Defaults()
	cfg.Engine.Mode = "paper"

	if err := save(cfg, path); err != nil {
		t.Fatalf("save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "paper") {
		t.Error("saved YAML does not contain expected mode value 'paper'")
	}
}

func TestSaveCreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "config.yaml")

	if err := save(Defaults(), nested); err != nil {
		t.Fatalf("save to nested path: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestValidateInvalidMode(t *testing.T) {
	cfg := Defaults()
	cfg.Engine.Mode = "invalid"
	errs := Validate(cfg)
	if !containsStr(errs, "engine.mode") {
		t.Errorf("expected engine.mode error, got: %v", errs)
	}
}

func TestValidateValidModes(t *testing.T) {
	for _, mode := range []string{"manual", "auto", "paper"} {
		cfg := Defaults()
		cfg.Engine.Mode = mode
		errs := Validate(cfg)
		for _, e := range errs {
			if strings.Contains(e, "engine.mode") {
				t.Errorf("mode %q: unexpected engine.mode error: %s", mode, e)
			}
		}
	}
}

func TestValidateMissingDBURL(t *testing.T) {
	cfg := Defaults()
	os.Unsetenv("DATABASE_URL")
	errs := Validate(cfg)
	if !containsStr(errs, "DATABASE_URL") {
		t.Errorf("expected DATABASE_URL error, got: %v", errs)
	}
}

func TestValidateMissingAPIKey(t *testing.T) {
	cfg := Defaults()
	cfg.Models.Primary.APIKeyEnv = "TE_TEST_KEY_NOTSET_XYZ"
	os.Unsetenv("TE_TEST_KEY_NOTSET_XYZ")
	errs := Validate(cfg)
	if !containsStr(errs, "TE_TEST_KEY_NOTSET_XYZ") {
		t.Errorf("expected missing API key env error, got: %v", errs)
	}
}

func TestResolveEnvEmpty(t *testing.T) {
	if got := resolveEnv(""); got != "" {
		t.Errorf("resolveEnv(\"\") = %q, want empty", got)
	}
}

func TestResolveEnvSet(t *testing.T) {
	t.Setenv("TE_TEST_RESOLVE_XYZ", "myvalue")
	if got := resolveEnv("TE_TEST_RESOLVE_XYZ"); got != "myvalue" {
		t.Errorf("resolveEnv = %q, want %q", got, "myvalue")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func containsStr(slice []string, sub string) bool {
	for _, s := range slice {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
