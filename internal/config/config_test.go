package config

import (
	"encoding/json"
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
	if cfg.Engine.TickIntervalSec != 3600 {
		t.Errorf("default tick_interval_sec = %d, want 3600", cfg.Engine.TickIntervalSec)
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
	// With registry present, validate missing env var in a registry entry.
	cfg := Defaults()
	entry := cfg.Models.Registry["analyst_cheap"]
	entry.ModelConfig.APIKeyEnv = "TE_TEST_KEY_NOTSET_XYZ"
	cfg.Models.Registry["analyst_cheap"] = entry
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

func TestValidateExtractionProviderMustExistInConfiguredModels(t *testing.T) {
	// When registry is absent, extraction_provider/model must match a configured model.
	cfg := Defaults()
	cfg.Models.Registry = nil
	cfg.Models.Roles = ModelRoles{}
	cfg.Markets.News.ExtractionEnabled = true
	cfg.Markets.News.ExtractionProvider = "openai"
	cfg.Markets.News.ExtractionModel = "gpt-4o"
	cfg.Markets.News.ExtractionRPM = 10
	cfg.Markets.News.ExtractionMaxArticlesPerTick = 5
	cfg.Models.Analysts = []ModelConfig{{Provider: "anthropic", Model: "claude-sonnet-4-6", APIKeyEnv: "ANTHROPIC_API_KEY"}}
	errs := Validate(cfg)
	if !containsStr(errs, "markets.news.extraction_provider/model") {
		t.Fatalf("expected extraction provider/model validation error, got: %v", errs)
	}
}

func TestValidateExtractionProviderSatisfiedByNewsEnricherRole(t *testing.T) {
	// extraction_provider/model should pass when the news_enricher registry role
	// matches, even if that model is not in the analyst list.
	cfg := Defaults()
	cfg.Models.Registry = map[string]ModelRegistryEntry{
		"enricher": {ModelConfig: ModelConfig{Provider: "google", Model: "gemini-2.5-flash-lite", APIKeyEnv: "GEMINI_API_KEY"}},
	}
	cfg.Models.Roles = ModelRoles{
		NewsEnricher: "enricher",
	}
	cfg.Models.Analysts = []ModelConfig{{Provider: "anthropic", Model: "claude-sonnet-4-6", APIKeyEnv: "ANTHROPIC_API_KEY"}}
	cfg.Markets.News.ExtractionEnabled = true
	cfg.Markets.News.ExtractionProvider = "google"
	cfg.Markets.News.ExtractionModel = "gemini-2.5-flash-lite"
	cfg.Markets.News.ExtractionRPM = 10
	cfg.Markets.News.ExtractionMaxArticlesPerTick = 5
	errs := Validate(cfg)
	if containsStr(errs, "markets.news.extraction_provider/model") {
		t.Fatalf("expected no extraction provider/model error when news_enricher role matches, got: %v", errs)
	}
}

func TestValidateExtractionProviderNotSatisfiedByUnusedRegistryEntry(t *testing.T) {
	// An entry that exists in the registry but is not wired into any role or the
	// analyst list must NOT satisfy the extraction_provider/model check — it is
	// never instantiated at runtime.
	cfg := Defaults()
	cfg.Models.Registry = map[string]ModelRegistryEntry{
		"unused": {ModelConfig: ModelConfig{Provider: "google", Model: "gemini-2.5-pro", APIKeyEnv: "GEMINI_API_KEY"}},
	}
	cfg.Models.Roles = ModelRoles{} // no news_enricher role configured
	cfg.Models.Analysts = []ModelConfig{{Provider: "anthropic", Model: "claude-sonnet-4-6", APIKeyEnv: "ANTHROPIC_API_KEY"}}
	cfg.Markets.News.ExtractionEnabled = true
	cfg.Markets.News.ExtractionProvider = "google"
	cfg.Markets.News.ExtractionModel = "gemini-2.5-pro"
	cfg.Markets.News.ExtractionRPM = 10
	cfg.Markets.News.ExtractionMaxArticlesPerTick = 5
	errs := Validate(cfg)
	if !containsStr(errs, "markets.news.extraction_provider/model") {
		t.Fatalf("expected extraction provider/model error for unused registry entry, got: %v", errs)
	}
}

func TestSetField_NestedPathAndBool(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	path := ConfigPath()
	if err := save(Defaults(), path); err != nil {
		t.Fatalf("save defaults: %v", err)
	}

	if err := SetField("notifications.telegram.enabled", "true"); err != nil {
		t.Fatalf("SetField: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Notifications.Telegram.Enabled {
		t.Fatal("expected notifications.telegram.enabled=true")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	s := string(data)
	if strings.Contains(s, "telegram.enabled") {
		t.Fatalf("unexpected flattened key in YAML: %s", s)
	}
}

func TestSetFieldAny_SequenceValue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	path := ConfigPath()
	if err := save(Defaults(), path); err != nil {
		t.Fatalf("save defaults: %v", err)
	}

	if err := SetFieldAny("notifications.telegram.notify_on", []any{"signal", "trade", "error"}); err != nil {
		t.Fatalf("SetFieldAny: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := len(cfg.Notifications.Telegram.NotifyOn), 3; got != want {
		t.Fatalf("notify_on length = %d, want %d", got, want)
	}
	if cfg.Notifications.Telegram.NotifyOn[0] != "signal" || cfg.Notifications.Telegram.NotifyOn[1] != "trade" || cfg.Notifications.Telegram.NotifyOn[2] != "error" {
		t.Fatalf("notify_on = %#v", cfg.Notifications.Telegram.NotifyOn)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	s := string(data)
	if strings.Contains(s, "[signal trade error]") {
		t.Fatalf("notify_on written as scalar, expected YAML sequence: %s", s)
	}
	if !strings.Contains(s, "notify_on:\n") {
		t.Fatalf("notify_on key missing: %s", s)
	}
	if !strings.Contains(s, "- signal") || !strings.Contains(s, "- trade") || !strings.Contains(s, "- error") {
		t.Fatalf("notify_on sequence items missing: %s", s)
	}
}

func TestSetFieldAny_JsonNumber(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	path := ConfigPath()
	save(Defaults(), path)

	// Test Integer json.Number
	if err := SetFieldAny("risk.max_open_positions", json.Number("15")); err != nil {
		t.Fatalf("SetFieldAny int: %v", err)
	}
	// Test Float json.Number
	if err := SetFieldAny("risk.max_position_pct", json.Number("3.5")); err != nil {
		t.Fatalf("SetFieldAny float: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(data)
	if strings.Contains(s, "max_open_positions: \"15\"") {
		t.Error("max_open_positions written as string, expected int")
	}
	if !strings.Contains(s, "max_open_positions: 15") {
		t.Error("max_open_positions missing or incorrect")
	}
	if !strings.Contains(s, "max_position_pct: 3.5") {
		t.Error("max_position_pct missing or incorrect")
	}
}

// ── Registry tests ────────────────────────────────────────────────────────────

func TestDefaultsRegistryPresent(t *testing.T) {
	cfg := Defaults()
	if cfg.Models.Registry == nil {
		t.Fatal("default config: models.registry is nil")
	}
	expectedKeys := []string{"analyst_cheap", "analyst_mid", "analyst_frontier", "arbiter", "classifier", "news_enricher", "embedder", "gemini_flash"}
	for _, key := range expectedKeys {
		if _, ok := cfg.Models.Registry[key]; !ok {
			t.Errorf("models.registry missing expected key %q", key)
		}
	}
}

func TestDefaultsRolesPresent(t *testing.T) {
	cfg := Defaults()
	roles := cfg.Models.Roles
	if len(roles.Analysts) != 3 {
		t.Errorf("models.roles.analysts: want 3 entries, got %d", len(roles.Analysts))
	}
	if roles.Arbiter == "" {
		t.Error("models.roles.arbiter is empty")
	}
	if roles.Classifier == "" {
		t.Error("models.roles.classifier is empty")
	}
	if roles.NewsEnricher == "" {
		t.Error("models.roles.news_enricher is empty")
	}
	if roles.Embedder == "" {
		t.Error("models.roles.embedder is empty")
	}
}

func TestResolveKnownKey(t *testing.T) {
	cfg := Defaults()
	mc, err := cfg.Models.Resolve("analyst_cheap")
	if err != nil {
		t.Fatalf("Resolve(analyst_cheap): %v", err)
	}
	if mc.Provider != "openai" {
		t.Errorf("analyst_cheap.provider = %q, want openai", mc.Provider)
	}
	if mc.Model != "gpt-4o" {
		t.Errorf("analyst_cheap.model = %q, want gpt-4o", mc.Model)
	}
}

func TestResolveUnknownKey(t *testing.T) {
	cfg := Defaults()
	_, err := cfg.Models.Resolve("does_not_exist")
	if err == nil {
		t.Fatal("expected error for unknown registry key, got nil")
	}
}

func TestResolveRoleClassifier(t *testing.T) {
	cfg := Defaults()
	mc, err := cfg.Models.ResolveRole("classifier")
	if err != nil {
		t.Fatalf("ResolveRole(classifier): %v", err)
	}
	if mc.Model != "gpt-4o-mini" {
		t.Errorf("classifier model = %q, want gpt-4o-mini", mc.Model)
	}
	if mc.MaxTokens != 256 {
		t.Errorf("classifier max_tokens = %d, want 256", mc.MaxTokens)
	}
}

func TestResolveRoleUnknownRole(t *testing.T) {
	cfg := Defaults()
	_, err := cfg.Models.ResolveRole("nonexistent_role")
	if err == nil {
		t.Fatal("expected error for unknown role, got nil")
	}
}

func TestResolveFallbackPresent(t *testing.T) {
	cfg := Defaults()
	fb, ok := cfg.Models.ResolveFallback("analyst_mid")
	if !ok {
		t.Fatal("analyst_mid should have a fallback (gemini_flash)")
	}
	if fb.Model != "gemini-2.5-flash" {
		t.Errorf("fallback model = %q, want gemini-2.5-flash", fb.Model)
	}
}

func TestResolveFallbackAbsent(t *testing.T) {
	cfg := Defaults()
	_, ok := cfg.Models.ResolveFallback("analyst_cheap")
	if ok {
		t.Error("analyst_cheap should have no fallback")
	}
}

func TestAnalystConfigsFromRegistry(t *testing.T) {
	cfg := Defaults()
	analysts := cfg.Models.AnalystConfigs()
	if len(analysts) != 3 {
		t.Fatalf("AnalystConfigs() returned %d, want 3", len(analysts))
	}
	// Ordered: cheap → mid → frontier
	if analysts[0].Model != "gpt-4o" {
		t.Errorf("analysts[0].model = %q, want gpt-4o", analysts[0].Model)
	}
	if analysts[1].Model != "gemini-3.1-pro-preview" {
		t.Errorf("analysts[1].model = %q, want gemini-3.1-pro-preview", analysts[1].Model)
	}
	if analysts[2].Model != "claude-sonnet-4-6" {
		t.Errorf("analysts[2].model = %q, want claude-sonnet-4-6", analysts[2].Model)
	}
}

func TestArbiterConfigFromRegistry(t *testing.T) {
	cfg := Defaults()
	arbiter := cfg.Models.ArbiterConfig()
	if arbiter.Provider != "anthropic" {
		t.Errorf("arbiter.provider = %q, want anthropic", arbiter.Provider)
	}
	if arbiter.MaxTokens != 4096 {
		t.Errorf("arbiter.max_tokens = %d, want 4096", arbiter.MaxTokens)
	}
}

func TestValidateRegistryMissingFallbackKey(t *testing.T) {
	cfg := Defaults()
	entry := cfg.Models.Registry["analyst_mid"]
	entry.Fallback = "nonexistent_fallback_key"
	cfg.Models.Registry["analyst_mid"] = entry
	// Clear env vars to avoid unrelated errors.
	errs := Validate(cfg)
	if !containsStr(errs, "nonexistent_fallback_key") {
		t.Errorf("expected fallback key error, got: %v", errs)
	}
}

func TestValidateRegistryUnresolvedAnalystRole(t *testing.T) {
	cfg := Defaults()
	cfg.Models.Roles.Analysts = []string{"analyst_cheap", "ghost_key"}
	errs := Validate(cfg)
	if !containsStr(errs, "ghost_key") {
		t.Errorf("expected unresolved analyst role error, got: %v", errs)
	}
}

func TestValidateRegistryUnresolvedNamedRole(t *testing.T) {
	cfg := Defaults()
	cfg.Models.Roles.Arbiter = "missing_arbiter"
	errs := Validate(cfg)
	if !containsStr(errs, "missing_arbiter") {
		t.Errorf("expected unresolved arbiter role error, got: %v", errs)
	}
}

func TestLegacyAnalystsFallback(t *testing.T) {
	// When registry is nil, AnalystConfigs() returns legacy Analysts[].
	cfg := Defaults()
	cfg.Models.Registry = nil
	cfg.Models.Roles = ModelRoles{}
	cfg.Models.Analysts = []ModelConfig{
		{Provider: "anthropic", Model: "claude-sonnet-4-5"},
	}
	analysts := cfg.Models.AnalystConfigs()
	if len(analysts) != 1 {
		t.Fatalf("expected 1 legacy analyst, got %d", len(analysts))
	}
	if analysts[0].Model != "claude-sonnet-4-5" {
		t.Errorf("legacy analyst model = %q, want claude-sonnet-4-5", analysts[0].Model)
	}
}

// ── ORB validation tests ──────────────────────────────────────────────────

func TestValidateORBInvalidRangeMinutes(t *testing.T) {
	cfg := Defaults()
	cfg.ORB.Enabled = true
	cfg.ORB.RangeMinutes = 10
	errs := Validate(cfg)
	if !containsStr(errs, "orb.range_minutes") {
		t.Errorf("expected orb.range_minutes error, got: %v", errs)
	}
}

func TestValidateORBInvalidRiskReward(t *testing.T) {
	cfg := Defaults()
	cfg.ORB.Enabled = true
	cfg.ORB.RiskReward = 1.0
	errs := Validate(cfg)
	if !containsStr(errs, "orb.risk_reward") {
		t.Errorf("expected orb.risk_reward error, got: %v", errs)
	}
}

func TestValidateORBInvalidMaxCandidates(t *testing.T) {
	cfg := Defaults()
	cfg.ORB.Enabled = true
	cfg.ORB.MaxCandidates = 0
	errs := Validate(cfg)
	if !containsStr(errs, "orb.max_candidates") {
		t.Errorf("expected orb.max_candidates error, got: %v", errs)
	}
	cfg.ORB.MaxCandidates = 6
	errs = Validate(cfg)
	if !containsStr(errs, "orb.max_candidates") {
		t.Errorf("expected orb.max_candidates error for 6, got: %v", errs)
	}
}

func TestValidateORBInvalidMaxRiskPct(t *testing.T) {
	cfg := Defaults()
	cfg.ORB.Enabled = true
	cfg.ORB.MaxRiskPct = 0
	errs := Validate(cfg)
	if !containsStr(errs, "orb.max_risk_pct") {
		t.Errorf("expected orb.max_risk_pct error, got: %v", errs)
	}
	cfg.ORB.MaxRiskPct = 6.0
	errs = Validate(cfg)
	if !containsStr(errs, "orb.max_risk_pct") {
		t.Errorf("expected orb.max_risk_pct error for 6.0, got: %v", errs)
	}
}

func TestValidateORBInvalidMode(t *testing.T) {
	cfg := Defaults()
	cfg.ORB.Enabled = true
	cfg.ORB.Mode = "invalid"
	errs := Validate(cfg)
	if !containsStr(errs, "orb.mode") {
		t.Errorf("expected orb.mode error, got: %v", errs)
	}
}

func TestValidateORBInvalidGapThreshold(t *testing.T) {
	cfg := Defaults()
	cfg.ORB.Enabled = true
	cfg.ORB.GapThresholdPct = -1.0
	errs := Validate(cfg)
	if !containsStr(errs, "orb.gap_threshold_pct") {
		t.Errorf("expected orb.gap_threshold_pct error, got: %v", errs)
	}
}

func TestValidateORBValidDefaults(t *testing.T) {
	cfg := Defaults()
	cfg.ORB.Enabled = true
	errs := Validate(cfg)
	for _, e := range errs {
		if strings.Contains(e, "orb.") {
			t.Errorf("unexpected ORB validation error with defaults: %s", e)
		}
	}
}

func TestValidateORBDisabledSkipsValidation(t *testing.T) {
	cfg := Defaults()
	cfg.ORB.Enabled = false
	cfg.ORB.RangeMinutes = 99 // invalid but should not matter
	errs := Validate(cfg)
	for _, e := range errs {
		if strings.Contains(e, "orb.") {
			t.Errorf("ORB disabled but got validation error: %s", e)
		}
	}
}

func TestDefaultsORBConfig(t *testing.T) {
	cfg := Defaults()
	if cfg.ORB.Mode != "paper" {
		t.Errorf("default orb.mode = %q, want paper", cfg.ORB.Mode)
	}
	if cfg.ORB.GapThresholdPct != 2.5 {
		t.Errorf("default orb.gap_threshold_pct = %f, want 2.5", cfg.ORB.GapThresholdPct)
	}
	if cfg.ORB.MaxCandidates != 2 {
		t.Errorf("default orb.max_candidates = %d, want 2", cfg.ORB.MaxCandidates)
	}
	if cfg.ORB.RangeMinutes != 15 {
		t.Errorf("default orb.range_minutes = %d, want 15", cfg.ORB.RangeMinutes)
	}
	if cfg.ORB.RiskReward != 2.0 {
		t.Errorf("default orb.risk_reward = %f, want 2.0", cfg.ORB.RiskReward)
	}
	if cfg.ORB.MaxRiskPct != 1.0 {
		t.Errorf("default orb.max_risk_pct = %f, want 1.0", cfg.ORB.MaxRiskPct)
	}
	if cfg.ORB.ScanCadenceMinutes != 2 {
		t.Errorf("default orb.scan_cadence_minutes = %d, want 2", cfg.ORB.ScanCadenceMinutes)
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
