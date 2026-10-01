// Package config handles loading, merging, and validating TradeEngine configuration.
// Merge order: defaults → YAML file → DB overrides → CLI flags.
package config

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ── Top-level config ──────────────────────────────────────────────────────────

type Config struct {
	Engine           EngineConfig           `yaml:"engine"`
	Models           ModelsConfig           `yaml:"models"`
	Markets          MarketsConfig          `yaml:"markets"`
	Strategies       StrategiesConfig       `yaml:"strategies"`
	Risk             RiskConfig             `yaml:"risk"`
	Notifications    NotificationsConfig    `yaml:"notifications"`
	Logging          LoggingConfig          `yaml:"logging"`
	Database         DatabaseConfig         `yaml:"database"`
	Reports          ReportsConfig          `yaml:"reports"`
	Backtest         BacktestConfig         `yaml:"backtest"`
	Embeddings       EmbeddingsConfig       `yaml:"embeddings"`
	Analysis         AnalysisConfig         `yaml:"analysis"`
	Cost             CostConfig             `yaml:"cost"`
	ForexORB         ForexORBConfig         `yaml:"forex_orb"`
	CurrencyStrength CurrencyStrengthConfig `yaml:"currency_strength"`
	FuturesORB       FuturesORBConfig       `yaml:"futures_orb"`
	OptionsORB       OptionsORBConfig       `yaml:"options_orb"`
	FuturesORBMetals FuturesORBConfig       `yaml:"futures_orb_metals"`
	Scanner          ScannerConfig          `yaml:"scanner"`
	Disclosures      DisclosuresConfig      `yaml:"disclosures"`
	Feedback         FeedbackConfig         `yaml:"feedback"`
	Portfolio        PortfolioConfig        `yaml:"portfolio"`
}

// DisclosuresConfig controls periodic ingestion of official congressional
// financial-disclosure filings. Ingestion is observation-only; promotion into
// the scanner remains independently gated by scanner.political_disclosures.
type DisclosuresConfig struct {
	Enabled                    bool                   `yaml:"enabled"`
	AcknowledgeUseRestrictions bool                   `yaml:"acknowledge_use_restrictions"`
	SyncIntervalHours          int                    `yaml:"sync_interval_hours"`
	LookbackDays               int                    `yaml:"lookback_days"`
	MaxReportsPerSync          int                    `yaml:"max_reports_per_sync"`
	RequestTimeoutSeconds      int                    `yaml:"request_timeout_seconds"`
	House                      DisclosureSourceConfig `yaml:"house"`
	Senate                     SenateDisclosureConfig `yaml:"senate"`
	OGE                        DisclosureSourceConfig `yaml:"oge"`
}

type DisclosureSourceConfig struct {
	Enabled                  bool   `yaml:"enabled"`
	BaseURL                  string `yaml:"base_url"`
	RequestDelayMilliseconds int    `yaml:"request_delay_milliseconds"`
}

type SenateDisclosureConfig struct {
	DisclosureSourceConfig `yaml:",inline"`
	// AcknowledgeUseRestrictions must be explicitly enabled before the client
	// accesses Senate eFD. Browser authorization accepts the agreement visibly.
	AcknowledgeUseRestrictions bool `yaml:"acknowledge_use_restrictions"`
	// Transport is "browser" (recommended) or "http" (legacy/testing).
	Transport         string `yaml:"transport"`
	BrowserProfileDir string `yaml:"browser_profile_dir"`
	BrowserExecutable string `yaml:"browser_executable"`
}

// ValidateDisclosuresConfig validates the independently runnable disclosure
// ingester. Keeping this separate lets CLI overrides and NewRunner reject bad
// values before constructing database, HTTP, PDFium, or Chrome resources.
func ValidateDisclosuresConfig(disclosures DisclosuresConfig) []string {
	if !disclosures.Enabled {
		return nil
	}
	const prefix = "disclosures"
	var errs []string
	if !disclosures.AcknowledgeUseRestrictions {
		errs = append(errs, prefix+".acknowledge_use_restrictions must be true")
	}
	if disclosures.SyncIntervalHours <= 0 || !durationConfigFits(disclosures.SyncIntervalHours, time.Hour) {
		errs = append(errs, prefix+".sync_interval_hours must be a safe positive duration")
	}
	if disclosures.LookbackDays <= 0 {
		errs = append(errs, prefix+".lookback_days must be > 0")
	}
	if disclosures.MaxReportsPerSync <= 0 {
		errs = append(errs, prefix+".max_reports_per_sync must be > 0")
	}
	if disclosures.RequestTimeoutSeconds <= 0 || !durationConfigFits(disclosures.RequestTimeoutSeconds, time.Second) {
		errs = append(errs, prefix+".request_timeout_seconds must be a safe positive duration")
	}
	enabledSources := 0
	for _, enabled := range []bool{disclosures.House.Enabled, disclosures.Senate.Enabled, disclosures.OGE.Enabled} {
		if enabled {
			enabledSources++
		}
	}
	if enabledSources == 0 {
		errs = append(errs, prefix+" requires at least one enabled source")
	}
	if disclosures.MaxReportsPerSync > 0 && disclosures.MaxReportsPerSync < enabledSources {
		errs = append(errs, fmt.Sprintf(prefix+".max_reports_per_sync must be >= %d enabled sources", enabledSources))
	}
	if disclosures.House.Enabled && strings.TrimSpace(disclosures.House.BaseURL) == "" {
		errs = append(errs, prefix+".house.base_url must not be blank")
	}
	for _, delay := range []struct {
		name  string
		value int
	}{
		{"house.request_delay_milliseconds", disclosures.House.RequestDelayMilliseconds},
		{"senate.request_delay_milliseconds", disclosures.Senate.RequestDelayMilliseconds},
		{"oge.request_delay_milliseconds", disclosures.OGE.RequestDelayMilliseconds},
	} {
		if delay.value < 0 {
			errs = append(errs, prefix+"."+delay.name+" must be >= 0")
		} else if !durationConfigFits(delay.value, time.Millisecond) {
			errs = append(errs, prefix+"."+delay.name+" must be a safe nonnegative duration")
		}
	}
	if disclosures.Senate.Enabled {
		if strings.TrimSpace(disclosures.Senate.BaseURL) == "" {
			errs = append(errs, prefix+".senate.base_url must not be blank")
		}
		if !disclosures.Senate.AcknowledgeUseRestrictions {
			errs = append(errs, prefix+".senate.acknowledge_use_restrictions must be true")
		}
		transport := strings.ToLower(strings.TrimSpace(disclosures.Senate.Transport))
		if transport != "" && transport != "browser" && transport != "http" {
			errs = append(errs, prefix+".senate.transport must be browser or http")
		}
	}
	if disclosures.OGE.Enabled && strings.TrimSpace(disclosures.OGE.BaseURL) == "" {
		errs = append(errs, prefix+".oge.base_url must not be blank")
	}
	return errs
}

// ── Portfolio ──────────────────────────────────────────────────────────────────

// PortfolioConfig groups broker-direct portfolio settings (Phase 18).
type PortfolioConfig struct {
	Poller PollerConfig `yaml:"poller"`
	Cache  CacheConfig  `yaml:"cache"`
}

// PollerConfig controls the independent portfolio snapshot goroutine that
// writes rows to portfolio_snapshots regardless of engine tick cadence.
type PollerConfig struct {
	Enabled              bool    `yaml:"enabled"`
	IntervalSec          int     `yaml:"interval_sec"`           // seconds between polls (default 60)
	SignificantChangePct float64 `yaml:"significant_change_pct"` // extra snapshot when equity moves by this % (default 0.05)
}

// CacheConfig controls the web-layer broker TTL cache.
type CacheConfig struct {
	AccountTTLSec   int `yaml:"account_ttl_sec"`   // default 3
	PositionsTTLSec int `yaml:"positions_ttl_sec"` // default 3
	HistoryTTLSec   int `yaml:"history_ttl_sec"`   // default 30
	OrdersTTLSec    int `yaml:"orders_ttl_sec"`    // default 10
}

// ORBConfig is the shared scheduler input assembled from a concrete ORB
// strategy configuration. It is intentionally not a top-level YAML section.
type ORBConfig struct {
	Enabled        bool
	MaxCandidates  int
	RangeMinutes   int
	RiskReward     float64
	MaxRiskPct     float64
	MaxPositionPct float64
}

// CostConfig controls the user-facing cost profile and budget guardrails.
type CostConfig struct {
	// Profile is the active cost profile: "budget" | "balanced" | "pro".
	// Profile defaults are applied at startup before explicit config values,
	// so any explicit value in this file always takes precedence.
	// Default: "balanced".
	Profile string `yaml:"profile"`

	// DailyTokenBudget is the maximum tokens allowed per calendar day across
	// all model calls. 0 disables the daily cap. Default: 200000.
	DailyTokenBudget int `yaml:"daily_token_budget"`

	// WeeklyTokenBudget is the maximum tokens allowed per calendar week. 0
	// disables the weekly cap. Default: 1200000.
	WeeklyTokenBudget int `yaml:"weekly_token_budget"`

	// AutoDegradeOnBudgetHit: when true, crossing a budget cap steps the
	// active profile down one tier (pro → balanced → budget) and sends a
	// Telegram notification immediately (bypasses the batcher).
	AutoDegradeOnBudgetHit bool `yaml:"auto_degrade_on_budget_hit"`

	// CacheTTLMin is the analysis cache TTL in minutes. Set by ApplyProfile
	// (180/120/60 for budget/balanced/pro) and overridable per-user.
	CacheTTLMin int `yaml:"cache_ttl_min"`
}

// ── Embeddings ─────────────────────────────────────────────────────────────────
//
// Embedding dimension decision (Phase 11):
//
//   Provider "openai" uses text-embedding-3-small (1536 dimensions).
//   Provider "ollama" uses nomic-embed-text (768 dimensions).
//
//   The vector columns in migrations/003_market_data.sql, 004_knowledge_graph.sql,
//   and 005_analysis.sql are all created as vector(1536), matching the OpenAI model.
//
//   To switch providers and re-dimension:
//     1. Update EmbeddingsConfig.Provider and EmbeddingsConfig.Dimension.
//     2. Write a new migration: ALTER TABLE … ALTER COLUMN embedding TYPE vector(N).
//     3. Run `te embeddings reindex` to regenerate all stored embeddings.

type EmbeddingsConfig struct {
	// Enabled controls whether the background embedding worker runs at all.
	Enabled bool `yaml:"enabled"`

	// Provider selects the embedding backend: "openai" (default) or "ollama".
	Provider string `yaml:"provider"`

	// Dimension is the vector size produced by the chosen model.
	// Must match the vector() column size in the migrations.
	// Default: 1536 (text-embedding-3-small). Ollama nomic-embed-text: 768.
	Dimension int `yaml:"dimension"`

	// APIKeyEnv is the environment variable name holding the OpenAI API key.
	// Only required when provider = "openai".
	APIKeyEnv string `yaml:"api_key_env"`
	APIKey    string `yaml:"-" json:"-"` // resolved from APIKeyEnv at load time

	// OllamaBaseURL is the Ollama server URL (default: http://localhost:11434).
	OllamaBaseURL string `yaml:"ollama_base_url"`

	// Model overrides the default embedding model for the chosen provider.
	// Leave empty to use the provider default.
	Model string `yaml:"model"`

	// WorkerIntervalSec is how often the background worker polls for unembedded rows.
	// Default: 30 seconds.
	WorkerIntervalSec int `yaml:"worker_interval_sec"`

	// BatchSize is the max number of rows the worker processes per poll cycle.
	// Default: 50.
	BatchSize int `yaml:"batch_size"`

	// SimilarAnalysesEnabled gates the "Similar Past Setups" section in prompts.
	SimilarAnalysesEnabled bool `yaml:"similar_analyses_enabled"`

	// SimilarAnalysesThreshold is the minimum cosine similarity score (0–1) for
	// a past analysis to be included. Default: 0.80.
	SimilarAnalysesThreshold float64 `yaml:"similar_analyses_threshold"`

	// SimilarAnalysesLimit caps how many past analyses are injected per prompt.
	// Default: 3.
	SimilarAnalysesLimit int `yaml:"similar_analyses_limit"`
}

// ── Engine ────────────────────────────────────────────────────────────────────

type EngineConfig struct {
	Mode            string  `yaml:"mode"`              // manual | auto | paper
	TickIntervalSec int     `yaml:"tick_interval_sec"` // default 3600 (60 min)
	MaxWatchlist    int     `yaml:"max_watchlist"`     // maximum active main-watchlist entries
	DryRun          bool    `yaml:"dry_run"`
	InitialCapital  float64 `yaml:"initial_capital"`  // starting cash for paper trading (default 100000)
	LogTimezone     string  `yaml:"log_timezone"`     // IANA timezone for display (default "UTC")
	SkipRecentTick  *bool   `yaml:"skip_recent_tick"` // skip startup tick if last analysis is within tick interval (default true)
	// AdoptBrokerOrphans lets the startup reconciler insert recovery trade rows
	// for broker positions the DB does not track (source=broker_only, or the
	// untracked surplus of a same-direction mismatch), so stops and analyst
	// exits manage them again. Stops anchor on the broker's current price.
	// Default false: drift is alert-only until an operator opts in.
	AdoptBrokerOrphans bool `yaml:"adopt_broker_orphans"`
}

// ShouldSkipRecentTick returns true (default) unless explicitly set to false.
func (c EngineConfig) ShouldSkipRecentTick() bool {
	if c.SkipRecentTick == nil {
		return true
	}
	return *c.SkipRecentTick
}

// ── Analysis (cost-reduction gates) ──────────────────────────────────────────

// AnalysisConfig controls the material-change gate and classifier (Epic 1–2).
type AnalysisConfig struct {
	// MaterialChangeOnly skips LLM analysis when no material change is detected.
	// Default: true.
	MaterialChangeOnly bool `yaml:"material_change_only"`

	// HoldCooldownMin is the minimum minutes to wait before re-analyzing a ticker
	// whose last decision was HOLD. Default: 120.
	HoldCooldownMin int `yaml:"hold_cooldown_min"`

	// RSIDeltaThreshold: minimum absolute RSI change to trigger re-analysis.
	// Default: 3.0.
	RSIDeltaThreshold float64 `yaml:"rsi_delta_threshold"`

	// EMAGapDeltaPct: minimum change in (price-EMA20)/EMA20 percentage to trigger
	// re-analysis. Default: 0.5.
	EMAGapDeltaPct float64 `yaml:"ema_gap_delta_pct"`

	// PriceMoveThresholdPct: minimum price change percentage since last analysis
	// to trigger re-analysis. Default: 0.75.
	PriceMoveThresholdPct float64 `yaml:"price_move_threshold_pct"`

	// NewsNoveltyEnabled enables news digest hash comparison. When the hash
	// is unchanged no new articles have arrived — treated as no material change.
	// Default: true.
	NewsNoveltyEnabled bool `yaml:"news_novelty_enabled"`

	// MaxNewsArticlesPerPrompt caps the number of news snippets injected into
	// the analysis prompt. Articles are ordered newest-first; excess are dropped.
	// Default: 5.
	MaxNewsArticlesPerPrompt int `yaml:"max_news_articles_per_prompt"`

	// MaxCandlesPerPrompt caps the number of recent candle snapshots injected
	// into the analysis prompt. Default: 10.
	MaxCandlesPerPrompt int `yaml:"max_candles_per_prompt"`

	// HistoryWindowByStrategy defines the maximum candle history window for
	// each strategy type. Overrides the global historyCandles constant.
	// Keys match strategy names: trend_following, mean_reversion, momentum.
	HistoryWindowByStrategy map[string]int `yaml:"history_window_by_strategy"`

	// Classifier configures the two-stage analysis gate (PRD §7.2).
	Classifier ClassifierConfig `yaml:"classifier"`

	// AdaptiveDepth configures impact-score-driven analyst tier routing (PRD §7.3).
	AdaptiveDepth AdaptiveDepthConfig `yaml:"adaptive_depth"`

	// SpecialistModeEnabled toggles the sequential specialist path: three
	// directional roles followed by a mandatory risk-manager review.
	// When true, the engine routes signal-generation calls to
	// AnalyzeSpecialist instead of AnalyzeFull. AnalyzeFull itself is
	// unchanged. Default: false (safe-rollout).
	SpecialistModeEnabled bool `yaml:"specialist_mode_enabled"`

	// SpecialistRoles maps each specialist role name to a registry key in
	// models.registry. Recognised roles: fundamental, technical, sentiment,
	// risk_manager. All four roles are required in specialist mode; analysis
	// fails closed if a mapped provider is missing or unavailable.
	// Example: {fundamental: claude, technical: deepseek, sentiment: gemini,
	//           risk_manager: grok}
	SpecialistRoles map[string]string `yaml:"specialist_roles"`
}

// ClassifierConfig controls the two-stage classifier gate.
type ClassifierConfig struct {
	// Mode: "disabled" | "shadow" | "active". Default: "shadow".
	// shadow = classifies but does not block; active = gates analysis.
	Mode string `yaml:"mode"`

	// Layer 1 thresholds — mirror AnalysisConfig for classifier-specific tuning.
	RSIDeltaThreshold     float64 `yaml:"rsi_delta_threshold"`
	EMAGapDeltaPct        float64 `yaml:"ema_gap_delta_pct"`
	PriceMoveThresholdPct float64 `yaml:"price_move_threshold_pct"`
	HoldCooldownMin       int     `yaml:"hold_cooldown_min"`
	NewsNoveltyEnabled    bool    `yaml:"news_novelty_enabled"`

	// Layer 2 — LLM fallback for ambiguous cases.
	LLMFallbackEnabled bool   `yaml:"llm_fallback_enabled"`
	LLMFallbackModel   string `yaml:"llm_fallback_model"` // registry role key
}

// AdaptiveDepthConfig controls impact-score-driven analyst tier routing.
type AdaptiveDepthConfig struct {
	// Enabled: when false the engine always uses all configured analysts.
	Enabled bool `yaml:"enabled"`

	// LowThreshold: scores below this → depth 1 (cheapest analyst only).
	LowThreshold float64 `yaml:"low_threshold"`

	// HighThreshold: scores at or above this → depth 3 (all analysts).
	HighThreshold float64 `yaml:"high_threshold"`

	// Weights for each impact score input.
	Weights AdaptiveDepthWeights `yaml:"weights"`
}

// AdaptiveDepthWeights holds the per-signal weights summing to 1.0.
type AdaptiveDepthWeights struct {
	Volatility    float64 `yaml:"volatility"`     // ATR%
	NewsNovelty   float64 `yaml:"news_novelty"`   // binary or normalised
	RiskProximity float64 `yaml:"risk_proximity"` // distance to stop/drawdown
	TrendStrength float64 `yaml:"trend_strength"` // ADX normalised
}

// ── Models ────────────────────────────────────────────────────────────────────

// ModelConfig describes a single model endpoint.
type ModelConfig struct {
	Provider    string  `yaml:"provider"` // anthropic | openai | deepseek | google | ollama
	Model       string  `yaml:"model"`
	APIKeyEnv   string  `yaml:"api_key_env"` // env var name
	APIKey      string  `yaml:"-" json:"-"`  // resolved at runtime, never serialised
	MaxTokens   int     `yaml:"max_tokens"`
	Temperature float64 `yaml:"temperature"`
	TimeoutSec  int     `yaml:"timeout_sec"`
	// ThinkingBudget controls Gemini 2.5 "thinking" tokens. Gemini counts
	// thinking tokens inside maxOutputTokens, so unbounded thinking can starve
	// the visible answer and truncate it mid-string. Semantics (google provider):
	//   > 0  → bounded thinking; client sends maxOutputTokens = max_tokens + budget
	//   < 0  → dynamic thinking (-1); operator must size max_tokens generously
	//   == 0 → unset; no thinkingConfig sent, so the model keeps its native
	//          default. (We can't force a budget of 0 because some models —
	//          gemini-2.5-pro, gemini-3 — reject a disabled thinking budget,
	//          and an int field can't distinguish explicit 0 from omitted.)
	// Ignored by non-Gemini providers.
	ThinkingBudget int `yaml:"thinking_budget"`
	// ReasoningEffort controls how many hidden reasoning tokens OpenAI's gpt-5
	// and o-series models spend before emitting the answer. These reasoning
	// tokens count inside max_completion_tokens, so unbounded reasoning on a
	// large prompt can consume the whole budget and return an empty completion
	// (finish_reason=length). Valid values are model-dependent:
	//   gpt-5:       "minimal" | "low" | "medium" | "high"
	//   gpt-5-pro:   "high" only
	//   o1/o3/o4:    "low" | "medium" | "high"   (these reject "minimal")
	// When unset for a reasoning model the OpenAI client defaults to "low",
	// which is accepted by every family except gpt-5-pro — for gpt-5-pro the
	// field is omitted (native default) rather than sent invalid. Ignored by
	// non-reasoning OpenAI models and all other providers.
	ReasoningEffort string `yaml:"reasoning_effort"`
	// RequestsPerMinute paces outbound requests for this model to stay under a
	// provider's per-model rate quota. One rate limiter is created per Gemini
	// client instance and shared by all of that client's concurrent calls (e.g.
	// the single enricher client paces all its news-extraction goroutines).
	// Limiting is per-instance, not global: as long as each Gemini model is used
	// by only one role (analyst and enricher use different models today), this
	// keeps each model under its quota. If two roles share one model their rates
	// are ADDITIVE — give them different models or lower each RPM accordingly.
	// 0 uses the provider default (10 RPM for google). Honoured by google only.
	RequestsPerMinute int `yaml:"requests_per_minute"`
}

// ModelRegistryEntry extends ModelConfig with an optional fallback registry key.
// The fallback key is used when the primary model returns HTTP 429 or 503.
type ModelRegistryEntry struct {
	ModelConfig `yaml:",inline"`
	// Fallback is the registry key to use on 429/503. One hop only — no chains.
	Fallback string `yaml:"fallback,omitempty"`
}

// ModelRegistry is a named map of model entries. Keys are role-agnostic labels
// (e.g. "analyst_cheap", "gemini_flash") that can be referenced by ModelRoles.
type ModelRegistry map[string]ModelRegistryEntry

// ModelRoles maps system roles to registry keys.
// Each role is resolved to a ModelConfig via ModelRegistry at runtime.
type ModelRoles struct {
	// Analysts is an ordered list of registry keys used as ensemble analysts.
	// Ordering determines adaptive depth tier: [cheap, mid, frontier].
	Analysts []string `yaml:"analysts,omitempty"`
	// Arbiter is the registry key for the arbiter model.
	Arbiter string `yaml:"arbiter,omitempty"`
	// Classifier is the registry key for the two-stage classifier LLM.
	Classifier string `yaml:"classifier,omitempty"`
	// NewsEnricher is the registry key for knowledge-graph entity extraction.
	NewsEnricher string `yaml:"news_enricher,omitempty"`
	// Embedder is the registry key for the embedding model.
	Embedder string `yaml:"embedder,omitempty"`
}

// ModelsConfig holds all model configuration.
// When models.registry + models.roles are present they take precedence.
// Legacy analysts[]/arbiter/primary fields are kept for backward compatibility
// and are auto-migrated to analysts[] on load if registry is absent.
type ModelsConfig struct {
	// Registry declares all named model entries used anywhere in the system.
	Registry ModelRegistry `yaml:"registry,omitempty"`
	// Roles maps system roles to registry keys.
	Roles ModelRoles `yaml:"roles,omitempty"`

	// Phase 12 fields (legacy — used when registry is absent).
	Analysts []ModelConfig `yaml:"analysts,omitempty"`
	Arbiter  ModelConfig   `yaml:"arbiter,omitempty"`

	// Legacy fields (pre-Phase 12) — kept for backward compatibility.
	Primary   ModelConfig   `yaml:"primary,omitempty"`
	Secondary []ModelConfig `yaml:"secondary,omitempty"`

	Consensus ConsensusConfig `yaml:"consensus"`
}

// Resolve returns the ModelConfig for the given registry key.
// Returns an error if the key is not found in the registry.
func (m *ModelsConfig) Resolve(key string) (ModelConfig, error) {
	if m.Registry == nil {
		return ModelConfig{}, fmt.Errorf("models.registry is not configured (key %q)", key)
	}
	entry, ok := m.Registry[key]
	if !ok {
		return ModelConfig{}, fmt.Errorf("registry key %q not found in models.registry", key)
	}
	return entry.ModelConfig, nil
}

// ResolveRole returns the ModelConfig for the given role name (e.g. "classifier").
// It looks up the role in models.roles, then resolves the key in models.registry.
func (m *ModelsConfig) ResolveRole(role string) (ModelConfig, error) {
	key := m.roleKey(role)
	if key == "" {
		return ModelConfig{}, fmt.Errorf("role %q is not defined in models.roles", role)
	}
	return m.Resolve(key)
}

// ResolveFallback returns the fallback ModelConfig for a registry key, if one is defined.
// Used by model clients when the primary model returns HTTP 429 or 503.
func (m *ModelsConfig) ResolveFallback(key string) (ModelConfig, bool) {
	if m.Registry == nil {
		return ModelConfig{}, false
	}
	entry, ok := m.Registry[key]
	if !ok || entry.Fallback == "" {
		return ModelConfig{}, false
	}
	fallbackEntry, ok := m.Registry[entry.Fallback]
	if !ok {
		return ModelConfig{}, false
	}
	return fallbackEntry.ModelConfig, true
}

// RegistryKeyForRole returns the registry key for a given role name.
func (m *ModelsConfig) RegistryKeyForRole(role string) string {
	return m.roleKey(role)
}

func (m *ModelsConfig) roleKey(role string) string {
	switch role {
	case "arbiter":
		return m.Roles.Arbiter
	case "classifier":
		return m.Roles.Classifier
	case "news_enricher":
		return m.Roles.NewsEnricher
	case "embedder":
		return m.Roles.Embedder
	default:
		return ""
	}
}

// AnalystConfigs returns the ordered list of analyst ModelConfigs.
// Uses registry roles when available; falls back to legacy Analysts[] field.
func (m *ModelsConfig) AnalystConfigs() []ModelConfig {
	if m.Registry != nil && len(m.Roles.Analysts) > 0 {
		out := make([]ModelConfig, 0, len(m.Roles.Analysts))
		for _, key := range m.Roles.Analysts {
			if entry, ok := m.Registry[key]; ok {
				out = append(out, entry.ModelConfig)
			} else {
				// Preserve slice length even on registry miss to alert downstream.
				out = append(out, ModelConfig{})
			}
		}
		return out
	}
	return m.Analysts
}

// AnalystNodeID returns the stable runtime/topology ID for an analyst slot.
// Registry roles provide semantic IDs. Legacy configurations retain
// llm_analysis for the primary slot because single-model and adaptive depth-1
// execution both publish against that ID.
func (m *ModelsConfig) AnalystNodeID(index int) string {
	if index >= 0 && index < len(m.Roles.Analysts) {
		return "analyst-" + m.Roles.Analysts[index]
	}
	if index <= 0 {
		return "llm_analysis"
	}
	return fmt.Sprintf("analyst-%d", index)
}

// ArbiterConfig returns the arbiter ModelConfig.
// Uses registry roles when available; falls back to legacy Arbiter field.
func (m *ModelsConfig) ArbiterConfig() ModelConfig {
	if m.Registry != nil && m.Roles.Arbiter != "" {
		if entry, ok := m.Registry[m.Roles.Arbiter]; ok {
			return entry.ModelConfig
		}
	}
	return m.Arbiter
}

type ConsensusConfig struct {
	Enabled bool `yaml:"enabled"`

	// Phase 12: fast-path fires when all analysts agree, minConf & spread met.
	FastPathConfidence float64 `yaml:"fast_path_confidence"` // default 0.80
	FastPathMaxSpread  float64 `yaml:"fast_path_max_spread"` // default 0.15

	// Legacy field.
	Strategy string `yaml:"strategy,omitempty"` // majority | unanimous | primary-with-veto
}

// ── Markets ───────────────────────────────────────────────────────────────────

type MarketsConfig struct {
	Stocks StocksConfig  `yaml:"stocks"`
	IBKR   IBKRConfig    `yaml:"ibkr"`
	News   NewsAPIConfig `yaml:"news"`
	OANDA  OANDAConfig   `yaml:"oanda"`

	Dukascopy DukascopyConfig `yaml:"dukascopy"`
	HistData  HistDataConfig  `yaml:"histdata"`
	Exness    ExnessConfig    `yaml:"exness"`
	// CandleSanity rejects corrupt provider bars before they reach the DB or
	// the trailing manager (issue #519). Thresholds are calibrated in
	// internal/data/sanity.go; leaving this block unset keeps those defaults.
	CandleSanity CandleSanityConfig `yaml:"candle_sanity"`
	// BacktestProvider selects the ORB backtest data source: "histdata" (default; free bulk
	// tick ZIPs, no account/key/throttle), "dukascopy", "exness", or "oanda". Overridable
	// per-run with `te backtest orb --provider`.
	BacktestProvider string `yaml:"backtest_provider"`
}

// CandleSanityConfig mirrors data.CandleSanityConfig so the config package
// stays a leaf (stdlib + yaml only) and never depends on internal/data.
// Converted at wiring time; see cmd/te applyCandleSanity.
//
// Thresholds are calibrated against 4,440 real 1h bars — see the commentary in
// internal/data/sanity.go before changing them. Zero values fall back to those
// defaults rather than disabling the guard.
type CandleSanityConfig struct {
	// Enabled gates only the intraday range check. Structural validation
	// (finite, positive, internally ordered OHLC) always runs — those bars are
	// unambiguously corrupt and there is no legitimate reason to store one.
	Enabled *bool `yaml:"enabled"`
	// MaxRangeRatio caps an intraday bar's own high-low span in multiples of
	// the window's median range.
	MaxRangeRatio float64 `yaml:"max_range_ratio"`
	// MaxRangePct is that same span as a percentage of the prior close. A bar
	// must breach BOTH this and MaxRangeRatio to be rejected.
	MaxRangePct float64 `yaml:"max_range_pct"`
	// ScaleWindow is how many prior bars form the median-range scale.
	ScaleWindow int `yaml:"scale_window"`
	// MaxConsecutiveRejections is how many rejections in a row are
	// reinterpreted as a genuine re-rating and restored.
	MaxConsecutiveRejections int `yaml:"max_consecutive_rejections"`
}

// CandleSanityEnabled reports whether the continuity check runs, defaulting to
// true when the operator has not set the key. A plain bool would make an
// omitted key indistinguishable from an explicit `false`, silently disabling
// the guard for every existing config file.
func (c CandleSanityConfig) CandleSanityEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// HistDataConfig configures the HistData.com historical tick provider — the preferred
// zero-credential source for the ORB backtest engine: free per-month tick ZIPs with no
// account, API key, or rate limit. Covers EUR/USD, GBP/USD, XAU/USD, XAG/USD back to 2023.
// Data-only; live trading does not use it.
type HistDataConfig struct {
	BaseURL string `yaml:"base_url"` // optional site override (default HistDataBaseURL)
	// CacheDir is the on-disk cache root. Each instrument-month ZIP is downloaded at most once,
	// ever, then served from disk. Empty → default ~/.tradeengine/histdata-cache; "off" disables.
	CacheDir string `yaml:"cache_dir"`
}

// ExnessConfig configures a local Exness MetaTrader OHLC export directory. Data-only; live
// trading and cost assumptions still come from the configured broker/execution model.
type ExnessConfig struct {
	// DataDir contains yearly XAUUSD_M1_2024_exness.csv files or consolidated
	// XAUUSD_M1_YYYYMMDDHHMM_YYYYMMDDHHMM.csv range exports. Empty defaults to data.
	DataDir string `yaml:"data_dir"`
}

// DukascopyConfig configures the Dukascopy historical tick-data provider — the default,
// zero-credential source for the ORB backtest engine (no account or API key; hourly .bi5
// tick files served as static objects). Data-only; live trading does not use it.
type DukascopyConfig struct {
	BaseURL string `yaml:"base_url"` // optional feed override (default DukascopyBaseURL)
	// CacheDir is the on-disk cache root. Each hour is fetched from the (rate-limited) feed at
	// most once, ever, then served from disk — so repeated backtests never re-fetch and never
	// re-trigger throttling. Empty → default ~/.tradeengine/dukascopy-cache; "off" disables.
	CacheDir string `yaml:"cache_dir"`
	// PointScale optionally overrides the integer→price divisor per ticker (e.g. XAU/USD).
	// Defaults: 5-decimal FX ÷1e5, JPY pairs, metals and …IDXUSD index CFDs ÷1e3.
	PointScale map[string]float64 `yaml:"point_scale"`
	// FetchConcurrency bounds in-flight hour fetches (0 → default 6). The free feed's
	// burst tolerance varies; when a cache-fill run drowns in 503s, lower this to 1.
	FetchConcurrency int `yaml:"fetch_concurrency"`
	// MinRequestIntervalMs spaces request STARTS globally across all workers (0 → no
	// spacing). The 2026 feed throttles bursts hard (~1 request per 3-5s tolerated from a
	// cold IP); 3000-5000 with fetch_concurrency 1 makes a first cache fill slow but
	// reliable. Cached hours are never re-fetched, so this only taxes the first run.
	MinRequestIntervalMs int `yaml:"min_request_interval_ms"`
}

// OANDAConfig configures the OANDA v20 REST data provider used by the ORB backtest
// engine. The practice (demo) environment is free, needs no funding, and on a non-US
// OANDA entity serves metals (XAU/USD, XAG/USD) — making it the backtest's source of
// deep 1-minute forex/metals history. Live trading does not use this; it is data-only.
type OANDAConfig struct {
	Enabled   bool   `yaml:"enabled"`
	TokenEnv  string `yaml:"token_env"`  // env var holding the v20 API token (e.g. OANDA_API_TOKEN)
	Token     string `yaml:"-" json:"-"` // resolved from TokenEnv at load time
	AccountID string `yaml:"account_id"` // OANDA account id (for the pricing endpoint; optional for candles)
	Practice  bool   `yaml:"practice"`   // true → practice base URL (default); false → live
	BaseURL   string `yaml:"base_url"`   // optional explicit override
}

// IBKRConfig configures the Interactive Brokers Client Portal gateway connection.
type IBKRConfig struct {
	Enabled     bool   `yaml:"enabled"`
	AccountID   string `yaml:"account_id"`
	GatewayURL  string `yaml:"gateway_url"`  // e.g. "https://localhost:5000"
	TickleSec   int    `yaml:"tickle_sec"`   // keepalive interval (default 240)
	InsecureTLS bool   `yaml:"insecure_tls"` // IBKR gateway uses self-signed certs

	// AuthMonitorSec is how often the auth monitor polls /iserver/auth/status.
	// Default: 60 seconds.
	AuthMonitorSec int `yaml:"auth_monitor_sec"`
	// AuthAlertDebounceMin is the minimum minutes between repeated "session
	// expired" Telegram alerts. Prevents spam while the user is re-authenticating.
	// Default: 30 minutes.
	AuthAlertDebounceMin int `yaml:"auth_alert_debounce_min"`
}

// Instrument defines a tradeable instrument for forex/CFD ORB.
type Instrument struct {
	Symbol     string  `yaml:"symbol"`       // e.g. "EUR/USD", "XAU/USD"
	SecType    string  `yaml:"sec_type"`     // "CASH" (forex) or "CFD" (metals)
	MinLotSize int     `yaml:"min_lot_size"` // e.g. 20000 for EUR/USD, 1 for XAU/USD
	PipBuffer  float64 `yaml:"pip_buffer"`   // e.g. 0.0002 for EUR/USD, 0.50 for XAU/USD

	// Strategy selects the live entry model for this instrument. Empty/"orb" keeps the
	// existing opening-range breakout path. Other values select independently validated
	// intraday strategy families: trend_continuation, sweep_reclaim, or opening_shock.
	Strategy string `yaml:"strategy"`

	// RangeMinutes overrides the strategy-level opening-range length for this instrument.
	// Primarily used by per-instrument futures research candidates. 0 inherits the
	// strategy-level range_minutes.
	RangeMinutes int `yaml:"range_minutes"`

	// SideMode restricts single-entry strategy families to both sides, long-only, or
	// short-only. Empty/"both" allows both. Used by trend_continuation, sweep_reclaim,
	// and opening_shock; ignored by legacy dual-sided ORB.
	SideMode string `yaml:"side_mode"`

	// MaxHoldMinutes is the per-instrument hard time limit on a *filled* ORB trade.
	// When a filled position is older than this, the scheduler cancels its remaining
	// bracket legs, flattens at market, and records exit_type='time_exit'. This caps
	// the multi-day-hold tail that dominated realized losses. 0 inherits the
	// strategy-level ForexORBConfig.MaxHoldMinutes; when both are 0 time exits are
	// disabled for this instrument and trades run until their bracket resolves.
	MaxHoldMinutes int `yaml:"max_hold_minutes"`

	// Entry-quality filters (Phase 2). All default to 0 = disabled, so an instrument
	// trades every breakout until it opts in. range_atr_ratio = opening_range_width /
	// ATR(atr_period, atr_timeframe).

	// MinRangeATR / MaxRangeATR bound the opening range's width relative to recent
	// volatility: too narrow is noise easily wicked through; too wide means much of the
	// move is already spent. A session whose ratio falls outside [min, max] is skipped.
	// A bound of 0 disables that side (e.g. MaxRangeATR=0 means no upper bound).
	MinRangeATR float64 `yaml:"min_range_atr"`
	MaxRangeATR float64 `yaml:"max_range_atr"`

	// MaxSpreadToRisk rejects a session when the current bid/ask spread is too large
	// relative to the planned stop distance: spread / |entry_stop - stop_loss|. 0 disables.
	MaxSpreadToRisk float64 `yaml:"max_spread_to_risk"`

	// CloseFracThreshold (Phase 2) gates each breakout side on where price closed within the
	// opening range at lock (OpeningRange.CloseFrac, 0 = at the low, 1 = at the high). It is a
	// direction-aware guard, not a whole-session skip: the buy side is kept only when
	// close_frac >= threshold (price finished the range near its top), the sell side only when
	// close_frac <= 1 - threshold (near its bottom). With a threshold in (0.5, 1) a mid-range
	// close falls in the [1-threshold, threshold] dead zone and BOTH sides are disabled, so the
	// session is skipped as directionless chop. A side zeroed here is skipped by the placement
	// loop, mirroring direction eligibility. 0 (default) disables the guard: buy needs >= 0 and
	// sell needs <= 1, both always true. Sensible live values are 0.55–0.70.
	CloseFracThreshold float64 `yaml:"close_frac_threshold"`

	// BreakoutConfirmation (Phase 3) decides what counts as a breakout before entering:
	//   "" | "stop_touch" — current behaviour: a resting stop fills the instant price
	//                       touches the breakout level (fills on a wick).
	//   "close_1m"        — require one 1-minute close beyond the breakout level.
	//   "close_3m"        — require three consecutive 1-minute closes beyond it (the
	//                       breakout must hold ~3 minutes), filtering false breakouts.
	//   "retest_swing"    — wait for the breakout to pull back to the range level and bounce,
	//                       then enter; stop just beyond the retest swing (tighter → better R:R).
	//   "retest_range"    — same retest entry, but stop at the opposite side of the opening range.
	// Confirmation trades enter later and at a worse price than a stop touch, in exchange
	// for a stronger signal. Default stop_touch preserves current behaviour.
	BreakoutConfirmation string `yaml:"breakout_confirmation"`

	// RiskReward overrides ForexORBConfig.RiskReward for this instrument when > 0.
	// Useful when one instrument's validated target differs from the strategy default.
	RiskReward float64 `yaml:"risk_reward"`

	// Trend-continuation parameters. Used only when Strategy is "trend_continuation".
	// Range filters are measured in pip_buffer units; exclude bands use "min:max" with
	// max exclusive, e.g. "32:60" skips widths in [32,60).
	ContinuationDelayMinutes       int     `yaml:"continuation_delay_minutes"`
	ContinuationConfirmBars        int     `yaml:"continuation_confirm_bars"`
	ContinuationDepthMult          float64 `yaml:"continuation_depth_mult"`
	ContinuationTrendFilter        string  `yaml:"continuation_trend_filter"` // none | aligned | contrarian
	ContinuationTrendLookbackMin   int     `yaml:"continuation_trend_lookback_minutes"`
	ContinuationMinRangePips       float64 `yaml:"continuation_min_range_pips"`
	ContinuationMaxRangePips       float64 `yaml:"continuation_max_range_pips"`
	ContinuationExcludeRangePips   string  `yaml:"continuation_exclude_range_pips"`
	ContinuationMonthlyStopLossUSD float64 `yaml:"continuation_monthly_stop_loss"`

	// Sweep/reclaim parameters. Used only when Strategy is "sweep_reclaim".
	SweepReclaimLevelSet           string  `yaml:"sweep_reclaim_level_set"` // prior_day | overnight
	SweepReclaimDepthMult          float64 `yaml:"sweep_reclaim_depth_mult"`
	SweepReclaimEntryWindowMinutes int     `yaml:"sweep_reclaim_entry_window_minutes"`

	// Opening-shock parameters. Used only when Strategy is "opening_shock".
	OpeningShockWindowMinutes int     `yaml:"opening_shock_window_minutes"`
	OpeningShockLookback      int     `yaml:"opening_shock_lookback"`
	OpeningShockPercentile    float64 `yaml:"opening_shock_percentile"`
	OpeningShockStopMult      float64 `yaml:"opening_shock_stop_mult"`
	OpeningShockEntryDelayMin int     `yaml:"opening_shock_entry_delay_minutes"`

	// Retest parameters (Phase 6), used only by the retest_* confirmation modes. RetestTolerance
	// is how close (as a fraction of the opening-range width) the pullback must return to the
	// breakout level to count as a retest; RetestTimeoutMinutes bounds how long after the breakout
	// the bounce entry may occur. Zero values fall back to sensible defaults in the engine.
	RetestTolerance      float64 `yaml:"retest_tolerance"`
	RetestTimeoutMinutes int     `yaml:"retest_timeout_minutes"`

	// DirectionEligibility (Phase 3) makes each breakout direction EARN its bracket: with
	// it on, only the side aligned with the trend (price vs the trend-timeframe EMA) is
	// placed, instead of blind dual-sided brackets that let a single wick pick direction.
	// false (default) keeps dual-sided. See ForexORBConfig.TrendTimeframe / TrendEMAPeriod.
	DirectionEligibility bool `yaml:"direction_eligibility"`

	// MarginRate is the broker's margin requirement for this instrument as a fraction of
	// notional (e.g. 0.033 ≈ 30:1 leverage for a forex major, 0.05 for a gold CFD). When set,
	// the ForexORBConfig.MaxPositionPct cap limits MARGIN CONSUMED (notional × margin_rate)
	// instead of full notional — without it, a leveraged forex position is capped to min-lot
	// territory where the broker's minimum commission dwarfs the dollar risk per trade
	// (sweep 9af76e63: fixed ~$4 round-trip on ~$2-15 risk turned TP hits into net losses).
	// <= 0 keeps the current full-notional behaviour. Applied in sizeForexQty, so it affects
	// live and backtest sizing identically.
	MarginRate float64 `yaml:"margin_rate"`

	// MaxNotionalUSD caps this instrument's order value (entry × qty), in account currency,
	// applied AFTER the risk- and margin-based sizing in sizeForexQty. A tight ORB stop on a
	// high-priced instrument (silver ~$62) lets the margin cap size a small-dollar-risk trade
	// into a large notional that trips the broker's precautionary order-value limit (IBKR
	// default $100k) — a cascade of confirmation prompts that fails placement outright. This
	// bounds notional so the order clears that limit. <= 0 disables the cap (default), leaving
	// every other instrument byte-identical. Applied in sizeForexQty (live + backtest parity).
	// entry×qty is denominated in the pair's QUOTE currency, which equals USD only for USD-quoted
	// pairs — Validate rejects this field on any non-…/USD symbol.
	MaxNotionalUSD float64 `yaml:"max_notional_usd"`

	// PointValue is the dollar value of a 1.0 price-point move per unit/contract. Forex and
	// CFD units are $1/point (the default when <= 0), so existing instruments are unaffected.
	// Exchange-listed futures move more than $1 per point per contract (MES $5, MNQ $2,
	// MYM $0.50) — the backtest CLI sets this from tick_value/tick_size when mapping a
	// FuturesInstrument onto this struct. Applied in sizeForexQty (dollar risk and notional
	// are price × point_value) and in the orbreplay engine's P&L/R/commission math.
	PointValue float64 `yaml:"point_value"`

	// TickSize is the instrument's minimum price increment. 0 (the forex/CFD default) means
	// prices are not tick-snapped. Futures set it (from FuturesInstrument.TickSize) so bracket
	// entry/stop/target land on tradeable ticks — an off-tick futures price is rejected by the
	// exchange. Applied by snapToTick in ComputeForexBracket / ScaleOutBrackets / retest.
	TickSize float64 `yaml:"tick_size"`

	// CommissionPerSide is the estimated broker commission in account currency per fill
	// (a bracket round trip pays 2×). Set it to max(broker minimum, bps × typical notional)
	// for this instrument's size regime. Used by the commission/risk entry gate below and,
	// when > 0, as the backtest cost model's commission for this instrument. 0 disables the
	// gate and leaves the backtest on its conservative default.
	CommissionPerSide float64 `yaml:"commission_per_side"`

	// CommissionBpsPerSide is an estimated broker commission rate in basis points of fill
	// notional per side. This is the accurate model for IBKR London metals: XAGUSD is 3.0
	// bps/side and XAUUSD is 1.5 bps/side, subject to CommissionMinPerSide. The flat
	// CommissionPerSide remains available for brokers/instruments charged in dollars per fill.
	CommissionBpsPerSide float64 `yaml:"commission_bps_per_side"`

	// CommissionMinPerSide is the minimum commission per fill when CommissionBpsPerSide is
	// set. For IBKR London metals this is USD 2.00. 0 disables the minimum.
	CommissionMinPerSide float64 `yaml:"commission_min_per_side"`

	// MaxCommissionToRisk rejects a session when the estimated round-trip commission is too
	// large relative to the planned DOLLAR risk. The spread gate (max_spread_to_risk) can't
	// catch this: spread scales with position size while commission has a fixed floor or a
	// bps-on-notional charge. 0 disables. Requires a flat or bps commission estimate.
	MaxCommissionToRisk float64 `yaml:"max_commission_to_risk"`

	// RiskMultiplier (Phase 5) scales this instrument's per-trade risk relative to the
	// strategy MaxRiskPct, so a higher-tail-risk instrument (e.g. XAU/USD) can be sized
	// down without disabling it. <= 0 is treated as 1.0 (no change). Applied in
	// ComputeForexBracket, so it affects live and backtest sizing identically.
	RiskMultiplier float64 `yaml:"risk_multiplier"`

	// Adaptive exit management (Phase 4). All default 0 = disabled, preserving the fixed
	// bracket. R is the planned risk = |entry_stop - stop_loss|; "favourable R" is how far
	// in profit the trade has gone. Currently modelled by the backtest `adaptive` variant;
	// the live order-management counterpart lands in a follow-up.
	BreakEvenR         float64 `yaml:"break_even_r"`          // move the stop to entry once this favourable R is reached
	TrailATRMult       float64 `yaml:"trail_atr_mult"`        // trail the stop at high-water-mark − this × ATR (ratchet up)
	TrailAfterR        float64 `yaml:"trail_after_r"`         // begin trailing only after this favourable R
	PartialTakeProfitR float64 `yaml:"partial_take_profit_r"` // take a partial at this favourable R
	PartialFraction    float64 `yaml:"partial_fraction"`      // fraction (0..1) of the position to close at the partial
	RunnerTakeProfitR  float64 `yaml:"runner_take_profit_r"`  // runner's target R after the partial (0 keeps the original target)
}
type ForexORBConfig struct {
	Enabled        bool         `yaml:"enabled"`
	Instruments    []Instrument `yaml:"instruments"`
	Session        string       `yaml:"session"`       // "london" | "newyork" | "both"
	Timezone       string       `yaml:"timezone"`      // e.g. "UTC" (default)
	RangeMinutes   int          `yaml:"range_minutes"` // 15 or 30
	RiskReward     float64      `yaml:"risk_reward"`
	MaxRiskPct     float64      `yaml:"max_risk_pct"`
	MaxPositionPct float64      `yaml:"max_position_pct"`

	// MaxHoldMinutes is the strategy-level default time limit on a filled ORB trade,
	// applied to any instrument that does not set its own Instrument.MaxHoldMinutes.
	// 0 leaves time exits disabled unless an instrument opts in. See Instrument.MaxHoldMinutes.
	MaxHoldMinutes int `yaml:"max_hold_minutes"`

	// ATRPeriod / ATRTimeframe define the ATR used by the range-quality filter
	// (range_atr_ratio). Defaults: period 14, timeframe "1h" — an hourly ATR against
	// which a 15-30 min opening range yields ratios in the doc's 0.25-1.25 band.
	ATRPeriod    int    `yaml:"atr_period"`
	ATRTimeframe string `yaml:"atr_timeframe"` // "1h" | "1d" | etc.

	// TrendTimeframe / TrendEMAPeriod define the EMA used by the direction-eligibility
	// trend filter (see Instrument.DirectionEligibility). Defaults: "1h", period 20.
	TrendTimeframe string `yaml:"trend_timeframe"`
	TrendEMAPeriod int    `yaml:"trend_ema_period"`

	// ReconcileOrphanFills, when true, lets the scheduler record an entry fill from
	// broker truth (positions) when GetOrderStatus can't confirm it — the metals
	// blind spot where a bracket fills at IBKR but the parent order id ages out of
	// the orders/trades endpoints, leaving the row 'pending' and the position
	// unmanaged (no stop/TP manager engages until status='filled'). It marks the
	// matching pending entry filled and cancels the opposing OCO leg. Opt-in
	// because it mutates real-money order/trade state from position inference.
	ReconcileOrphanFills bool `yaml:"reconcile_orphan_fills"`
}

// CurrencyStrengthConfig controls the London-session FX currency-strength
// rotation research setup. It is intentionally separate from forex_orb: this
// strategy ranks a basket at fixed times, enters at market, and exits by time
// instead of managing opening-range brackets.
type CurrencyStrengthConfig struct {
	Enabled bool   `yaml:"enabled"`
	Mode    string `yaml:"mode"` // "shadow" | "paper"; "live" is rejected until live risk controls exist

	RankInstruments  []string `yaml:"rank_instruments"`  // pairs used to rank currency strength
	TradeInstruments []string `yaml:"trade_instruments"` // pairs eligible for orders

	Timezone       string  `yaml:"timezone"`         // IANA timezone for the configured HH:MM times
	StrengthStart  string  `yaml:"strength_start"`   // HH:MM, start of measurement window
	StrengthEnd    string  `yaml:"strength_end"`     // HH:MM, rank/entry decision time
	ExitTime       string  `yaml:"exit_time"`        // HH:MM, fixed flatten time
	MinGap         float64 `yaml:"min_gap"`          // minimum strongest-vs-weakest score gap
	BroadMin       int     `yaml:"broad_min"`        // minimum broad pair confirmations
	ConfirmMinMove float64 `yaml:"confirm_min_move"` // minimum signed pair move before a trade is allowed
	Units          int     `yaml:"units"`            // base-currency units per signal in shadow/paper mode
}

// EffectiveMaxHoldMinutes returns the resolved max-hold limit for a symbol:
// the instrument's own value when set, otherwise the strategy-level default.
// 0 means time exits are disabled for that symbol. An unknown symbol falls back
// to the strategy default so a stray filled position is still capped.
func (c ForexORBConfig) EffectiveMaxHoldMinutes(symbol string) int {
	for _, inst := range c.Instruments {
		if inst.Symbol == symbol {
			if inst.MaxHoldMinutes > 0 {
				return inst.MaxHoldMinutes
			}
			break
		}
	}
	return c.MaxHoldMinutes
}

// FuturesInstrument defines a single exchange-listed futures contract for ORB.
type FuturesInstrument struct {
	Symbol      string  `yaml:"symbol"`       // e.g. "ES", "NQ", "GC"
	Exchange    string  `yaml:"exchange"`     // e.g. "CME", "CBOT", "COMEX"
	AssetClass  string  `yaml:"asset_class"`  // "equity_index" | "metal" | "energy"
	ExpiryCycle string  `yaml:"expiry_cycle"` // "quarterly" | "monthly"
	TickSize    float64 `yaml:"tick_size"`    // minimum price increment (e.g. 0.25 for ES)
	TickValue   float64 `yaml:"tick_value"`   // dollar value per tick (e.g. $12.50 for ES)
	TickBuffer  int     `yaml:"tick_buffer"`  // ticks of buffer above/below range high/low
	// MarginRate makes max_position_pct cap MARGIN CONSUMED (notional × margin_rate)
	// instead of full contract notional — the same lever forex_orb uses. Futures are
	// leveraged (one MES controls ~$28k of notional on ~$2.6k overnight margin), so a
	// full-notional cap sizes to zero contracts on a small account. 0/unset = cap on
	// full notional (the pre-margin behaviour). ESTIMATE — refine against IBKR margin.
	MarginRate float64 `yaml:"margin_rate"`

	// ── Strategy fields shared with forex Instrument ─────────────────────────────
	// These make each futures contract carry the same per-instrument strategy config a
	// forex Instrument does, so futures can route through the shared dual-sided + gated +
	// confirmation path instead of the long-only gap-sign path. All default 0/"" = off,
	// preserving the pre-existing behaviour for a contract that configures none of them.

	// MaxHoldMinutes: per-contract time limit on a filled trade (0 inherits the
	// FuturesORBConfig.MaxHoldMinutes strategy default; both 0 = time exits disabled).
	MaxHoldMinutes int `yaml:"max_hold_minutes"`

	// Strategy selects the live entry model for this contract. Empty/"orb" keeps the
	// opening-range breakout path. Other values select independently validated strategy
	// families: trend_continuation, sweep_reclaim, or opening_shock.
	Strategy string `yaml:"strategy"`

	// RangeMinutes overrides FuturesORBConfig.RangeMinutes for this contract. 0 inherits.
	RangeMinutes int `yaml:"range_minutes"`

	// SideMode restricts single-entry strategy families to both, long-only, or short-only.
	SideMode string `yaml:"side_mode"`

	// RiskReward overrides FuturesORBConfig.RiskReward for this contract when > 0.
	RiskReward float64 `yaml:"risk_reward"`

	// Entry-quality gates (Phase 2). range_atr_ratio = opening_range_width / ATR. A session
	// whose ratio is outside [MinRangeATR, MaxRangeATR] is skipped; a bound of 0 disables
	// that side. MaxSpreadToRisk rejects a session when spread / |entry−stop| exceeds it.
	MinRangeATR     float64 `yaml:"min_range_atr"`
	MaxRangeATR     float64 `yaml:"max_range_atr"`
	MaxSpreadToRisk float64 `yaml:"max_spread_to_risk"`

	// BreakoutConfirmation (Phase 3): "" / "stop_touch" (resting stop, fills on a touch),
	// "close_1m" / "close_3m" (require N 1-min closes beyond the level), or "retest_swing" /
	// "retest_range" (wait for the pullback to retest the level and bounce). retest_* enter
	// via the live retest path. RetestTolerance/RetestTimeoutMinutes tune the retest_* modes.
	BreakoutConfirmation string  `yaml:"breakout_confirmation"`
	RetestTolerance      float64 `yaml:"retest_tolerance"`
	RetestTimeoutMinutes int     `yaml:"retest_timeout_minutes"`

	// Trend-continuation parameters. Used only when Strategy is "trend_continuation".
	ContinuationDelayMinutes       int     `yaml:"continuation_delay_minutes"`
	ContinuationConfirmBars        int     `yaml:"continuation_confirm_bars"`
	ContinuationDepthMult          float64 `yaml:"continuation_depth_mult"`
	ContinuationTrendFilter        string  `yaml:"continuation_trend_filter"`
	ContinuationTrendLookbackMin   int     `yaml:"continuation_trend_lookback_minutes"`
	ContinuationMinRangePips       float64 `yaml:"continuation_min_range_pips"`
	ContinuationMaxRangePips       float64 `yaml:"continuation_max_range_pips"`
	ContinuationExcludeRangePips   string  `yaml:"continuation_exclude_range_pips"`
	ContinuationMonthlyStopLossUSD float64 `yaml:"continuation_monthly_stop_loss"`

	// Sweep/reclaim parameters. Used only when Strategy is "sweep_reclaim".
	SweepReclaimLevelSet           string  `yaml:"sweep_reclaim_level_set"`
	SweepReclaimDepthMult          float64 `yaml:"sweep_reclaim_depth_mult"`
	SweepReclaimEntryWindowMinutes int     `yaml:"sweep_reclaim_entry_window_minutes"`

	// Opening-shock parameters. Used only when Strategy is "opening_shock".
	OpeningShockWindowMinutes int     `yaml:"opening_shock_window_minutes"`
	OpeningShockLookback      int     `yaml:"opening_shock_lookback"`
	OpeningShockPercentile    float64 `yaml:"opening_shock_percentile"`
	OpeningShockStopMult      float64 `yaml:"opening_shock_stop_mult"`
	OpeningShockEntryDelayMin int     `yaml:"opening_shock_entry_delay_minutes"`

	// DirectionEligibility (Phase 3): place only the trend-aligned side (price vs the
	// FuturesORBConfig trend EMA) instead of blind dual-sided brackets. false = dual-sided.
	DirectionEligibility bool `yaml:"direction_eligibility"`

	// Commission/dollar-risk gate: CommissionPerSide is the estimated commission per fill
	// (a round trip pays 2×) and MaxCommissionToRisk the max round-trip commission as a
	// fraction of planned dollar risk. 0 disables the gate; CommissionPerSide also feeds the
	// backtest cost model. RiskMultiplier scales this contract's per-trade risk (<=0 → 1.0).
	// MaxNotionalUSD caps order value (entry × qty × point_value) after sizing (0 disables).
	CommissionPerSide   float64 `yaml:"commission_per_side"`
	MaxCommissionToRisk float64 `yaml:"max_commission_to_risk"`
	RiskMultiplier      float64 `yaml:"risk_multiplier"`
	MaxNotionalUSD      float64 `yaml:"max_notional_usd"`

	// Adaptive exit management (Phase 4). All default 0 = fixed bracket. R = planned risk.
	BreakEvenR         float64 `yaml:"break_even_r"`
	TrailATRMult       float64 `yaml:"trail_atr_mult"`
	TrailAfterR        float64 `yaml:"trail_after_r"`
	PartialTakeProfitR float64 `yaml:"partial_take_profit_r"`
	PartialFraction    float64 `yaml:"partial_fraction"`
	RunnerTakeProfitR  float64 `yaml:"runner_take_profit_r"`
}

// ToInstrument maps a futures contract onto the forex Instrument shape so it can flow
// through the shared dual-sided / gated / confirmation ORB path (ComputeForexBracket,
// the entry-quality gates, direction eligibility, confirmation). This is the LIVE
// counterpart of the backtest's cmd/te buildFuturesUniverse mapping, kept in one place so
// live and backtest can't drift on how a contract becomes an Instrument.
//
//   - PipBuffer = TickBuffer × TickSize keeps entry/stop on exact tick multiples by
//     construction (r.High + buffer, r.Low − buffer).
//   - PointValue = TickValue / TickSize is the $/point multiplier ComputeForexBracket's
//     sizing and the P&L math need (MES $5, MNQ $2, MYM $0.50).
//   - TickSize is carried so snapToTick can round the take-profit (which risk×RiskReward
//     need not land on a tick) to a tradeable price.
//   - MinLotSize 1 = whole contracts.
//
// All Phase 2/3/4 strategy fields are copied straight across.
func (fi FuturesInstrument) ToInstrument() Instrument {
	pipBuffer := float64(fi.TickBuffer) * fi.TickSize
	var pointValue float64
	if fi.TickSize > 0 {
		pointValue = fi.TickValue / fi.TickSize
	}
	return Instrument{
		Symbol:                         fi.Symbol,
		SecType:                        "FUT",
		MinLotSize:                     1,
		PipBuffer:                      pipBuffer,
		PointValue:                     pointValue,
		TickSize:                       fi.TickSize,
		MarginRate:                     fi.MarginRate,
		Strategy:                       fi.Strategy,
		RangeMinutes:                   fi.RangeMinutes,
		SideMode:                       fi.SideMode,
		RiskReward:                     fi.RiskReward,
		MaxHoldMinutes:                 fi.MaxHoldMinutes,
		MinRangeATR:                    fi.MinRangeATR,
		MaxRangeATR:                    fi.MaxRangeATR,
		MaxSpreadToRisk:                fi.MaxSpreadToRisk,
		BreakoutConfirmation:           fi.BreakoutConfirmation,
		RetestTolerance:                fi.RetestTolerance,
		RetestTimeoutMinutes:           fi.RetestTimeoutMinutes,
		ContinuationDelayMinutes:       fi.ContinuationDelayMinutes,
		ContinuationConfirmBars:        fi.ContinuationConfirmBars,
		ContinuationDepthMult:          fi.ContinuationDepthMult,
		ContinuationTrendFilter:        fi.ContinuationTrendFilter,
		ContinuationTrendLookbackMin:   fi.ContinuationTrendLookbackMin,
		ContinuationMinRangePips:       fi.ContinuationMinRangePips,
		ContinuationMaxRangePips:       fi.ContinuationMaxRangePips,
		ContinuationExcludeRangePips:   fi.ContinuationExcludeRangePips,
		ContinuationMonthlyStopLossUSD: fi.ContinuationMonthlyStopLossUSD,
		SweepReclaimLevelSet:           fi.SweepReclaimLevelSet,
		SweepReclaimDepthMult:          fi.SweepReclaimDepthMult,
		SweepReclaimEntryWindowMinutes: fi.SweepReclaimEntryWindowMinutes,
		OpeningShockWindowMinutes:      fi.OpeningShockWindowMinutes,
		OpeningShockLookback:           fi.OpeningShockLookback,
		OpeningShockPercentile:         fi.OpeningShockPercentile,
		OpeningShockStopMult:           fi.OpeningShockStopMult,
		OpeningShockEntryDelayMin:      fi.OpeningShockEntryDelayMin,
		DirectionEligibility:           fi.DirectionEligibility,
		CommissionPerSide:              fi.CommissionPerSide,
		MaxCommissionToRisk:            fi.MaxCommissionToRisk,
		RiskMultiplier:                 fi.RiskMultiplier,
		MaxNotionalUSD:                 fi.MaxNotionalUSD,
		BreakEvenR:                     fi.BreakEvenR,
		TrailATRMult:                   fi.TrailATRMult,
		TrailAfterR:                    fi.TrailAfterR,
		PartialTakeProfitR:             fi.PartialTakeProfitR,
		PartialFraction:                fi.PartialFraction,
		RunnerTakeProfitR:              fi.RunnerTakeProfitR,
	}
}

// AsForexConfig maps the futures strategy config onto a ForexORBConfig so futures can reuse
// ComputeForexBracket / sizeForexQty / the entry-quality gates, which take a ForexORBConfig.
// Session is left empty (the live scheduler already branches session/timezone on
// isFuturesMode independently); this only carries the sizing and ATR/trend knobs. Mirrors
// the backtest's buildFuturesUniverse base-config mapping.
func (c FuturesORBConfig) AsForexConfig() ForexORBConfig {
	return ForexORBConfig{
		RangeMinutes:   c.RangeMinutes,
		RiskReward:     c.RiskReward,
		MaxRiskPct:     c.MaxRiskPct,
		MaxPositionPct: c.MaxPositionPct,
		MaxHoldMinutes: c.MaxHoldMinutes,
		ATRPeriod:      c.ATRPeriod,
		ATRTimeframe:   c.ATRTimeframe,
		TrendTimeframe: c.TrendTimeframe,
		TrendEMAPeriod: c.TrendEMAPeriod,
	}
}

// OptionsORBConfig configures the explicit options-only ORB runner: fixed
// underlyings execute as long single-leg equity options on Alpaca. Default-off.
// The gate thresholds mirror internal/options.GateConfig; sizing/exit knobs feed
// the router and app-managed exit manager.
type OptionsORBConfig struct {
	Enabled bool `yaml:"enabled"`
	// Underlyings is the fixed, rule-based set of options-mode symbols.
	Underlyings []string `yaml:"underlyings"`
	// ETFUnderlyings are earnings-free (SPY/QQQ) and exempt from the earnings gate.
	// A single-name underlying not listed here is subject to the earnings blackout,
	// and — with no earnings provider wired yet — fails closed (never trades) until
	// one is configured. v1 enables ETFs first for exactly this reason.
	ETFUnderlyings []string `yaml:"etf_underlyings"`

	// Sizing (dollars).
	PremiumBudget   float64 `yaml:"premium_budget"`     // premium to deploy per trade
	MaxLossPerTrade float64 `yaml:"max_loss_per_trade"` // hard cap on premium at risk (0 = none)
	RangeMinutes    int     `yaml:"range_minutes"`      // opening range, 5 or 15 minutes
	RiskReward      float64 `yaml:"risk_reward"`        // underlying target multiple, >= 1.5

	// Gate thresholds (see internal/options.GateConfig defaults).
	IVRankCeiling        float64 `yaml:"iv_rank_ceiling"`        // default 70
	MaxDTE               int     `yaml:"max_dte"`                // default 5
	DeltaMin             float64 `yaml:"delta_min"`              // default 0.50
	DeltaMax             float64 `yaml:"delta_max"`              // default 0.60
	SpreadPctCeiling     float64 `yaml:"spread_pct_ceiling"`     // default 5
	EarningsBlackoutDays int     `yaml:"earnings_blackout_days"` // default 3

	// Exit + IV pipeline.
	TimeExitMinutes int `yaml:"time_exit_minutes"` // IV-crush time exit window; 0 disables
	IVLookbackDays  int `yaml:"iv_lookback_days"`  // IV-Rank trailing window (default 252)
	IVMinSamples    int `yaml:"iv_min_samples"`    // fail-closed below this many stored days (default 40)
}

// FuturesORBConfig configures the Opening Range Breakout strategy for exchange-listed futures via IBKR.
// Re-used for different asset class groups (equity index, metals, energy) via separate top-level keys.
type FuturesORBConfig struct {
	Enabled        bool    `yaml:"enabled"`
	SessionOpenET  string  `yaml:"session_open_et"` // "HH:MM" in US/Eastern, e.g. "09:30"
	RangeMinutes   int     `yaml:"range_minutes"`
	RiskReward     float64 `yaml:"risk_reward"`
	MaxRiskPct     float64 `yaml:"max_risk_pct"`
	MaxPositionPct float64 `yaml:"max_position_pct"`

	// MaxHoldMinutes is the strategy-level default time limit on a filled futures ORB trade,
	// applied to any instrument that does not set its own FuturesInstrument.MaxHoldMinutes.
	// 0 leaves time exits disabled unless an instrument opts in. Mirrors ForexORBConfig.
	MaxHoldMinutes int `yaml:"max_hold_minutes"`

	// ATRPeriod / ATRTimeframe define the ATR used by the range-quality filter, and
	// TrendTimeframe / TrendEMAPeriod the EMA used by direction eligibility — the same
	// filters forex_orb uses, now available to futures once they route through the shared
	// dual-sided/gated path. Defaults (period 14 / "1h" ATR, "1h" / period 20 trend) match
	// the forex defaults, so an unset futures config behaves identically.
	ATRPeriod      int    `yaml:"atr_period"`
	ATRTimeframe   string `yaml:"atr_timeframe"`
	TrendTimeframe string `yaml:"trend_timeframe"`
	TrendEMAPeriod int    `yaml:"trend_ema_period"`

	Instruments []FuturesInstrument `yaml:"instruments"`
}

// ScannerConfig controls the autonomous market scanner that discovers swing-trade
// candidates and adds them to the watchlist with a configurable TTL.
type ScannerConfig struct {
	// Enabled turns the scanner on or off. Default: false.
	Enabled bool `yaml:"enabled"`

	// ScanIntervalHours is how often the scanner runs during market hours.
	// Default: 2 (runs at roughly 10am, 12pm, 2pm ET on a standard trading day).
	ScanIntervalHours int `yaml:"scan_interval_hours"`

	// CandidateCap is retained only so older operator files continue to load. New
	// installations use ResearchQueue, which also includes scheduled earnings.
	CandidateCap int `yaml:"candidate_cap"`

	// TTLDays is the default source-lease duration for ordinary scanner sources.
	// A scanner ticker is expired only after every source lease has elapsed.
	TTLDays int `yaml:"ttl_days"`

	// MinConviction is retained for configuration compatibility. Scanner
	// admission is deterministic and never reads this value.
	MinConviction float64 `yaml:"min_conviction"`

	// ResearchQueue is the legacy configuration key for the durable scanner
	// admission allocation on the main watchlist, including scheduled earnings.
	// Source reservations preserve coverage while Overflow provides shared
	// capacity once a source's own reservation is full.
	ResearchQueue ResearchQueueConfig `yaml:"research_queue"`
	// ResearchStaging is the short-lived scanner candidate queue ahead of main
	// watchlist admission. Unlike ResearchQueue, it does not reserve a source
	// allocation after a candidate has been admitted.
	ResearchStaging ResearchStagingConfig `yaml:"research_staging"`

	// NewsLookbackHours controls how far back to fetch news when enriching
	// screener candidates before LLM scoring. Default: 24.
	NewsLookbackHours int `yaml:"news_lookback_hours"`

	// TopMovers is the number of top movers (gainers + losers) to fetch per scan.
	// Default: 20.
	TopMovers int `yaml:"top_movers"`
	// TopMoversEnabled controls whether the Alpaca movers screener contributes
	// candidates. Default: true. Use this switch rather than setting TopMovers
	// to zero, which retains its legacy fallback of 20.
	TopMoversEnabled bool `yaml:"top_movers_enabled"`

	// TopActives is the number of most-active-by-volume stocks to fetch per scan.
	// Default: 20.
	TopActives int `yaml:"top_actives"`
	// TopActivesEnabled controls whether the Alpaca most-active screener
	// contributes candidates. Default: true.
	TopActivesEnabled bool `yaml:"top_actives_enabled"`

	// PromotionMaxPerIndustry is retained for legacy configuration only. The
	// deterministic research queue deliberately has no scanner-wide industry
	// admission cap; source reservations are the shared capacity contract.
	PromotionMaxPerIndustry int `yaml:"max_promotions_per_industry"`

	// PromotionCorrelation* values are retained for compatibility. When enabled,
	// the scanner applies its legacy DB-backed position/active-row duplicate
	// guard only; the former correlation selector no longer controls admission.
	PromotionCorrelationEnabled      bool    `yaml:"promotion_correlation_enabled"`
	PromotionMaxAbsCorrelation       float64 `yaml:"promotion_max_abs_correlation"`
	PromotionCorrelationLookbackDays int     `yaml:"promotion_correlation_lookback_days"`
	PromotionCorrelationMinOverlap   int     `yaml:"promotion_correlation_min_overlap"`

	// Universe configures the point-in-time, full-market stock universe used by
	// leadership and industry scanners. It is independent of the daily
	// movers/actives pool and defaults off so existing scanner behavior is
	// unchanged until an operator explicitly opts in.
	Universe UniverseConfig `yaml:"universe"`

	// WeeklyLeader configures the deterministic, full-universe leadership scan.
	// It runs after completed US sessions and only contributes candidates to the
	// normal scanner gate; it never grants trade permission by itself.
	WeeklyLeader WeeklyLeaderConfig `yaml:"weekly_leader"`

	// PotentIndustry configures the observation-first industry rotation lens.
	// Industry is the primary grouping; sector benchmarks provide context and a
	// fallback for members whose finer taxonomy is not sufficiently fresh.
	PotentIndustry PotentIndustryConfig `yaml:"potent_industry"`

	// PremarketEpisodic configures a pre-open, catalyst-aware gap scanner.
	// Qualified rows are only additive inputs to the normal scanner/analyst/risk
	// path; this source never places orders or grants trade permission.
	PremarketEpisodic PremarketEpisodicConfig `yaml:"premarket_episodic"`

	// EarningsWatchlist adds a small, calendar-driven cohort to the ordinary
	// analyst funnel before the next regular session.
	EarningsWatchlist EarningsWatchlistConfig `yaml:"earnings_watchlist"`

	// PoliticalDisclosures introduces persisted STOCK Act purchase/sale filings
	// to the normal scanner review path. It never bypasses LLM/risk gates.
	PoliticalDisclosures PoliticalDisclosureScannerConfig `yaml:"political_disclosures"`

	// GraphNeighbors configures sourcing scanner candidates from the knowledge
	// graph — tradeable companies one hop from a manually-watched ticker.
	GraphNeighbors GraphNeighborsConfig `yaml:"graph_neighbors"`

	// NewsSympathy configures news-triggered sympathy sourcing: fresh,
	// sentiment-strong news on a manually-watched ticker fans out to its graph
	// neighbours as candidates, carrying the triggering headline as the thesis.
	NewsSympathy NewsSympathyConfig `yaml:"news_sympathy"`
}

type ResearchQueueConfig struct {
	Capacity     int                       `yaml:"capacity"`
	Overflow     int                       `yaml:"overflow"`
	Reservations ResearchQueueReservations `yaml:"reservations"`
}

// ResearchStagingConfig bounds pending scanner work before it is admitted to
// the main watchlist. Queue rows are retried only while their short lease is
// live; source allocations remain owned by ResearchQueue.
type ResearchStagingConfig struct {
	Capacity     int `yaml:"capacity"`
	LeaseMinutes int `yaml:"lease_minutes"`
}

type ResearchQueueReservations struct {
	PoliticalDisclosure int `yaml:"political_disclosure"`
	WeeklyLeader        int `yaml:"weekly_leader"`
	PotentIndustry      int `yaml:"potent_industry"`
	MarketActivity      int `yaml:"market_activity"`
	PremarketEpisodic   int `yaml:"premarket_episodic"`
	GraphNeighbor       int `yaml:"graph_neighbor"`
	NewsSympathy        int `yaml:"news_sympathy"`
	EarningsScheduled   int `yaml:"earnings_scheduled"`
}

func (r ResearchQueueReservations) Total() int {
	return r.PoliticalDisclosure + r.WeeklyLeader + r.PotentIndustry +
		r.MarketActivity + r.PremarketEpisodic + r.GraphNeighbor +
		r.NewsSympathy + r.EarningsScheduled
}

type PoliticalDisclosureScannerConfig struct {
	Enabled             bool `yaml:"enabled"`
	PromoteCandidates   bool `yaml:"promote_candidates"`
	MaxCandidatesPerRun int  `yaml:"max_candidates_per_run"`
	ClaimLeaseMinutes   int  `yaml:"claim_lease_minutes"`
	// TTLDays is the legacy all-in-one window. It remains accepted as the
	// fallback for every new timing control so deployed configs are safe to
	// upgrade without an operator edit.
	TTLDays                    int `yaml:"ttl_days"`
	TriggerLookbackDays        int `yaml:"trigger_lookback_days"`
	AnalystEvidenceHorizonDays int `yaml:"analyst_evidence_horizon_days"`
	WatchlistLeaseDays         int `yaml:"watchlist_lease_days"`
	WatchlistLeaseSessions     int `yaml:"watchlist_lease_sessions"`
	// These loader-only flags retain whether zero was explicitly written in
	// YAML, distinct from a legacy config that omitted the newly split window.
	explicitTriggerLookbackDays        bool
	explicitAnalystEvidenceHorizonDays bool
	explicitWatchlistLeaseDays         bool
	explicitWatchlistLeaseSessions     bool
}

func (c PoliticalDisclosureScannerConfig) TriggerWindowDays() int {
	if c.TriggerLookbackDays > 0 {
		return c.TriggerLookbackDays
	}
	return c.TTLDays
}

func (c PoliticalDisclosureScannerConfig) EvidenceHorizonDays() int {
	if c.AnalystEvidenceHorizonDays > 0 {
		return c.AnalystEvidenceHorizonDays
	}
	return c.TTLDays
}

func (c PoliticalDisclosureScannerConfig) WatchlistLeaseWindowDays() int {
	if c.WatchlistLeaseDays > 0 {
		return c.WatchlistLeaseDays
	}
	return c.TTLDays
}

func (c PoliticalDisclosureScannerConfig) WatchlistLeaseWindowSessions() int {
	if c.WatchlistLeaseSessions > 0 {
		return c.WatchlistLeaseSessions
	}
	return 0
}

// WeeklyLeaderConfig controls the point-in-time weekly-leadership lens. The
// name describes its multi-week horizons; snapshots run after every completed
// session so breadth expansion and contraction remain observable.
type WeeklyLeaderConfig struct {
	Enabled                bool    `yaml:"enabled"`
	PromoteCandidates      bool    `yaml:"promote_candidates"`
	MaxPromotionsPerRun    int     `yaml:"max_promotions_per_run"`
	AfterCloseDelayMinutes int     `yaml:"after_close_delay_minutes"`
	UniverseMaxAgeHours    int     `yaml:"universe_max_age_hours"`
	HistoryCalendarDays    int     `yaml:"history_calendar_days"`
	MinHistorySessions     int     `yaml:"min_history_sessions"`
	Benchmark              string  `yaml:"benchmark"`
	IndustryMinPeers       int     `yaml:"industry_min_peers"`
	SelectionPercentile    float64 `yaml:"selection_percentile"`
	MinCompositeScore      float64 `yaml:"min_composite_score"`
	MinFeatureCoverage     float64 `yaml:"min_feature_coverage"`
	// MinCrossSectionCoverage is the minimum share of eligible symbols with
	// valid provider history required before qualified rows can be promoted.
	MinCrossSectionCoverage float64 `yaml:"min_cross_section_coverage"`
	BatchSize               int     `yaml:"batch_size"`
	// RunLeaseMinutes bounds both scheduled-run and candidate-promotion claims.
	RunLeaseMinutes int `yaml:"run_lease_minutes"`
	TTLDays         int `yaml:"ttl_days"`
}

// PotentIndustryConfig controls deterministic, point-in-time industry breadth
// evaluation and member ranking. Promotion is independently gated so operators
// can collect prospective observations before allowing candidates downstream.
type PotentIndustryConfig struct {
	Enabled                    bool              `yaml:"enabled"`
	PromoteCandidates          bool              `yaml:"promote_candidates"`
	MaxPromotionsPerRun        int               `yaml:"max_promotions_per_run"`
	AfterCloseDelayMinutes     int               `yaml:"after_close_delay_minutes"`
	UniverseMaxAgeHours        int               `yaml:"universe_max_age_hours"`
	TaxonomyMaxAgeHours        int               `yaml:"taxonomy_max_age_hours"`
	HistoryCalendarDays        int               `yaml:"history_calendar_days"`
	MinHistorySessions         int               `yaml:"min_history_sessions"`
	MarketBenchmark            string            `yaml:"market_benchmark"`
	SectorBenchmarks           map[string]string `yaml:"sector_benchmarks"`
	IndustryBenchmarkOverrides map[string]string `yaml:"industry_benchmark_overrides"`
	MinFreshTaxonomyCoverage   float64           `yaml:"min_fresh_taxonomy_coverage"`
	MinGroupMembers            int               `yaml:"min_group_members"`
	MinGroupCoverage           float64           `yaml:"min_group_coverage"`
	MinPotencyScore            float64           `yaml:"min_potency_score"`
	MinMemberScore             float64           `yaml:"min_member_score"`
	SelectionPercentile        float64           `yaml:"selection_percentile"`
	MinAdvanceShare            float64           `yaml:"min_advance_share"`
	MinMedianReturn5D          float64           `yaml:"min_median_return_5d"`
	MaxCapEqualDivergence      float64           `yaml:"max_cap_equal_divergence"`
	MinPersistenceSessions     int               `yaml:"min_persistence_sessions"`
	MaxMembersPerGroup         int               `yaml:"max_members_per_group"`
	BatchSize                  int               `yaml:"batch_size"`
	// RunLeaseMinutes bounds scheduled-run claims.
	RunLeaseMinutes int `yaml:"run_lease_minutes"`
	TTLDays         int `yaml:"ttl_days"`
}

// PremarketEpisodicConfig controls deterministic premarket gap observations.
// Times are interpreted in America/New_York. Every checkpoint must precede the
// hard cutoff so a slow scan can never overlap opening-range construction.
type PremarketEpisodicConfig struct {
	Enabled           bool     `yaml:"enabled"`
	PromoteCandidates bool     `yaml:"promote_candidates"`
	Checkpoints       []string `yaml:"checkpoints"`
	HardCutoff        string   `yaml:"hard_cutoff"`
	Feed              string   `yaml:"feed"`
	// UniverseMaxAgeHours is retained for configuration compatibility. Freshness
	// is now exact-session based so weekends and exchange holidays are safe.
	UniverseMaxAgeHours int     `yaml:"universe_max_age_hours"`
	HistorySessions     int     `yaml:"history_sessions"`
	NewsLookbackHours   int     `yaml:"news_lookback_hours"`
	MaxCandidatesPerRun int     `yaml:"max_candidates_per_run"`
	MaxPromotionsPerRun int     `yaml:"max_promotions_per_run"`
	MinGapPct           float64 `yaml:"min_gap_pct"`
	MinPremarketVolume  int64   `yaml:"min_premarket_volume"`
	MinTimeAdjustedRVOL float64 `yaml:"min_time_adjusted_rvol"`
	MinDollarVolume     float64 `yaml:"min_dollar_volume"`
	MinTradeCount       int64   `yaml:"min_trade_count"`
	MaxSpreadBPS        float64 `yaml:"max_spread_bps"`
	MaxQuoteAgeSeconds  int     `yaml:"max_quote_age_seconds"`
	MinBarCoverage      float64 `yaml:"min_bar_coverage"`
	MinPrice            float64 `yaml:"min_price"`
	MinMarketCapMM      float64 `yaml:"min_market_cap_mm"`
	BatchSize           int     `yaml:"batch_size"`
	RunLeaseMinutes     int     `yaml:"run_lease_minutes"`
}

// EarningsWatchlistConfig controls the scheduled-earnings watchlist feeder.
// It intentionally has no result-confirmation or scanner-promotion settings.
type EarningsWatchlistConfig struct {
	Enabled                bool `yaml:"enabled"`
	AfterCloseDelayMinutes int  `yaml:"after_close_delay_minutes"`
	CandidateCap           int  `yaml:"candidate_cap"`
	WatchlistSessions      int  `yaml:"watchlist_sessions"`
	BarBatchSize           int  `yaml:"bar_batch_size"`
}

// UniverseConfig controls construction and caching of the point-in-time stock
// universe. Eligibility evidence and taxonomy are recorded as-of the requested
// snapshot; a provider must never substitute current data for unavailable
// historical data. Missing-data policies apply per symbol so a partial provider
// failure does not discard successful results from the same bulk request.
type UniverseConfig struct {
	// Enabled opts into full-universe discovery. Default: false.
	Enabled bool `yaml:"enabled"`

	// AfterCloseDelayMinutes schedules the durable universe build after an
	// official session close. RunLeaseMinutes bounds one build claim.
	AfterCloseDelayMinutes int `yaml:"after_close_delay_minutes"`
	RunLeaseMinutes        int `yaml:"run_lease_minutes"`

	// MinMarketDataCoverage is the minimum share of asset-master-eligible
	// symbols with both a usable snapshot and completed daily-bar history.
	MinMarketDataCoverage float64 `yaml:"min_market_data_coverage"`

	// Provider selects the provider-neutral universe adapter. Default: "alpaca".
	Provider string `yaml:"provider"`

	// SnapshotFeed and HistoricalFeed select Alpaca feeds by operation. Universe
	// snapshots run after the close and can use delayed consolidated evidence.
	// SIPRecentDataDelayMinutes keeps historical requests outside the account's
	// entitlement window; set it to zero when real-time SIP is entitled. Keeping
	// these settings separate prevents an accidental real-time SIP requirement.
	SnapshotFeed              string `yaml:"snapshot_feed"`
	HistoricalFeed            string `yaml:"historical_feed"`
	SIPRecentDataDelayMinutes int    `yaml:"sip_recent_data_delay_minutes"`

	// Objective eligibility thresholds. A zero MaxSpreadBPS disables only the
	// spread ceiling; the remaining defaults are active once Enabled is true.
	MinPrice                 float64  `yaml:"min_price"`
	MinMarketCapMM           float64  `yaml:"min_market_cap_mm"`
	MinMedianDollarVolume    float64  `yaml:"min_median_dollar_volume"`
	DollarVolumeLookbackDays int      `yaml:"dollar_volume_lookback_days"`
	AllowedSecurityTypes     []string `yaml:"allowed_security_types"`
	AllowedExchanges         []string `yaml:"allowed_exchanges"`
	ExcludedNamePatterns     []string `yaml:"excluded_name_patterns"`
	MaxSpreadBPS             float64  `yaml:"max_spread_bps"`
	RequireActive            bool     `yaml:"require_active"`
	RequireTradable          bool     `yaml:"require_tradable"`
	RequireMarketData        bool     `yaml:"require_market_data"`

	// SnapshotTTLHours bounds reuse of a complete universe snapshot.
	// TaxonomyMaxAgeHours independently prevents stale classifications from
	// qualifying an industry-level signal.
	SnapshotTTLHours    int `yaml:"snapshot_ttl_hours"`
	TaxonomyMaxAgeHours int `yaml:"taxonomy_max_age_hours"`

	// Bulk-operation controls keep provider calls bounded and rate-limit aware.
	BatchSize            int `yaml:"batch_size"`
	MaxConcurrency       int `yaml:"max_concurrency"`
	TaxonomyRefreshLimit int `yaml:"taxonomy_refresh_limit"`

	// MissingFundamentalsPolicy and MissingMarketDataPolicy are "exclude" or
	// "include". The safe defaults are "exclude"; "include" is an explicit
	// fail-open choice whose missing evidence remains visible in snapshots.
	MissingFundamentalsPolicy string `yaml:"missing_fundamentals_policy"`
	MissingMarketDataPolicy   string `yaml:"missing_market_data_policy"`
}

// NewsSympathyConfig controls news-triggered sympathy candidate sourcing. Where
// GraphNeighborsConfig walks the graph on a timer regardless of events, this
// source reacts to news: a fresh, sentiment-strong article about watched ticker
// A surfaces A's eligible graph neighbours as candidates, each carrying the
// triggering headline + sentiment + relationship so the scoring LLM evaluates
// the spillover thesis (and its direction) rather than a naked chart. Edge
// eligibility reuses the GraphNeighbors thresholds. Disabled by default.
type NewsSympathyConfig struct {
	// Enabled turns news-sympathy sourcing on or off. Default: false.
	Enabled bool `yaml:"enabled"`

	// LookbackHours is how fresh a triggering article must be. Stale news has
	// already been priced in — the spillover edge decays in hours. Default: 6.
	LookbackHours int `yaml:"lookback_hours"`

	// MinAbsSentiment is the minimum |sentiment_score| (0–1) for an article to
	// act as a trigger; near-neutral news does not move related names. Default: 0.35.
	MinAbsSentiment float64 `yaml:"min_abs_sentiment"`

	// MaxPerScan caps sympathy candidates added to the pool per scan. Default: 5.
	MaxPerScan int `yaml:"max_per_scan"`

	// PairCooldownHours suppresses re-triggering the same (seed, neighbour)
	// pair, so one running story doesn't re-surface the same sympathy idea
	// every scan. Default: 24.
	PairCooldownHours int `yaml:"pair_cooldown_hours"`
}

// GraphNeighborsConfig controls whether the scanner sources additional candidates
// from the knowledge graph. When enabled, every scan traverses out from manually
// watched tickers (source != "scanner") to their trusted graph neighbours and feeds
// those tickers through the same LLM scoring + conviction gate as screener
// candidates. Disabled by default so it never changes behaviour until opted in.
type GraphNeighborsConfig struct {
	// Enabled turns graph-neighbour sourcing on or off. Default: false.
	Enabled bool `yaml:"enabled"`

	// MaxPerScan caps how many graph-neighbour candidates are added to the
	// candidate pool per scan, so the graph can't crowd out screener candidates.
	// Default: 5.
	MaxPerScan int `yaml:"max_per_scan"`

	// MinConfidence is the minimum extractor confidence (0–1) for an
	// auto-generated edge to be eligible. Seed/confirmed edges bypass this.
	// Default: 0.80.
	MinConfidence float64 `yaml:"min_confidence"`

	// MinCorroboration is the minimum number of distinct news items that must have
	// asserted an auto-generated edge for it to be eligible. Seed/confirmed edges
	// bypass this. Default: 2.
	MinCorroboration int `yaml:"min_corroboration"`

	// MinEdgeStrength is the minimum edge strength (0–1) for a neighbour to be
	// considered. Default: 0.6.
	MinEdgeStrength float64 `yaml:"min_edge_strength"`
}

// FeedbackConfig controls the automated feedback pipeline (Phase 17).
type FeedbackConfig struct {
	Enabled          bool                  `yaml:"enabled"`
	ReviewTimeUTC    string                `yaml:"review_time_utc"`
	Windows          []int                 `yaml:"windows"`
	PromptEnrichment bool                  `yaml:"prompt_enrichment"`
	Thresholds       FeedbackThresholds    `yaml:"thresholds"`
	ORBAutoThrottle  ORBAutoThrottleConfig `yaml:"orb_auto_throttle"`
}

// ORBAutoThrottleConfig closes the feedback loop on ORB forex position sizing: it derives a
// per-instrument risk multiplier from rolling realised expectancy (the feedback avg_pnl_pct
// over Window), sizing a losing instrument down and a winning one up within [Floor, Cap].
// The nightly reviewer computes and stores it as a `risk_multiplier` insight; the scheduler
// reads it and MULTIPLIES the static per-instrument risk_multiplier by it. Default-off, so
// disabled (or below MinTrades, or no data) leaves sizing exactly as today (×1.0).
type ORBAutoThrottleConfig struct {
	Enabled     bool    `yaml:"enabled"`
	Window      string  `yaml:"window"`       // feedback window to learn from, e.g. "30d" (default "30d")
	MinTrades   int     `yaml:"min_trades"`   // below this trade count → neutral 1.0 (default 20)
	Sensitivity float64 `yaml:"sensitivity"`  // multiplier change per 1 pct-point of avg pnl (default 1.0)
	Floor       float64 `yaml:"floor"`        // minimum multiplier (default 0.5)
	Cap         float64 `yaml:"cap"`          // maximum multiplier (default 1.5)
	MaxAgeDays  int     `yaml:"max_age_days"` // ignore a stored multiplier older than this (default 3)
}

// ThrottleWindow returns the configured learn-from window, defaulting to "30d".
func (c ORBAutoThrottleConfig) ThrottleWindow() string {
	if c.Window == "" {
		return "30d"
	}
	return c.Window
}

// MinTradesOrDefault is the minimum logical-trade count below which the throttle takes no
// action, defaulting to 20. Shared by the reviewer (which skips storing a multiplier below
// it, so a transient data gap can't overwrite a valid one) and the mapping function.
func (c ORBAutoThrottleConfig) MinTradesOrDefault() int {
	if c.MinTrades <= 0 {
		return 20
	}
	return c.MinTrades
}

// MaxAgeDaysOrDefault is the freshness horizon: a stored multiplier older than this is
// ignored (the scheduler falls back to neutral sizing), so a rolling window that dries up —
// dropping below min-trades, or the instrument going quiet — stops applying a stale value
// once the reviewer stops refreshing it. Because the reviewer SKIPS writing below min-trades,
// the stored date is exactly "when we last had enough data", so this tolerates a transient
// dip (still recent) while reverting a sustained one to neutral. Defaults to 3 days.
func (c ORBAutoThrottleConfig) MaxAgeDaysOrDefault() int {
	if c.MaxAgeDays <= 0 {
		return 3
	}
	return c.MaxAgeDays
}

type FeedbackThresholds struct {
	ModelWinRateMin       float64 `yaml:"model_win_rate_min"`
	ModelMinTrades        int     `yaml:"model_min_trades"`
	InstrumentWinRateMin  float64 `yaml:"instrument_win_rate_min"`
	InstrumentMinTrades   int     `yaml:"instrument_min_trades"`
	ArbiterInvokedRateMax float64 `yaml:"arbiter_invoked_rate_max"`
}

// NewsAPIConfig configures the news data provider (Finnhub or Benzinga).
type NewsAPIConfig struct {
	Provider      string `yaml:"provider"`        // finnhub | benzinga
	APIKeyEnv     string `yaml:"api_key_env"`     // env var name holding the API key
	APIKey        string `yaml:"-" json:"-"`      // resolved at runtime
	FetchEveryMin int    `yaml:"fetch_every_min"` // cadence in minutes (default 15)

	// Extraction controls knowledge-graph enrichment LLM calls.
	ExtractionEnabled            bool   `yaml:"extraction_enabled"`
	ExtractionProvider           string `yaml:"extraction_provider"`              // anthropic | openai | deepseek | google | ollama
	ExtractionModel              string `yaml:"extraction_model"`                 // optional provider model override
	ExtractionMaxArticlesPerTick int    `yaml:"extraction_max_articles_per_tick"` // cap per ticker per engine tick
	ExtractionRPM                int    `yaml:"extraction_rpm"`                   // global requests/minute cap
}

type StocksConfig struct {
	Enabled               bool   `yaml:"enabled"`
	Paper                 bool   `yaml:"paper"`
	ExpectedAccountNumber string `yaml:"expected_account_number"`
	APIKeyEnv             string `yaml:"api_key_env"`
	APIKey                string `yaml:"-" json:"-"`
	SecretEnv             string `yaml:"secret_env"`
	Secret                string `yaml:"-" json:"-"`
	BaseURL               string `yaml:"base_url"`
	DataURL               string `yaml:"data_url"`
	// OptionFeed selects the Alpaca options market-data feed: "opra" (real-time,
	// requires the Algo Trader Plus subscription) or "indicative" (free, 15-min
	// delayed). Empty defaults to "indicative". Options-ORB (issue #414) requires
	// "opra" to trade the open; the adapter logs when it falls back to indicative.
	OptionFeed string `yaml:"option_feed"`
}

const (
	// AlpacaPaperTradingURL and AlpacaLiveTradingURL are the only trading hosts
	// accepted for stock execution. Keeping the environment and host paired
	// prevents paper credentials from silently being sent to a custom or live
	// endpoint (and vice versa).
	AlpacaPaperTradingURL = "https://paper-api.alpaca.markets"
	AlpacaLiveTradingURL  = "https://api.alpaca.markets"
)

// ValidateStockBrokerConfig verifies the effective stock execution environment.
// Call it after runtime overrides have been applied, rather than while reading
// YAML, because a DB override may deliberately switch paper/live at startup.
func ValidateStockBrokerConfig(stocks StocksConfig) error {
	if !stocks.Enabled {
		return nil
	}

	baseURL, err := url.Parse(strings.TrimSpace(stocks.BaseURL))
	if err != nil || baseURL.Scheme != "https" || baseURL.Host == "" ||
		baseURL.User != nil || (baseURL.Path != "" && baseURL.Path != "/") ||
		baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return fmt.Errorf("markets.stocks.base_url must be a canonical HTTPS Alpaca trading host, got %q", stocks.BaseURL)
	}

	expected := AlpacaLiveTradingURL
	if stocks.Paper {
		expected = AlpacaPaperTradingURL
	}
	expectedURL, _ := url.Parse(expected)
	if !strings.EqualFold(baseURL.Host, expectedURL.Host) {
		environment := "live"
		if stocks.Paper {
			environment = "paper"
		}
		return fmt.Errorf("markets.stocks.paper=%t requires the %s Alpaca host %s, got %q", stocks.Paper, environment, expected, stocks.BaseURL)
	}
	return nil
}

// ValidateStockWorkflowConfig verifies the one unsafe engine/environment
// pairing. Engine paper mode is a paper workflow, so it must not point at a
// live Alpaca account. Auto mode deliberately remains valid with either
// environment: it is used for both paper testing and live execution.
func ValidateStockWorkflowConfig(mode string, stocks StocksConfig) error {
	if stocks.Enabled && mode == "paper" && !stocks.Paper {
		return fmt.Errorf("engine.mode=paper requires markets.stocks.paper=true")
	}
	return nil
}

// ── Strategies ────────────────────────────────────────────────────────────────

type StrategiesConfig struct {
	TrendFollowing TrendFollowingConfig `yaml:"trend_following"`
	MeanReversion  MeanReversionConfig  `yaml:"mean_reversion"`
	Momentum       MomentumConfig       `yaml:"momentum"`
	NewsSentiment  NewsSentimentConfig  `yaml:"news_sentiment"`
}

type TrendFollowingConfig struct {
	Enabled   bool    `yaml:"enabled"`
	EMAPeriod int     `yaml:"ema_period"`
	ADXMin    float64 `yaml:"adx_min"`
	RSIMin    float64 `yaml:"rsi_min"`
	RSIMax    float64 `yaml:"rsi_max"`
}

type MeanReversionConfig struct {
	Enabled       bool    `yaml:"enabled"`
	BBPeriod      int     `yaml:"bb_period"`
	BBStdDev      float64 `yaml:"bb_std_dev"`
	RSIOversold   float64 `yaml:"rsi_oversold"`
	RSIOverbought float64 `yaml:"rsi_overbought"`
}

type MomentumConfig struct {
	Enabled        bool    `yaml:"enabled"`
	MACDFast       int     `yaml:"macd_fast"`
	MACDSlow       int     `yaml:"macd_slow"`
	MACDSignal     int     `yaml:"macd_signal"`
	VolumeMultiple float64 `yaml:"volume_multiple"`
}

type NewsSentimentConfig struct {
	Enabled      bool    `yaml:"enabled"`
	MinSentiment float64 `yaml:"min_sentiment"`
	MaxAgeHours  int     `yaml:"max_age_hours"`
}

// ── Risk ─────────────────────────────────────────────────────────────────────

type RiskConfig struct {
	ConfidenceThreshold float64 `yaml:"confidence_threshold"`
	// ConfidenceTiers expresses tiered confidence requirements that vary by
	// the trade's R:R ratio (#317). Default empty (no tiers — falls back
	// to the static `confidence_threshold`). Configure tiers in YAML to
	// enable R:R-aware confidence gating. Tiers are evaluated in
	// declaration order; the first tier whose RRMin is <= the trade's R:R
	// applies. Best practice: declare tiers in descending RRMin order
	// (e.g. {2.0, 0.60} then {1.4, 0.65}). Defaults() leaves this nil so
	// existing configs that omit the key are unaffected by the upgrade —
	// see config.example.yaml for the recommended ladder.
	ConfidenceTiers  []ConfidenceTier `yaml:"confidence_tiers"`
	MinRiskReward    float64          `yaml:"min_risk_reward"`
	MaxPositionPct   float64          `yaml:"max_position_pct"`
	MaxOpenPositions int              `yaml:"max_open_positions"`
	// MaxPerTicker caps the number of open positions in the same direction
	// for a single ticker (#312). Default is 0 (disabled) so older configs
	// that don't set this key upgrade safely without unexpected blocks; the
	// example config recommends 2. Operators opt in by setting a positive
	// value in their config.
	MaxPerTicker int `yaml:"max_per_ticker"`
	// MaxDollarRiskPct caps the per-trade dollar at risk
	// (qty × |entry − stop|) as a percent of portfolio equity (#320).
	// Default 1.0. Treated as disabled when set to 0.
	MaxDollarRiskPct float64 `yaml:"max_dollar_risk_pct"`
	// PyramidEnabled gates the pyramiding-into-winners feature (#324). When
	// true, a BUY signal on a ticker with an existing open long that is up
	// ≥ PyramidMinGainPct adds a new lot with a tightened stop instead of
	// being rejected as a duplicate. Default is false (safe rollout): the
	// operator must explicitly opt in. When false, BUY-on-existing-long is
	// blocked with reason "pyramid_disabled".
	PyramidEnabled bool `yaml:"pyramid_enabled"`
	// PyramidMinGainPct is the minimum unrealised gain (%) required on the
	// existing long for a BUY to qualify as a pyramid add. Default 5.0%.
	PyramidMinGainPct    float64 `yaml:"pyramid_min_gain_pct"`
	DailyLossLimitPct    float64 `yaml:"daily_loss_limit_pct"`
	MaxDrawdownPct       float64 `yaml:"max_drawdown_pct"`
	CooldownAfterLoss    int     `yaml:"cooldown_after_loss_min"`
	DefaultStopLossPct   float64 `yaml:"default_stop_loss_pct"`
	DefaultTakeProfitPct float64 `yaml:"default_take_profit_pct"`
	SignalExpiryMin      int     `yaml:"signal_expiry_min"`
	StalePriceMovePct    float64 `yaml:"stale_price_move_pct"`

	// TrailingStop configures the server-side trailing stop manager (#322).
	// When Enabled, a goroutine manages broker-resident protective stops and
	// applies the selected ATR or daily-close EMA9 exit rule. Disabled by
	// default.
	TrailingStop TrailingStopConfig `yaml:"trailing_stop"`

	// UncapTakeProfit removes the fixed take-profit from stock-path long
	// entries when the trailing-stop manager is enabled. The initial hard
	// stop remains active; the selected trailing rule owns the winning exit.
	// Default false preserves the existing bracket/software TP behavior.
	UncapTakeProfit bool `yaml:"uncap_take_profit"`

	// ExitConfMin is the minimum confidence required for an analyst SELL to
	// close an existing stock-path long. Zero preserves the current ungated
	// behavior. Protective and trailing stops are not subject to this gate.
	ExitConfMin float64 `yaml:"exit_conf_min"`

	// BracketExitsEnabled submits stock-path BUY entries as broker bracket
	// orders (GTC, whole shares): the take-profit and stop-loss live at the
	// broker and fire intrabar, instead of the engine's hourly candle-close
	// checks (measured ~1.5%/trade of exit slippage, PR #433 study).
	// Requires stop_loss AND take_profit on the signal. Enabled entries are
	// whole-share only and reject rather than falling back to application-only
	// protection. Default off.
	BracketExitsEnabled bool `yaml:"bracket_exits_enabled"`

	// EntryGates are evidence-driven entry-quality filters from the execution
	// and exit-policy studies: daily downtrend alignment, news tape
	// confirmation, momentum anti-chase, and first-tick deferral. All gates
	// default OFF; each is independent.
	EntryGates EntryGatesConfig `yaml:"entry_gates"`
}

// EntryGatesConfig holds per-strategy entry-quality gates. Zero values
// disable every gate so existing configs upgrade without behavior change.
// Gates apply to BUY signals only. The pullback strategy is an additive path:
// missing or incomplete pullback evidence falls back to the standard entry
// gates, while a confirmed setup receives its dedicated stop and revalidation.
type EntryGatesConfig struct {
	// AntiChase is the universal, deterministic BUY timing policy. Unlike the
	// legacy lens-specific run-up checks below, it cannot be bypassed by a lens
	// fallback and is re-evaluated immediately before broker submission.
	AntiChase AntiChaseConfig `yaml:"anti_chase"`
	// TrendAlignmentEnabled blocks stock BUYs in a confirmed daily downtrend:
	// price below both the 9- and 21-day EMAs while both completed-session
	// EMAs are declining. False preserves existing behavior. The gate fails
	// open when daily history is unavailable or insufficient.
	TrendAlignmentEnabled bool `yaml:"trend_alignment_enabled"`
	// PullbackStrengthEnabled adds the complete Phase 2 pullback-in-strength
	// path for universe-qualified scanner candidates: strong multi-month momentum,
	// a tight, rotating eight-week base around aligned rising EMA9/EMA21, causal
	// AVWAP/horizontal stacked support, an intraday flush and positive reclaim,
	// and a sufficiently tight low-of-day stop. Candidates that do not complete
	// the setup continue through the standard entry path. False preserves
	// existing behavior.
	PullbackStrengthEnabled bool `yaml:"pullback_strength_enabled"`
	// PullbackMinMomentumReturnPct is the minimum return on at least one of the
	// 1/3/6-month horizons. Zero defaults to the study's 30%.
	PullbackMinMomentumReturnPct float64 `yaml:"pullback_min_momentum_return_pct"`
	// PullbackConsolidationSessions is the completed-session structure window.
	// Zero defaults to 40 trading sessions (approximately eight weeks).
	PullbackConsolidationSessions int `yaml:"pullback_consolidation_sessions"`
	// PullbackSupportTolerancePct controls AVWAP/EMA stacking, support clustering,
	// the maximum flush penetration, and the maximum executable extension above
	// reclaimed support.
	// Zero defaults to 1.5%.
	PullbackSupportTolerancePct float64 `yaml:"pullback_support_tolerance_pct"`
	// PullbackMaxStopWidthPct caps the entry-to-low-of-day risk. Zero defaults
	// to the study's upper bound of 4%.
	PullbackMaxStopWidthPct float64 `yaml:"pullback_max_stop_width_pct"`
	// NewsMinRunupPct blocks news_sentiment BUYs unless the price is at
	// least this % above the prior daily close — the market's reaction
	// must confirm the headline (its absence is information). 0 disables.
	// Recommended 2.0.
	NewsMinRunupPct float64 `yaml:"news_min_runup_pct"`
	// MomentumMaxRunupPct blocks momentum BUYs when the price is already
	// more than this % above the prior daily close (anti-chase). 0
	// disables. Recommended 2.0.
	MomentumMaxRunupPct float64 `yaml:"momentum_max_runup_pct"`
	// FirstTickDefer holds BUY signals generated on the session's first
	// analysis tick (including the first tick after a restart) as pending
	// instead of executing immediately; the next tick executes them only
	// if the price has held above the signal price minus the tolerance.
	// Breaks the correlated overnight-headline burst without a
	// time-of-day ban. Deferred signals inherit the normal signal-expiry
	// and stale-price protections while pending.
	FirstTickDefer bool `yaml:"first_tick_defer"`
	// FirstTickHoldTolerancePct is how far (%) the price may sit below the
	// deferred signal's price and still count as "held". Defaults to 1.0
	// when FirstTickDefer is on and this is 0.
	FirstTickHoldTolerancePct float64 `yaml:"first_tick_hold_tolerance_pct"`
}

// AntiChaseConfig controls the V1 deterministic BUY timing policy. The three
// combinations are deliberately typed rather than expressed as arbitrary YAML
// logic so every persisted decision has stable, reviewable semantics.
type AntiChaseConfig struct {
	Enabled bool `yaml:"enabled"`

	StaleCatalystRunupEnabled bool    `yaml:"stale_catalyst_runup_enabled"`
	StaleAfterHours           float64 `yaml:"stale_after_hours"`
	MinReturn3SessionPct      float64 `yaml:"min_return_3_session_pct"`
	WeakVolumeRatioMax        float64 `yaml:"weak_volume_ratio_max"`
	StaleEMAExtensionATR      float64 `yaml:"stale_ema_extension_atr"`

	PriorHighExtensionEnabled bool    `yaml:"prior_high_extension_enabled"`
	NearPriorHighPct          float64 `yaml:"near_prior_high_pct"`
	HighEMAExtensionATR       float64 `yaml:"high_ema_extension_atr"`

	MomentumExhaustionEnabled bool    `yaml:"momentum_exhaustion_enabled"`
	RSIMin                    float64 `yaml:"rsi_min"`
	StochKMin                 float64 `yaml:"stoch_k_min"`
	ExhaustionReturn3Pct      float64 `yaml:"exhaustion_return_3_session_pct"`
}

// TrailingStopConfig governs the server-side trailing stop manager (#322).
// Rule is "atr" (the existing hourly ATR ratchet) or "ema9" (exit when the
// latest daily candle closes below its daily 9-EMA). The zero value defaults
// to "atr" for backward compatibility. ATRMultiple is a unitless coefficient
// applied to absolute-price ATR; the trailing stop is
// `high_water_mark - ATRMultiple * ATR`. IntervalSec defaults to 60s when
// zero. ATRPeriod defaults to 14 (Wilder).
type TrailingStopConfig struct {
	Enabled     bool    `yaml:"enabled"`
	Rule        string  `yaml:"rule"`
	ATRMultiple float64 `yaml:"atr_multiple"`
	IntervalSec int     `yaml:"interval_sec"`
	ATRPeriod   int     `yaml:"atr_period"`

	// ActivateAtGainPct withholds the trail until the position's PEAK gain from
	// entry reaches this percentage, so a trade that ticks up and pulls back is
	// left to its original protective stop instead of being trailed out before
	// it has run. Keyed on the high-water mark, so arming latches: once earned,
	// the trail stays on even if price falls back.
	//
	// 0 (default) keeps the pre-existing behaviour of trailing from the first
	// tick. Applies to rule "atr" only — the "ema9" rule tracks no high-water
	// mark, so it has nothing to latch on.
	ActivateAtGainPct float64 `yaml:"activate_at_gain_pct"`
}

// ConfidenceTier expresses a (R:R floor → confidence floor) pair (#317).
// Used to require stricter confidence at lower R:R bands and looser
// confidence at higher R:R bands. See RiskConfig.ConfidenceTiers.
type ConfidenceTier struct {
	RRMin   float64 `yaml:"rr_min"`
	ConfMin float64 `yaml:"conf_min"`
}

// ── Notifications ─────────────────────────────────────────────────────────────

type NotificationsConfig struct {
	Telegram TelegramConfig `yaml:"telegram"`
}

type TelegramConfig struct {
	Enabled     bool     `yaml:"enabled"`
	BotTokenEnv string   `yaml:"bot_token_env"`
	BotToken    string   `yaml:"-" json:"-"`
	ChatID      string   `yaml:"chat_id"`
	NotifyOn    []string `yaml:"notify_on"`
	// ScannerNotificationsConfigured distinguishes an older notify_on list from
	// an explicit scanner-category choice made in the current configuration UI.
	ScannerNotificationsConfigured *bool `yaml:"scanner_notifications_configured,omitempty"`

	// Batching config (Epic 1 cost reduction).
	BatchEnabled          bool    `yaml:"batch_enabled"`
	BatchWindowSec        int     `yaml:"batch_window_sec"`
	MaxItemsPerBatch      int     `yaml:"max_items_per_batch"`
	IncludeHolds          bool    `yaml:"include_holds"`
	MinConfidenceForAlert float64 `yaml:"min_confidence_for_alert"`
}

// ── Logging ───────────────────────────────────────────────────────────────────

type LoggingConfig struct {
	Level      string `yaml:"level"`        // debug | info | warn | error
	Format     string `yaml:"format"`       // json | text
	MaxSizeMB  int    `yaml:"max_size_mb"`  // max size of engine.log before rotation (MB)
	MaxBackups int    `yaml:"max_backups"`  // number of rotated log files to keep
	MaxAgeDays int    `yaml:"max_age_days"` // max age of rotated log files (days)
	Compress   bool   `yaml:"compress"`     // gzip rotated log files
}

// ── Database ──────────────────────────────────────────────────────────────────

type DatabaseConfig struct {
	URLEnv string `yaml:"url_env"` // default DATABASE_URL
}

// ── Reports ───────────────────────────────────────────────────────────────────

type ReportsConfig struct {
	OutputDir       string `yaml:"output_dir"`
	AutoOpenBrowser bool   `yaml:"auto_open_browser"`
}

// ── Backtest ──────────────────────────────────────────────────────────────────

type BacktestConfig struct {
	// InitialCash is the virtual starting portfolio value for each backtest run.
	InitialCash float64 `yaml:"initial_cash"`
	// RiskFreeRate is the annual risk-free rate used for Sharpe/Sortino/Calmar.
	// Default 0.04 (4%, approximate US Treasury yield).
	RiskFreeRate float64 `yaml:"risk_free_rate"`
}

// ── Loader ────────────────────────────────────────────────────────────────────

var (
	mu      sync.RWMutex
	current *Config
)

// ConfigPath returns the default config file path.
func ConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".tradeengine", "config.yaml")
}

// Load reads config.yaml (creating from defaults if absent), resolves env vars,
// and stores the result as the current global config.
//
// Load order (guarantees user explicit values always win over profile defaults):
//  1. Defaults()           — baseline safe values
//  2. peek cost.profile    — extract profile key only from YAML
//  3. ApplyProfile()       — overlay profile-specific defaults
//  4. yaml.Unmarshal()     — user's explicit values overwrite profile defaults
//  5. migrateModelsConfig / migratePoliticalDisclosureTimingConfig — legacy field migration
//  6. resolveEnvVars       — substitute environment variables
func Load() (*Config, error) {
	mu.Lock()
	defer mu.Unlock()

	cfg := Defaults()
	path := ConfigPath()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// First run: apply profile defaults before saving so the written
			// file contains the correct profile-driven values.
			ApplyProfile(cfg)
			if err := save(cfg, path); err != nil {
				return nil, fmt.Errorf("create default config: %w", err)
			}
		} else {
			return nil, fmt.Errorf("read config: %w", err)
		}
	} else {
		// Peek the cost.profile value from YAML so we can apply profile
		// defaults before the full unmarshal, ensuring user values win.
		var peek struct {
			Cost struct {
				Profile string `yaml:"profile"`
			} `yaml:"cost"`
		}
		// Ignore peek errors — invalid YAML caught by full unmarshal below.
		_ = yaml.Unmarshal(data, &peek)
		if peek.Cost.Profile != "" {
			cfg.Cost.Profile = peek.Cost.Profile
		}

		ApplyProfile(cfg)

		// Defaults marks freshly generated configurations as scanner-category
		// aware. Existing files that predate that marker must remain unmarked
		// when they omit it, so their legacy scanner summaries stay enabled.
		// yaml.Unmarshal does not clear a non-nil pointer for an omitted field.
		cfg.Notifications.Telegram.ScannerNotificationsConfigured = nil
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
		migratePoliticalDisclosureTimingConfig(cfg, data)
	}

	migrateModelsConfig(cfg)
	resolveEnvVars(cfg)
	current = cfg
	return cfg, nil
}

// migrateModelsConfig auto-migrates legacy primary/secondary to Phase 12 analysts/arbiter.
func migrateModelsConfig(cfg *Config) {
	m := &cfg.Models
	if len(m.Analysts) > 0 {
		return // already using new format
	}
	if m.Primary.Provider != "" {
		m.Analysts = append([]ModelConfig{m.Primary}, m.Secondary...)
	}
}

// migratePoliticalDisclosureTimingConfig keeps legacy ttl_days-only operator
// configs behaviorally stable after the three disclosure timing controls were
// separated. Explicit new values always win; the migration runs only when a
// political_disclosures block supplied ttl_days but omitted a new field.
func migratePoliticalDisclosureTimingConfig(cfg *Config, raw []byte) {
	var doc struct {
		Scanner struct {
			PoliticalDisclosures map[string]any `yaml:"political_disclosures"`
		} `yaml:"scanner"`
	}
	if yaml.Unmarshal(raw, &doc) != nil {
		return
	}
	values := doc.Scanner.PoliticalDisclosures
	political := &cfg.Scanner.PoliticalDisclosures
	_, political.explicitTriggerLookbackDays = values["trigger_lookback_days"]
	_, political.explicitAnalystEvidenceHorizonDays = values["analyst_evidence_horizon_days"]
	_, political.explicitWatchlistLeaseDays = values["watchlist_lease_days"]
	_, political.explicitWatchlistLeaseSessions = values["watchlist_lease_sessions"]
	if !political.explicitWatchlistLeaseSessions &&
		(political.explicitWatchlistLeaseDays || values["ttl_days"] != nil) {
		// Keep older operator files on their explicitly configured calendar-day
		// lease until they opt into the session-based field.
		political.WatchlistLeaseSessions = 0
	}
	if len(values) == 0 {
		return
	}
	if _, hasLegacy := values["ttl_days"]; !hasLegacy {
		return
	}
	if _, explicit := values["trigger_lookback_days"]; !explicit {
		political.TriggerLookbackDays = political.TTLDays
	}
	if _, explicit := values["analyst_evidence_horizon_days"]; !explicit {
		political.AnalystEvidenceHorizonDays = political.TTLDays
	}
	if _, explicit := values["watchlist_lease_days"]; !explicit {
		political.WatchlistLeaseDays = political.TTLDays
	}
}

// Get returns the current config. Panics if Load was not called.
func Get() *Config {
	mu.RLock()
	defer mu.RUnlock()
	if current == nil {
		panic("config.Load() must be called before config.Get()")
	}
	return current
}

// Loaded reports whether Load has populated the global config. Callers that can
// run in processes which never call Load (e.g. the standalone `te web` server)
// must check this before Get to avoid the panic.
func Loaded() bool {
	mu.RLock()
	defer mu.RUnlock()
	return current != nil
}

// SetField updates a single dotted-path key in the config YAML file.
func SetField(dotPath, value string) error {
	return SetFieldAny(dotPath, parseScalarString(value))
}

func parseScalarString(value string) any {
	if b, err := strconv.ParseBool(value); err == nil {
		return b
	}
	if i, err := strconv.ParseInt(value, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		return f
	}
	return value
}

// SetFieldAny updates a single dotted-path key in the config YAML file.
// Supports scalar and sequence values.
func SetFieldAny(dotPath string, value any) error {
	data, err := os.ReadFile(ConfigPath())
	var node yaml.Node
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read config: %w", err)
	}
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &node); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
	}

	parts := strings.Split(dotPath, ".")
	if len(node.Content) == 0 {
		node = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	setYAMLNode(node.Content[0], parts, value)

	out, err := yaml.Marshal(&node)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(ConfigPath(), out, 0600)
}

// Validate checks the loaded config for common errors.
// validBreakoutConfirmation is the set of accepted ORB breakout_confirmation modes. All are
// live-wired: stop_touch (resting stop), close_1m/close_3m (N closes beyond), retest_swing/
// retest_range (pull back to the level and bounce). "" defaults to stop_touch.
var validBreakoutConfirmation = map[string]bool{
	"": true, "stop_touch": true, "close_1m": true, "close_3m": true,
	"retest_swing": true, "retest_range": true,
}

var validInstrumentStrategy = map[string]bool{
	"": true, "orb": true, "trend_continuation": true, "sweep_reclaim": true, "opening_shock": true,
}

var validTrendContinuationFilter = map[string]bool{
	"": true, "none": true, "aligned": true, "contrarian": true,
}

var validStrategySideMode = map[string]bool{
	"": true, "both": true, "long": true, "short": true,
}

var validSweepReclaimLevelSet = map[string]bool{
	"prior_day": true, "overnight": true,
}

func validRangeBandSpec(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	for _, raw := range strings.Split(s, ",") {
		part := strings.TrimSpace(raw)
		if part == "" {
			return fmt.Errorf("empty band")
		}
		lo, hi, ok := strings.Cut(part, ":")
		if !ok {
			return fmt.Errorf("%q must use min:max", part)
		}
		min, err := strconv.ParseFloat(strings.TrimSpace(lo), 64)
		if err != nil {
			return fmt.Errorf("%q min is not a number", part)
		}
		max, err := strconv.ParseFloat(strings.TrimSpace(hi), 64)
		if err != nil {
			return fmt.Errorf("%q max is not a number", part)
		}
		if min < 0 || max <= 0 {
			return fmt.Errorf("%q bounds must be non-negative with max > 0", part)
		}
		if min >= max {
			return fmt.Errorf("%q min must be less than max", part)
		}
	}
	return nil
}

func validateInstrumentStrategyParams(prefix string, inst Instrument, defaultRR float64) []string {
	var errs []string
	if !validInstrumentStrategy[inst.Strategy] {
		errs = append(errs, fmt.Sprintf(
			"%s instrument %q: strategy %q must be one of orb|trend_continuation|sweep_reclaim|opening_shock",
			prefix, inst.Symbol, inst.Strategy))
	}
	if !validStrategySideMode[inst.SideMode] {
		errs = append(errs, fmt.Sprintf(
			"%s instrument %q: side_mode %q must be one of both|long|short",
			prefix, inst.Symbol, inst.SideMode))
	}
	if inst.RangeMinutes < 0 {
		errs = append(errs, fmt.Sprintf(
			"%s instrument %q: range_minutes must not be negative",
			prefix, inst.Symbol))
	}
	if inst.RiskReward < 0 {
		errs = append(errs, fmt.Sprintf(
			"%s instrument %q: risk_reward %.2f must not be negative",
			prefix, inst.Symbol, inst.RiskReward))
	}
	if inst.ContinuationDelayMinutes < 0 || inst.ContinuationConfirmBars < 0 || inst.ContinuationTrendLookbackMin < 0 {
		errs = append(errs, fmt.Sprintf(
			"%s instrument %q: trend-continuation minute/bar parameters must not be negative",
			prefix, inst.Symbol))
	}
	if inst.ContinuationDepthMult < 0 || inst.ContinuationMinRangePips < 0 || inst.ContinuationMaxRangePips < 0 || inst.ContinuationMonthlyStopLossUSD < 0 {
		errs = append(errs, fmt.Sprintf(
			"%s instrument %q: trend-continuation numeric thresholds must not be negative",
			prefix, inst.Symbol))
	}
	if inst.ContinuationMaxRangePips > 0 && inst.ContinuationMinRangePips > inst.ContinuationMaxRangePips {
		errs = append(errs, fmt.Sprintf(
			"%s instrument %q: continuation_min_range_pips must not exceed continuation_max_range_pips",
			prefix, inst.Symbol))
	}
	if !validTrendContinuationFilter[inst.ContinuationTrendFilter] {
		errs = append(errs, fmt.Sprintf(
			"%s instrument %q: continuation_trend_filter %q must be one of none|aligned|contrarian",
			prefix, inst.Symbol, inst.ContinuationTrendFilter))
	}
	if err := validRangeBandSpec(inst.ContinuationExcludeRangePips); err != nil {
		errs = append(errs, fmt.Sprintf(
			"%s instrument %q: continuation_exclude_range_pips invalid: %v",
			prefix, inst.Symbol, err))
	}
	if inst.Strategy == "trend_continuation" {
		if inst.ContinuationDelayMinutes <= 0 {
			errs = append(errs, fmt.Sprintf(
				"%s instrument %q: continuation_delay_minutes must be > 0 for trend_continuation",
				prefix, inst.Symbol))
		}
		if inst.ContinuationConfirmBars <= 0 {
			errs = append(errs, fmt.Sprintf(
				"%s instrument %q: continuation_confirm_bars must be > 0 for trend_continuation",
				prefix, inst.Symbol))
		}
		if inst.ContinuationDepthMult <= 0 {
			errs = append(errs, fmt.Sprintf(
				"%s instrument %q: continuation_depth_mult must be > 0 for trend_continuation",
				prefix, inst.Symbol))
		}
		if (inst.ContinuationTrendFilter == "aligned" || inst.ContinuationTrendFilter == "contrarian") && inst.ContinuationTrendLookbackMin <= 0 {
			errs = append(errs, fmt.Sprintf(
				"%s instrument %q: continuation_trend_lookback_minutes must be > 0 when continuation_trend_filter is %q",
				prefix, inst.Symbol, inst.ContinuationTrendFilter))
		}
	}
	if inst.Strategy == "sweep_reclaim" {
		if !validSweepReclaimLevelSet[inst.SweepReclaimLevelSet] {
			errs = append(errs, fmt.Sprintf(
				"%s instrument %q: sweep_reclaim_level_set must be prior_day or overnight",
				prefix, inst.Symbol))
		}
		if inst.SweepReclaimDepthMult <= 0 {
			errs = append(errs, fmt.Sprintf(
				"%s instrument %q: sweep_reclaim_depth_mult must be > 0 for sweep_reclaim",
				prefix, inst.Symbol))
		}
		if inst.SweepReclaimEntryWindowMinutes <= 0 {
			errs = append(errs, fmt.Sprintf(
				"%s instrument %q: sweep_reclaim_entry_window_minutes must be > 0 for sweep_reclaim",
				prefix, inst.Symbol))
		}
	}
	if inst.Strategy == "opening_shock" {
		if inst.OpeningShockWindowMinutes <= 0 {
			errs = append(errs, fmt.Sprintf(
				"%s instrument %q: opening_shock_window_minutes must be > 0 for opening_shock",
				prefix, inst.Symbol))
		}
		if inst.OpeningShockLookback <= 0 {
			errs = append(errs, fmt.Sprintf(
				"%s instrument %q: opening_shock_lookback must be > 0 for opening_shock",
				prefix, inst.Symbol))
		}
		if inst.OpeningShockPercentile <= 0 || inst.OpeningShockPercentile >= 100 {
			errs = append(errs, fmt.Sprintf(
				"%s instrument %q: opening_shock_percentile must be in (0,100)",
				prefix, inst.Symbol))
		}
		if inst.OpeningShockStopMult <= 0 {
			errs = append(errs, fmt.Sprintf(
				"%s instrument %q: opening_shock_stop_mult must be > 0 for opening_shock",
				prefix, inst.Symbol))
		}
		if inst.OpeningShockEntryDelayMin < 0 {
			errs = append(errs, fmt.Sprintf(
				"%s instrument %q: opening_shock_entry_delay_minutes must not be negative",
				prefix, inst.Symbol))
		}
	}
	if (inst.Strategy == "trend_continuation" || inst.Strategy == "sweep_reclaim" || inst.Strategy == "opening_shock") && inst.RiskReward <= 0 && defaultRR <= 0 {
		errs = append(errs, fmt.Sprintf(
			"%s instrument %q: risk_reward or strategy default risk_reward must be > 0 for %s",
			prefix, inst.Symbol, inst.Strategy))
	}
	return errs
}

func validUniverseSnapshotFeed(feed string) bool {
	return feed == "iex" || feed == "sip" || feed == "delayed_sip"
}

func validUniverseHistoricalFeed(feed string) bool {
	return feed == "iex" || feed == "sip"
}

const delayedSIPSnapshotDelayMinutes = 16

func Validate(cfg *Config) []string {
	var errs []string
	if cfg.Engine.MaxWatchlist <= 0 {
		errs = append(errs, "engine.max_watchlist must be > 0")
	}
	queue := cfg.Scanner.ResearchQueue
	if queue.Capacity <= 0 {
		errs = append(errs, "scanner.research_queue.capacity must be > 0")
	}
	if queue.Overflow < 0 {
		errs = append(errs, "scanner.research_queue.overflow must be >= 0")
	}
	for name, value := range map[string]int{
		"political_disclosure": queue.Reservations.PoliticalDisclosure,
		"weekly_leader":        queue.Reservations.WeeklyLeader,
		"potent_industry":      queue.Reservations.PotentIndustry,
		"market_activity":      queue.Reservations.MarketActivity,
		"premarket_episodic":   queue.Reservations.PremarketEpisodic,
		"graph_neighbor":       queue.Reservations.GraphNeighbor,
		"news_sympathy":        queue.Reservations.NewsSympathy,
		"earnings_scheduled":   queue.Reservations.EarningsScheduled,
	} {
		if value < 0 {
			errs = append(errs, "scanner.research_queue.reservations."+name+" must be >= 0")
		}
	}
	if queue.Capacity > 0 && queue.Reservations.Total()+queue.Overflow != queue.Capacity {
		errs = append(errs, "scanner.research_queue.capacity must equal reservations plus overflow")
	}
	staging := cfg.Scanner.ResearchStaging
	if staging.Capacity <= 0 {
		errs = append(errs, "scanner.research_staging.capacity must be > 0")
	}
	if staging.LeaseMinutes <= 0 {
		errs = append(errs, "scanner.research_staging.lease_minutes must be > 0")
	} else if !durationConfigFits(staging.LeaseMinutes, time.Minute) {
		errs = append(errs, "scanner.research_staging.lease_minutes must be a safe positive duration")
	}
	if cfg.Scanner.PromotionMaxPerIndustry < 0 {
		errs = append(errs, "scanner.max_promotions_per_industry must be >= 0")
	}
	if cfg.Scanner.PromotionMaxAbsCorrelation <= 0 || cfg.Scanner.PromotionMaxAbsCorrelation > 1 ||
		math.IsNaN(cfg.Scanner.PromotionMaxAbsCorrelation) || math.IsInf(cfg.Scanner.PromotionMaxAbsCorrelation, 0) {
		errs = append(errs, "scanner.promotion_max_abs_correlation must be in (0,1]")
	}
	if cfg.Scanner.Universe.Enabled || cfg.Scanner.PromotionCorrelationEnabled || cfg.Scanner.EarningsWatchlist.Enabled {
		delay := cfg.Scanner.Universe.SIPRecentDataDelayMinutes
		if delay < 0 {
			errs = append(errs,
				"scanner.universe.sip_recent_data_delay_minutes must be >= 0")
		} else if !durationConfigFits(delay, time.Minute) {
			errs = append(errs,
				"scanner.universe.sip_recent_data_delay_minutes overflows time.Duration")
		}
	}

	validModes := map[string]bool{
		"manual": true, "auto": true, "paper": true,
		"forex": true, "orb-forex": true, "orb-futures": true,
		"orb-futures-metals": true, "orb-newyork": true, "orb-options": true,
	}
	if !validModes[cfg.Engine.Mode] {
		errs = append(errs, fmt.Sprintf("engine.mode must be one of manual|auto|paper|forex|orb-forex|orb-futures|orb-futures-metals|orb-newyork|orb-options, got %q", cfg.Engine.Mode))
	}

	if cfg.Engine.LogTimezone != "" {
		if _, err := time.LoadLocation(cfg.Engine.LogTimezone); err != nil {
			errs = append(errs, fmt.Sprintf("engine.log_timezone %q is not a valid IANA timezone: %v", cfg.Engine.LogTimezone, err))
		}
	}

	// Validate classifier mode — unknown values silently default to "shadow" at
	// runtime (Gate.NewGate), but an explicit validation error is clearer.
	validClassifierModes := map[string]bool{"disabled": true, "shadow": true, "active": true}
	if mode := cfg.Analysis.Classifier.Mode; mode != "" && !validClassifierModes[mode] {
		errs = append(errs, fmt.Sprintf("analysis.classifier.mode must be one of disabled|shadow|active, got %q", mode))
	}

	if universe := cfg.Scanner.Universe; universe.Enabled {
		if !cfg.Scanner.Enabled {
			errs = append(errs, "scanner.universe requires scanner.enabled=true")
		}
		if universe.AfterCloseDelayMinutes < 0 {
			errs = append(errs, "scanner.universe.after_close_delay_minutes must be >= 0")
		} else if !durationConfigFits(universe.AfterCloseDelayMinutes, time.Minute) {
			errs = append(errs, "scanner.universe.after_close_delay_minutes overflows time.Duration")
		}
		if universe.RunLeaseMinutes <= 0 {
			errs = append(errs, "scanner.universe.run_lease_minutes must be > 0")
		} else if !durationConfigFits(universe.RunLeaseMinutes, time.Minute) {
			errs = append(errs, "scanner.universe.run_lease_minutes overflows time.Duration")
		}
		if math.IsNaN(universe.MinMarketDataCoverage) ||
			math.IsInf(universe.MinMarketDataCoverage, 0) ||
			universe.MinMarketDataCoverage <= 0 || universe.MinMarketDataCoverage > 1 {
			errs = append(errs, "scanner.universe.min_market_data_coverage must be > 0 and <= 1")
		}
		validUniverseProviders := map[string]bool{"alpaca": true}
		if !validUniverseProviders[universe.Provider] {
			errs = append(errs, fmt.Sprintf(
				"scanner.universe.provider must be one of alpaca, got %q",
				universe.Provider))
		}
		if !validUniverseSnapshotFeed(universe.SnapshotFeed) {
			errs = append(errs, fmt.Sprintf(
				"scanner.universe.snapshot_feed must be one of iex|sip|delayed_sip, got %q",
				universe.SnapshotFeed))
		}
		if !validUniverseHistoricalFeed(universe.HistoricalFeed) {
			errs = append(errs, fmt.Sprintf(
				"scanner.universe.historical_feed must be one of iex|sip, got %q",
				universe.HistoricalFeed))
		}
		requiredFeedDelayMinutes := 0
		if universe.SnapshotFeed == "delayed_sip" {
			requiredFeedDelayMinutes = delayedSIPSnapshotDelayMinutes
		}
		if universe.HistoricalFeed == "sip" &&
			universe.SIPRecentDataDelayMinutes > requiredFeedDelayMinutes {
			requiredFeedDelayMinutes = universe.SIPRecentDataDelayMinutes
		}
		if universe.AfterCloseDelayMinutes < requiredFeedDelayMinutes {
			errs = append(errs, fmt.Sprintf(
				"scanner.universe.after_close_delay_minutes must be >= %d for the configured market-data delays",
				requiredFeedDelayMinutes))
		}
		if universe.MinPrice < 0 {
			errs = append(errs, "scanner.universe.min_price must be >= 0")
		}
		if universe.MinMarketCapMM < 0 {
			errs = append(errs, "scanner.universe.min_market_cap_mm must be >= 0")
		}
		if universe.MinMedianDollarVolume < 0 {
			errs = append(errs, "scanner.universe.min_median_dollar_volume must be >= 0")
		}
		if universe.DollarVolumeLookbackDays <= 0 {
			errs = append(errs, "scanner.universe.dollar_volume_lookback_days must be > 0")
		}
		if len(universe.AllowedSecurityTypes) == 0 {
			errs = append(errs, "scanner.universe.allowed_security_types must not be empty")
		} else {
			for _, securityType := range universe.AllowedSecurityTypes {
				if strings.TrimSpace(securityType) == "" {
					errs = append(errs, "scanner.universe.allowed_security_types must not contain blank values")
					break
				}
			}
		}
		if len(universe.AllowedExchanges) == 0 {
			errs = append(errs, "scanner.universe.allowed_exchanges must not be empty")
		} else {
			for _, exchange := range universe.AllowedExchanges {
				if strings.TrimSpace(exchange) == "" {
					errs = append(errs, "scanner.universe.allowed_exchanges must not contain blank values")
					break
				}
			}
		}
		for _, pattern := range universe.ExcludedNamePatterns {
			if strings.TrimSpace(pattern) == "" {
				errs = append(errs, "scanner.universe.excluded_name_patterns must not contain blank values")
				break
			}
			if _, err := regexp.Compile(pattern); err != nil {
				errs = append(errs, fmt.Sprintf("scanner.universe.excluded_name_patterns contains invalid pattern %q: %v", pattern, err))
			}
		}
		if universe.MaxSpreadBPS < 0 {
			errs = append(errs, "scanner.universe.max_spread_bps must be >= 0")
		}
		if universe.SnapshotTTLHours <= 0 {
			errs = append(errs, "scanner.universe.snapshot_ttl_hours must be > 0")
		}
		if universe.TaxonomyMaxAgeHours <= 0 {
			errs = append(errs, "scanner.universe.taxonomy_max_age_hours must be > 0")
		}
		if universe.BatchSize <= 0 {
			errs = append(errs, "scanner.universe.batch_size must be > 0")
		}
		if universe.MaxConcurrency <= 0 {
			errs = append(errs, "scanner.universe.max_concurrency must be > 0")
		}
		if universe.TaxonomyRefreshLimit <= 0 {
			errs = append(errs, "scanner.universe.taxonomy_refresh_limit must be > 0")
		}
		validMissingPolicy := func(policy string) bool {
			return policy == "exclude" || policy == "include"
		}
		if !validMissingPolicy(universe.MissingFundamentalsPolicy) {
			errs = append(errs, fmt.Sprintf(
				"scanner.universe.missing_fundamentals_policy must be one of exclude|include, got %q",
				universe.MissingFundamentalsPolicy))
		}
		if !validMissingPolicy(universe.MissingMarketDataPolicy) {
			errs = append(errs, fmt.Sprintf(
				"scanner.universe.missing_market_data_policy must be one of exclude|include, got %q",
				universe.MissingMarketDataPolicy))
		}
	}
	if leader := cfg.Scanner.WeeklyLeader; leader.Enabled {
		if !cfg.Scanner.Enabled {
			errs = append(errs, "scanner.weekly_leader requires scanner.enabled=true")
		}
		if !cfg.Scanner.Universe.Enabled {
			errs = append(errs, "scanner.weekly_leader requires scanner.universe.enabled=true")
		} else if cfg.Scanner.Universe.AfterCloseDelayMinutes < leader.AfterCloseDelayMinutes {
			errs = append(errs, "scanner.universe.after_close_delay_minutes must be >= scanner.weekly_leader.after_close_delay_minutes")
		}
		if leader.AfterCloseDelayMinutes < 0 {
			errs = append(errs, "scanner.weekly_leader.after_close_delay_minutes must be >= 0")
		} else if !durationConfigFits(leader.AfterCloseDelayMinutes, time.Minute) {
			errs = append(errs,
				"scanner.weekly_leader.after_close_delay_minutes overflows time.Duration")
		}
		if leader.MaxPromotionsPerRun <= 0 {
			errs = append(errs, "scanner.weekly_leader.max_promotions_per_run must be > 0")
		}
		if leader.TTLDays <= 0 {
			errs = append(errs, "scanner.weekly_leader.ttl_days must be > 0")
		}
		if leader.UniverseMaxAgeHours <= 0 {
			errs = append(errs, "scanner.weekly_leader.universe_max_age_hours must be > 0")
		} else if !durationConfigFits(leader.UniverseMaxAgeHours, time.Hour) {
			errs = append(errs,
				"scanner.weekly_leader.universe_max_age_hours overflows time.Duration")
		}
		if leader.HistoryCalendarDays < 400 {
			errs = append(errs, "scanner.weekly_leader.history_calendar_days must be >= 400")
		}
		if leader.MinHistorySessions < 253 {
			errs = append(errs, "scanner.weekly_leader.min_history_sessions must be >= 253")
		}
		requiredCalendarDays, historyWindowOK := weeklyLeaderHistoryCalendarDays(
			leader.MinHistorySessions,
		)
		if !historyWindowOK {
			errs = append(errs,
				"scanner.weekly_leader.min_history_sessions is too large to convert to a safe calendar window")
		} else if leader.MinHistorySessions > 0 &&
			leader.HistoryCalendarDays < requiredCalendarDays {
			errs = append(errs, fmt.Sprintf(
				"scanner.weekly_leader.history_calendar_days must be >= %d for min_history_sessions=%d",
				requiredCalendarDays, leader.MinHistorySessions,
			))
		}
		if leader.HistoryCalendarDays > weeklyLeaderMaxHistoryCalendarDays {
			errs = append(errs, fmt.Sprintf(
				"scanner.weekly_leader.history_calendar_days must be <= %d",
				weeklyLeaderMaxHistoryCalendarDays))
		}
		if strings.TrimSpace(leader.Benchmark) == "" {
			errs = append(errs, "scanner.weekly_leader.benchmark must not be blank")
		}
		if leader.IndustryMinPeers <= 0 {
			errs = append(errs, "scanner.weekly_leader.industry_min_peers must be > 0")
		}
		if leader.SelectionPercentile <= 0 || leader.SelectionPercentile >= 1 {
			errs = append(errs, "scanner.weekly_leader.selection_percentile must be between 0 and 1")
		}
		if leader.MinCompositeScore <= 0 || leader.MinCompositeScore > 1 {
			errs = append(errs, "scanner.weekly_leader.min_composite_score must be in (0,1]")
		}
		if leader.MinFeatureCoverage <= 0 || leader.MinFeatureCoverage > 1 {
			errs = append(errs, "scanner.weekly_leader.min_feature_coverage must be in (0,1]")
		}
		if math.IsNaN(leader.MinCrossSectionCoverage) ||
			math.IsInf(leader.MinCrossSectionCoverage, 0) ||
			leader.MinCrossSectionCoverage <= 0 || leader.MinCrossSectionCoverage > 1 {
			errs = append(errs,
				"scanner.weekly_leader.min_cross_section_coverage must be in (0,1]")
		}
		if leader.BatchSize <= 0 {
			errs = append(errs, "scanner.weekly_leader.batch_size must be > 0")
		}
		if leader.RunLeaseMinutes <= 0 {
			errs = append(errs, "scanner.weekly_leader.run_lease_minutes must be > 0")
		} else if !durationConfigFits(leader.RunLeaseMinutes, time.Minute) {
			errs = append(errs,
				"scanner.weekly_leader.run_lease_minutes overflows time.Duration")
		}
	}
	if industry := cfg.Scanner.PotentIndustry; industry.Enabled && industry.TTLDays <= 0 {
		errs = append(errs, "scanner.potent_industry.ttl_days must be > 0")
	}
	if industry := cfg.Scanner.PotentIndustry; industry.Enabled {
		const prefix = "scanner.potent_industry"
		if !cfg.Scanner.Enabled {
			errs = append(errs, prefix+" requires scanner.enabled=true")
		}
		if !cfg.Scanner.Universe.Enabled {
			errs = append(errs, prefix+" requires scanner.universe.enabled=true")
		} else if cfg.Scanner.Universe.AfterCloseDelayMinutes < industry.AfterCloseDelayMinutes {
			errs = append(errs, "scanner.universe.after_close_delay_minutes must be >= "+prefix+".after_close_delay_minutes")
		}
		if industry.AfterCloseDelayMinutes < 0 {
			errs = append(errs, prefix+".after_close_delay_minutes must be >= 0")
		} else if !durationConfigFits(industry.AfterCloseDelayMinutes, time.Minute) {
			errs = append(errs, prefix+".after_close_delay_minutes overflows time.Duration")
		}
		if industry.UniverseMaxAgeHours <= 0 {
			errs = append(errs, prefix+".universe_max_age_hours must be > 0")
		} else if !durationConfigFits(industry.UniverseMaxAgeHours, time.Hour) {
			errs = append(errs, prefix+".universe_max_age_hours overflows time.Duration")
		}
		if industry.TaxonomyMaxAgeHours <= 0 {
			errs = append(errs, prefix+".taxonomy_max_age_hours must be > 0")
		} else if !durationConfigFits(industry.TaxonomyMaxAgeHours, time.Hour) {
			errs = append(errs, prefix+".taxonomy_max_age_hours overflows time.Duration")
		}
		if industry.RunLeaseMinutes <= 0 {
			errs = append(errs, prefix+".run_lease_minutes must be > 0")
		} else if !durationConfigFits(industry.RunLeaseMinutes, time.Minute) {
			errs = append(errs, prefix+".run_lease_minutes overflows time.Duration")
		}
		if industry.HistoryCalendarDays < 400 {
			errs = append(errs, prefix+".history_calendar_days must be >= 400")
		}
		if industry.MinHistorySessions < 253 {
			errs = append(errs, prefix+".min_history_sessions must be >= 253")
		}
		requiredCalendarDays, historyWindowOK := weeklyLeaderHistoryCalendarDays(
			industry.MinHistorySessions,
		)
		if !historyWindowOK {
			errs = append(errs, prefix+
				".min_history_sessions is too large to convert to a safe calendar window")
		} else if industry.MinHistorySessions > 0 &&
			industry.HistoryCalendarDays < requiredCalendarDays {
			errs = append(errs, fmt.Sprintf(
				"%s.history_calendar_days must be >= %d for min_history_sessions=%d",
				prefix, requiredCalendarDays, industry.MinHistorySessions))
		}
		if industry.HistoryCalendarDays > weeklyLeaderMaxHistoryCalendarDays {
			errs = append(errs, fmt.Sprintf(
				"%s.history_calendar_days must be <= %d",
				prefix, weeklyLeaderMaxHistoryCalendarDays))
		}
		if strings.TrimSpace(industry.MarketBenchmark) == "" ||
			industry.MarketBenchmark != strings.TrimSpace(industry.MarketBenchmark) ||
			industry.MarketBenchmark != strings.ToUpper(industry.MarketBenchmark) {
			errs = append(errs, prefix+".market_benchmark must be a normalized nonblank symbol")
		}
		validateBenchmarkMap := func(field string, values map[string]string, allowEmpty bool) {
			if len(values) == 0 && !allowEmpty {
				errs = append(errs, prefix+"."+field+" must not be empty")
				return
			}
			normalizedGroups := make(map[string]struct{}, len(values))
			for group, benchmark := range values {
				if strings.TrimSpace(group) == "" || strings.TrimSpace(benchmark) == "" ||
					group != strings.TrimSpace(group) ||
					benchmark != strings.TrimSpace(benchmark) ||
					benchmark != strings.ToUpper(benchmark) {
					errs = append(errs, prefix+"."+field+
						" must contain normalized nonblank group and benchmark values")
					return
				}
				normalizedGroup := normalizedBenchmarkLabel(group)
				if _, duplicate := normalizedGroups[normalizedGroup]; duplicate {
					errs = append(errs, prefix+"."+field+
						" must not contain duplicate normalized group labels")
					return
				}
				normalizedGroups[normalizedGroup] = struct{}{}
			}
		}
		validateBenchmarkMap("sector_benchmarks", industry.SectorBenchmarks, false)
		validateBenchmarkMap(
			"industry_benchmark_overrides", industry.IndustryBenchmarkOverrides, true,
		)
		validateUnitInterval := func(field string, value float64, inclusiveZero bool) {
			valid := !math.IsNaN(value) && !math.IsInf(value, 0) &&
				value > 0 && value <= 1
			if inclusiveZero {
				valid = !math.IsNaN(value) && !math.IsInf(value, 0) &&
					value >= 0 && value <= 1
			}
			if !valid {
				interval := "(0,1]"
				if inclusiveZero {
					interval = "[0,1]"
				}
				errs = append(errs, prefix+"."+field+" must be in "+interval)
			}
		}
		validateUnitInterval(
			"min_fresh_taxonomy_coverage", industry.MinFreshTaxonomyCoverage, false,
		)
		validateUnitInterval("min_group_coverage", industry.MinGroupCoverage, false)
		validateUnitInterval("min_potency_score", industry.MinPotencyScore, false)
		validateUnitInterval("min_member_score", industry.MinMemberScore, false)
		if math.IsNaN(industry.SelectionPercentile) ||
			math.IsInf(industry.SelectionPercentile, 0) ||
			industry.SelectionPercentile <= 0 || industry.SelectionPercentile >= 1 {
			errs = append(errs, prefix+".selection_percentile must be between 0 and 1")
		}
		validateUnitInterval("min_advance_share", industry.MinAdvanceShare, false)
		if math.IsNaN(industry.MinMedianReturn5D) ||
			math.IsInf(industry.MinMedianReturn5D, 0) ||
			industry.MinMedianReturn5D < -1 {
			errs = append(errs, prefix+".min_median_return_5d must be >= -1")
		}
		if math.IsNaN(industry.MaxCapEqualDivergence) ||
			math.IsInf(industry.MaxCapEqualDivergence, 0) ||
			industry.MaxCapEqualDivergence <= 0 {
			errs = append(errs, prefix+".max_cap_equal_divergence must be > 0")
		}
		for field, value := range map[string]int{
			"max_promotions_per_run":   industry.MaxPromotionsPerRun,
			"min_group_members":        industry.MinGroupMembers,
			"min_persistence_sessions": industry.MinPersistenceSessions,
			"max_members_per_group":    industry.MaxMembersPerGroup,
			"batch_size":               industry.BatchSize,
		} {
			if value <= 0 {
				errs = append(errs, prefix+"."+field+" must be > 0")
			}
		}
		if industry.MinPersistenceSessions > 5 {
			errs = append(errs,
				prefix+".min_persistence_sessions must be <= 5")
		}
	}
	if premarket := cfg.Scanner.PremarketEpisodic; premarket.Enabled {
		const prefix = "scanner.premarket_episodic"
		if !cfg.Scanner.Enabled {
			errs = append(errs, prefix+" requires scanner.enabled=true")
		}
		if !cfg.Scanner.Universe.Enabled {
			errs = append(errs, prefix+" requires scanner.universe.enabled=true")
		}
		if strings.ToLower(strings.TrimSpace(premarket.Feed)) != "sip" {
			errs = append(errs, prefix+".feed must be sip; executable premarket liquidity cannot use IEX")
		}
		cutoff, cutoffErr := time.Parse("15:04", premarket.HardCutoff)
		if cutoffErr != nil || cutoff.Format("15:04") != premarket.HardCutoff {
			errs = append(errs, prefix+".hard_cutoff must use HH:MM")
		} else {
			open, _ := time.Parse("15:04", "09:30")
			if !cutoff.Before(open) {
				errs = append(errs, prefix+".hard_cutoff must be before 09:30")
			}
		}
		if len(premarket.Checkpoints) == 0 {
			errs = append(errs, prefix+".checkpoints must not be empty")
		}
		seenCheckpoints := make(map[string]struct{}, len(premarket.Checkpoints))
		for _, raw := range premarket.Checkpoints {
			checkpoint, err := time.Parse("15:04", raw)
			if err != nil || checkpoint.Format("15:04") != raw {
				errs = append(errs, prefix+".checkpoints must use HH:MM")
				break
			}
			if _, duplicate := seenCheckpoints[raw]; duplicate {
				errs = append(errs, prefix+".checkpoints must be unique")
				break
			}
			seenCheckpoints[raw] = struct{}{}
			premarketOpen, _ := time.Parse("15:04", "04:00")
			if checkpoint.Before(premarketOpen) {
				errs = append(errs, prefix+".checkpoints must be at or after 04:00")
				break
			}
			if cutoffErr == nil && !checkpoint.Before(cutoff) {
				errs = append(errs, prefix+".checkpoints must precede hard_cutoff")
				break
			}
		}
		for field, value := range map[string]int{
			"universe_max_age_hours": premarket.UniverseMaxAgeHours,
			"history_sessions":       premarket.HistorySessions,
			"news_lookback_hours":    premarket.NewsLookbackHours,
			"max_candidates_per_run": premarket.MaxCandidatesPerRun,
			"max_promotions_per_run": premarket.MaxPromotionsPerRun,
			"max_quote_age_seconds":  premarket.MaxQuoteAgeSeconds,
			"batch_size":             premarket.BatchSize,
			"run_lease_minutes":      premarket.RunLeaseMinutes,
		} {
			if value <= 0 {
				errs = append(errs, prefix+"."+field+" must be > 0")
			}
		}
		for field, duration := range map[string]struct {
			value int
			unit  time.Duration
		}{
			"universe_max_age_hours": {premarket.UniverseMaxAgeHours, time.Hour},
			"news_lookback_hours":    {premarket.NewsLookbackHours, time.Hour},
			"max_quote_age_seconds":  {premarket.MaxQuoteAgeSeconds, time.Second},
			"run_lease_minutes":      {premarket.RunLeaseMinutes, time.Minute},
		} {
			if duration.value > 0 && !durationConfigFits(duration.value, duration.unit) {
				errs = append(errs, prefix+"."+field+" overflows time.Duration")
			}
		}
		for field, value := range map[string]float64{
			"min_gap_pct":            premarket.MinGapPct,
			"min_time_adjusted_rvol": premarket.MinTimeAdjustedRVOL,
			"min_dollar_volume":      premarket.MinDollarVolume,
			"max_spread_bps":         premarket.MaxSpreadBPS,
			"min_price":              premarket.MinPrice,
			"min_market_cap_mm":      premarket.MinMarketCapMM,
		} {
			if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
				errs = append(errs, prefix+"."+field+" must be > 0")
			}
		}
		if premarket.MinPremarketVolume <= 0 {
			errs = append(errs, prefix+".min_premarket_volume must be > 0")
		}
		if premarket.MinTradeCount <= 0 {
			errs = append(errs, prefix+".min_trade_count must be > 0")
		}
		if premarket.MinBarCoverage <= 0 || premarket.MinBarCoverage > 1 ||
			math.IsNaN(premarket.MinBarCoverage) {
			errs = append(errs, prefix+".min_bar_coverage must be in (0,1]")
		}
	}
	if earnings := cfg.Scanner.EarningsWatchlist; earnings.Enabled {
		const prefix = "scanner.earnings_watchlist"
		if !validUniverseHistoricalFeed(cfg.Scanner.Universe.HistoricalFeed) {
			errs = append(errs, fmt.Sprintf(
				"scanner.universe.historical_feed must be one of iex|sip when earnings watchlist is enabled, got %q",
				cfg.Scanner.Universe.HistoricalFeed))
		}
		if earnings.AfterCloseDelayMinutes < 0 {
			errs = append(errs, prefix+".after_close_delay_minutes must be >= 0")
		} else if !durationConfigFits(earnings.AfterCloseDelayMinutes, time.Minute) {
			errs = append(errs, prefix+".after_close_delay_minutes overflows time.Duration")
		}
		if cfg.Scanner.Universe.HistoricalFeed == "sip" &&
			earnings.AfterCloseDelayMinutes < cfg.Scanner.Universe.SIPRecentDataDelayMinutes {
			errs = append(errs, prefix+".after_close_delay_minutes must be >= scanner.universe.sip_recent_data_delay_minutes when historical_feed=sip")
		}
		for field, value := range map[string]int{
			"candidate_cap":      earnings.CandidateCap,
			"watchlist_sessions": earnings.WatchlistSessions,
			"bar_batch_size":     earnings.BarBatchSize,
		} {
			if value <= 0 {
				errs = append(errs, prefix+"."+field+" must be > 0")
			}
		}
		if earnings.CandidateCap > 0 {
			effectiveCandidateCap := earnings.CandidateCap
			if effectiveCandidateCap > 5 {
				effectiveCandidateCap = 5
			}
			if cfg.Engine.MaxWatchlist < effectiveCandidateCap {
				errs = append(errs, fmt.Sprintf(
					"engine.max_watchlist must be >= effective earnings candidate cap (%d) when scanner.earnings_watchlist.enabled=true", effectiveCandidateCap))
			}
			if cfg.Scanner.ResearchStaging.Capacity < effectiveCandidateCap {
				errs = append(errs, fmt.Sprintf(
					"scanner.research_staging.capacity must be >= effective earnings candidate cap (%d) when scanner.earnings_watchlist.enabled=true", effectiveCandidateCap))
			}
		}
	}
	errs = append(errs, ValidateDisclosuresConfig(cfg.Disclosures)...)
	if political := cfg.Scanner.PoliticalDisclosures; political.Enabled {
		const prefix = "scanner.political_disclosures"
		if !cfg.Scanner.Enabled {
			errs = append(errs, prefix+" requires scanner.enabled=true")
		}
		if !cfg.Disclosures.Enabled {
			errs = append(errs, prefix+" requires disclosures.enabled=true")
		}
		if political.MaxCandidatesPerRun <= 0 {
			errs = append(errs, prefix+".max_candidates_per_run must be > 0")
		}
		if political.ClaimLeaseMinutes <= 0 ||
			!durationConfigFits(political.ClaimLeaseMinutes, time.Minute) {
			errs = append(errs, prefix+".claim_lease_minutes must be a safe positive duration")
		}
		for _, field := range []struct {
			name     string
			value    int
			explicit bool
		}{
			{"trigger_lookback_days", political.TriggerLookbackDays, political.explicitTriggerLookbackDays},
			{"analyst_evidence_horizon_days", political.AnalystEvidenceHorizonDays, political.explicitAnalystEvidenceHorizonDays},
			{"watchlist_lease_days", political.WatchlistLeaseDays, political.explicitWatchlistLeaseDays},
			{"watchlist_lease_sessions", political.WatchlistLeaseSessions, political.explicitWatchlistLeaseSessions},
		} {
			if field.value < 0 || (field.explicit && field.value == 0) {
				errs = append(errs, prefix+"."+field.name+" must be > 0 when set")
			}
		}
		if political.TriggerWindowDays() <= 0 {
			errs = append(errs, prefix+".trigger_lookback_days (or legacy ttl_days) must be > 0")
		}
		if political.EvidenceHorizonDays() <= 0 {
			errs = append(errs, prefix+".analyst_evidence_horizon_days (or legacy ttl_days) must be > 0")
		}
		if political.WatchlistLeaseWindowSessions() <= 0 && political.WatchlistLeaseWindowDays() <= 0 {
			errs = append(errs, prefix+".watchlist_lease_sessions or watchlist_lease_days (or legacy ttl_days) must be > 0")
		}
	}

	// Validate cost profile. Empty string is not valid — Defaults() always sets
	// "balanced", so an empty Profile at validation time means an explicit blank
	// was written in YAML, which would break downstream profile logic.
	validCostProfiles := map[string]bool{"budget": true, "balanced": true, "pro": true}
	if p := cfg.Cost.Profile; !validCostProfiles[p] {
		errs = append(errs, fmt.Sprintf("cost.profile must be one of budget|balanced|pro, got %q", p))
	}

	// Validate logging rotation settings. lumberjack treats 0 as "use default"
	// for MaxSize/MaxAge/MaxBackups, but we require explicit positive values so
	// users don't accidentally disable rotation by zeroing a field.
	if cfg.Logging.MaxSizeMB <= 0 {
		errs = append(errs, fmt.Sprintf("logging.max_size_mb must be > 0, got %d", cfg.Logging.MaxSizeMB))
	}
	if cfg.Logging.MaxBackups < 0 {
		errs = append(errs, fmt.Sprintf("logging.max_backups must be >= 0, got %d", cfg.Logging.MaxBackups))
	}
	if cfg.Logging.MaxAgeDays < 0 {
		errs = append(errs, fmt.Sprintf("logging.max_age_days must be >= 0, got %d", cfg.Logging.MaxAgeDays))
	}

	if cfg.Risk.TrailingStop.ActivateAtGainPct < 0 {
		errs = append(errs, fmt.Sprintf("risk.trailing_stop.activate_at_gain_pct must be >= 0, got %v",
			cfg.Risk.TrailingStop.ActivateAtGainPct))
	}
	trailingRule := strings.ToLower(strings.TrimSpace(cfg.Risk.TrailingStop.Rule))
	if trailingRule != "" && trailingRule != "atr" && trailingRule != "ema9" {
		errs = append(errs, fmt.Sprintf("risk.trailing_stop.rule must be one of atr|ema9, got %q", cfg.Risk.TrailingStop.Rule))
	}
	if cfg.Risk.ExitConfMin < 0 || cfg.Risk.ExitConfMin > 1 {
		errs = append(errs, fmt.Sprintf("risk.exit_conf_min must be within [0,1], got %.4f", cfg.Risk.ExitConfMin))
	}
	entryGates := cfg.Risk.EntryGates
	if entryGates.PullbackMinMomentumReturnPct < 0 || entryGates.PullbackMinMomentumReturnPct > 500 {
		errs = append(errs, fmt.Sprintf("risk.entry_gates.pullback_min_momentum_return_pct must be 0 or within (0,500], got %.4f", entryGates.PullbackMinMomentumReturnPct))
	}
	if n := entryGates.PullbackConsolidationSessions; n != 0 && n < 20 {
		errs = append(errs, fmt.Sprintf("risk.entry_gates.pullback_consolidation_sessions must be 0 or >= 20, got %d", n))
	}
	if v := entryGates.PullbackSupportTolerancePct; v < 0 || v > 10 {
		errs = append(errs, fmt.Sprintf("risk.entry_gates.pullback_support_tolerance_pct must be within [0,10], got %.4f", v))
	}
	if v := entryGates.PullbackMaxStopWidthPct; v < 0 || v > 20 {
		errs = append(errs, fmt.Sprintf("risk.entry_gates.pullback_max_stop_width_pct must be within [0,20], got %.4f", v))
	}
	if anti := entryGates.AntiChase; anti.Enabled {
		validate := func(name string, value, max float64) {
			if value <= 0 || value > max || math.IsNaN(value) || math.IsInf(value, 0) {
				errs = append(errs, fmt.Sprintf("risk.entry_gates.anti_chase.%s must be within (0,%.0f], got %.4f", name, max, value))
			}
		}
		needsWeakVolume := anti.StaleCatalystRunupEnabled || anti.PriorHighExtensionEnabled || anti.MomentumExhaustionEnabled
		if !needsWeakVolume {
			errs = append(errs, "risk.entry_gates.anti_chase.enabled requires at least one enabled rule")
		}
		if anti.StaleCatalystRunupEnabled {
			if anti.StaleAfterHours <= 0 || anti.StaleAfterHours >= 24*30 || math.IsNaN(anti.StaleAfterHours) || math.IsInf(anti.StaleAfterHours, 0) {
				errs = append(errs, fmt.Sprintf("risk.entry_gates.anti_chase.stale_after_hours must be within (0,720), got %.4f", anti.StaleAfterHours))
			}
			validate("min_return_3_session_pct", anti.MinReturn3SessionPct, 200)
			validate("stale_ema_extension_atr", anti.StaleEMAExtensionATR, 20)
		}
		if anti.PriorHighExtensionEnabled {
			validate("near_prior_high_pct", anti.NearPriorHighPct, 100)
			validate("high_ema_extension_atr", anti.HighEMAExtensionATR, 20)
		}
		if anti.MomentumExhaustionEnabled {
			validate("rsi_min", anti.RSIMin, 100)
			validate("stoch_k_min", anti.StochKMin, 100)
			validate("exhaustion_return_3_session_pct", anti.ExhaustionReturn3Pct, 200)
		}
		if needsWeakVolume {
			validate("weak_volume_ratio_max", anti.WeakVolumeRatioMax, 10)
		}
	}

	// Validate cost budgets. Guardrail.Check gates on budget > 0, so negative
	// values would silently disable enforcement rather than error.  Zero is
	// valid (means "no budget cap for this period").
	if cfg.Cost.DailyTokenBudget < 0 {
		errs = append(errs, fmt.Sprintf("cost.daily_token_budget must be >= 0, got %d", cfg.Cost.DailyTokenBudget))
	}
	if cfg.Cost.WeeklyTokenBudget < 0 {
		errs = append(errs, fmt.Sprintf("cost.weekly_token_budget must be >= 0, got %d", cfg.Cost.WeeklyTokenBudget))
	}

	// ── Registry validation ──────────────────────────────────────────────────
	// When models.registry is present, validate that all declared roles resolve
	// to known registry keys, and that all registry entries have required fields.
	if cfg.Models.Registry != nil {
		// Collect registry keys that are actually referenced by roles (+ their fallbacks).
		// Only these need api_key_env set; unused registry entries may intentionally
		// lack keys (e.g. template entries or future models).
		referencedKeys := make(map[string]struct{})
		addWithFallback := func(key string) {
			for key != "" {
				if _, seen := referencedKeys[key]; seen {
					break
				}
				referencedKeys[key] = struct{}{}
				if e, ok := cfg.Models.Registry[key]; ok {
					key = e.Fallback
				} else {
					break
				}
			}
		}
		for _, k := range cfg.Models.Roles.Analysts {
			addWithFallback(k)
		}
		for _, k := range []string{
			cfg.Models.Roles.Arbiter, cfg.Models.Roles.Classifier,
			cfg.Models.Roles.NewsEnricher, cfg.Models.Roles.Embedder,
		} {
			addWithFallback(k)
		}
		if cfg.Analysis.SpecialistModeEnabled {
			for _, role := range []string{"fundamental", "technical", "sentiment", "risk_manager"} {
				addWithFallback(cfg.Analysis.SpecialistRoles[role])
			}
		}

		// Validate all registry entries have provider + model.
		for key, entry := range cfg.Models.Registry {
			if entry.Provider == "" {
				errs = append(errs, fmt.Sprintf("models.registry.%s: provider is required", key))
			}
			if entry.Model == "" {
				errs = append(errs, fmt.Sprintf("models.registry.%s: model is required", key))
			}
			// Validate fallback key exists if specified.
			if entry.Fallback != "" {
				if _, ok := cfg.Models.Registry[entry.Fallback]; !ok {
					errs = append(errs, fmt.Sprintf("models.registry.%s: fallback key %q not found in registry", key, entry.Fallback))
				}
			}
			// Only require api_key_env for keys actually used by a configured role.
			if _, referenced := referencedKeys[key]; referenced {
				if entry.APIKeyEnv != "" && os.Getenv(entry.APIKeyEnv) == "" {
					errs = append(errs, fmt.Sprintf("env var %s (models.registry.%s.api_key_env) is not set", entry.APIKeyEnv, key))
				}
			}
		}
		// Validate analyst roles resolve.
		for i, key := range cfg.Models.Roles.Analysts {
			if _, ok := cfg.Models.Registry[key]; !ok {
				errs = append(errs, fmt.Sprintf("models.roles.analysts[%d]: key %q not found in models.registry", i, key))
			}
		}
		// Validate named roles resolve.
		namedRoles := map[string]string{
			"arbiter":       cfg.Models.Roles.Arbiter,
			"classifier":    cfg.Models.Roles.Classifier,
			"news_enricher": cfg.Models.Roles.NewsEnricher,
			"embedder":      cfg.Models.Roles.Embedder,
		}
		for role, key := range namedRoles {
			if key == "" {
				continue // optional roles may be unset
			}
			if _, ok := cfg.Models.Registry[key]; !ok {
				errs = append(errs, fmt.Sprintf("models.roles.%s: key %q not found in models.registry", role, key))
			}
		}
		// Registry path: analyst list comes from roles.
		analysts := cfg.Models.AnalystConfigs()
		if len(analysts) == 0 {
			errs = append(errs, "models.roles.analysts must contain at least one registry key")
		} else if len(analysts) < 3 && cfg.Models.Consensus.Enabled {
			errs = append(errs, fmt.Sprintf("models.roles.analysts has %d entries; ensemble works best with 3+", len(analysts)))
		}
	} else {
		// ── Legacy validation (no registry) ─────────────────────────────────
		if len(cfg.Models.Analysts) == 0 {
			errs = append(errs, "models.analysts must contain at least one entry (or use models.registry)")
		} else if len(cfg.Models.Analysts) < 3 && cfg.Models.Consensus.Enabled {
			errs = append(errs, fmt.Sprintf("models.analysts has %d entries; ensemble works best with 3+ analysts", len(cfg.Models.Analysts)))
		}
		for i, a := range cfg.Models.Analysts {
			if a.APIKeyEnv != "" && os.Getenv(a.APIKeyEnv) == "" {
				errs = append(errs, fmt.Sprintf("env var %s (models.analysts[%d].api_key_env) is not set", a.APIKeyEnv, i))
			}
		}
		// Legacy single-model validation.
		if len(cfg.Models.Analysts) == 0 && cfg.Models.Primary.APIKeyEnv != "" && os.Getenv(cfg.Models.Primary.APIKeyEnv) == "" {
			errs = append(errs, fmt.Sprintf("env var %s (models.primary.api_key_env) is not set", cfg.Models.Primary.APIKeyEnv))
		}
	}

	if cfg.Analysis.SpecialistModeEnabled {
		for _, role := range []string{"fundamental", "technical", "sentiment", "risk_manager"} {
			key := cfg.Analysis.SpecialistRoles[role]
			if key == "" {
				errs = append(errs, fmt.Sprintf("analysis.specialist_roles.%s is required when specialist mode is enabled", role))
				continue
			}
			if _, ok := cfg.Models.Registry[key]; !ok {
				errs = append(errs, fmt.Sprintf("analysis.specialist_roles.%s: key %q not found in models.registry", role, key))
			}
		}
	}

	if cfg.Markets.Stocks.Enabled && cfg.Markets.Stocks.APIKeyEnv != "" && os.Getenv(cfg.Markets.Stocks.APIKeyEnv) == "" {
		errs = append(errs, fmt.Sprintf("env var %s (markets.stocks.api_key_env) is not set", cfg.Markets.Stocks.APIKeyEnv))
	}
	if cfg.Markets.Stocks.Enabled && cfg.Markets.Stocks.SecretEnv != "" && os.Getenv(cfg.Markets.Stocks.SecretEnv) == "" {
		errs = append(errs, fmt.Sprintf("env var %s (markets.stocks.secret_env) is not set", cfg.Markets.Stocks.SecretEnv))
	}

	if cfg.Markets.News.ExtractionEnabled {
		if cfg.Markets.News.ExtractionRPM <= 0 {
			errs = append(errs, "markets.news.extraction_rpm must be > 0 when extraction is enabled")
		}
		if cfg.Markets.News.ExtractionMaxArticlesPerTick <= 0 {
			errs = append(errs, "markets.news.extraction_max_articles_per_tick must be > 0 when extraction is enabled")
		}
		// Always validate legacy extraction_provider/model: the engine still uses
		// cfg.Markets.News.ExtractionProvider/ExtractionModel via pickExtractionProvider()
		// regardless of whether a model registry is present. The news_enricher registry
		// role is not yet wired into the extraction path.
		if cfg.Markets.News.ExtractionProvider != "" && !hasConfiguredModel(cfg, cfg.Markets.News.ExtractionProvider, cfg.Markets.News.ExtractionModel) {
			errs = append(errs, fmt.Sprintf("markets.news.extraction_provider/model (%s/%s) not found in configured model clients", cfg.Markets.News.ExtractionProvider, cfg.Markets.News.ExtractionModel))
		}
	}

	if cfg.Notifications.Telegram.Enabled && cfg.Notifications.Telegram.BotTokenEnv != "" {
		if os.Getenv(cfg.Notifications.Telegram.BotTokenEnv) == "" {
			errs = append(errs, fmt.Sprintf("env var %s (notifications.telegram.bot_token_env) is not set", cfg.Notifications.Telegram.BotTokenEnv))
		}
	}

	dbEnv := cfg.Database.URLEnv
	if dbEnv == "" {
		dbEnv = "DATABASE_URL"
	}
	if os.Getenv(dbEnv) == "" {
		errs = append(errs, fmt.Sprintf("env var %s (database.url_env) is not set", dbEnv))
	}

	if cfg.OptionsORB.Enabled {
		if len(cfg.OptionsORB.Underlyings) == 0 {
			errs = append(errs, "options_orb.underlyings must not be empty when enabled")
		}
		if cfg.OptionsORB.RangeMinutes != 5 && cfg.OptionsORB.RangeMinutes != 15 {
			errs = append(errs, "options_orb.range_minutes must be 5 or 15")
		}
		if cfg.OptionsORB.RiskReward < 1.5 {
			errs = append(errs, "options_orb.risk_reward must be >= 1.5")
		}
	}

	if cfg.CurrencyStrength.Enabled {
		switch cfg.CurrencyStrength.Mode {
		case "shadow", "paper":
		case "live":
			errs = append(errs, "currency_strength.mode live is not supported yet; use shadow or paper until live execution controls are implemented")
		default:
			errs = append(errs, fmt.Sprintf("currency_strength.mode must be one of shadow|paper, got %q", cfg.CurrencyStrength.Mode))
		}
		if len(cfg.CurrencyStrength.RankInstruments) < 3 {
			errs = append(errs, "currency_strength.rank_instruments must contain at least 3 pairs")
		}
		if len(cfg.CurrencyStrength.TradeInstruments) == 0 {
			errs = append(errs, "currency_strength.trade_instruments must contain at least one pair")
		}
		rankSet := make(map[string]struct{}, len(cfg.CurrencyStrength.RankInstruments))
		for _, pair := range cfg.CurrencyStrength.RankInstruments {
			if !strings.Contains(pair, "/") {
				errs = append(errs, fmt.Sprintf("currency_strength.rank_instruments contains invalid pair %q (expected BASE/QUOTE)", pair))
			}
			rankSet[pair] = struct{}{}
		}
		for _, pair := range cfg.CurrencyStrength.TradeInstruments {
			if _, ok := rankSet[pair]; !ok {
				errs = append(errs, fmt.Sprintf("currency_strength.trade_instruments pair %q must also be present in rank_instruments", pair))
			}
		}
		if cfg.CurrencyStrength.Timezone == "" {
			errs = append(errs, "currency_strength.timezone is required")
		} else if _, err := time.LoadLocation(cfg.CurrencyStrength.Timezone); err != nil {
			errs = append(errs, fmt.Sprintf("currency_strength.timezone %q is not a valid IANA timezone: %v", cfg.CurrencyStrength.Timezone, err))
		}
		startMin, startOK := parseHHMMConfig("currency_strength.strength_start", cfg.CurrencyStrength.StrengthStart, &errs)
		endMin, endOK := parseHHMMConfig("currency_strength.strength_end", cfg.CurrencyStrength.StrengthEnd, &errs)
		exitMin, exitOK := parseHHMMConfig("currency_strength.exit_time", cfg.CurrencyStrength.ExitTime, &errs)
		if startOK && endOK && exitOK && !(startMin < endMin && endMin < exitMin) {
			errs = append(errs, "currency_strength times must satisfy strength_start < strength_end < exit_time")
		}
		if cfg.CurrencyStrength.MinGap <= 0 {
			errs = append(errs, "currency_strength.min_gap must be > 0")
		}
		if cfg.CurrencyStrength.BroadMin <= 0 {
			errs = append(errs, "currency_strength.broad_min must be > 0")
		}
		if cfg.CurrencyStrength.ConfirmMinMove < 0 {
			errs = append(errs, "currency_strength.confirm_min_move must be >= 0")
		}
		if cfg.CurrencyStrength.Units <= 0 {
			errs = append(errs, "currency_strength.units must be > 0")
		}
	}

	// Validate forex ORB per-instrument breakout_confirmation so a typo (e.g. "close_3")
	// fails loudly instead of silently disabling the confirmation gate and re-enabling the
	// wick-touch entries Phase 3 is meant to filter.
	if cfg.ForexORB.Enabled {
		// Reject an unknown breakout_confirmation so a typo (e.g. "close_3") fails loud instead
		// of silently disabling the confirmation gate. All five modes are now live-wired
		// (retest_swing/retest_range entered the live path alongside stop_touch/close_*).
		for _, inst := range cfg.ForexORB.Instruments {
			errs = append(errs, validateInstrumentStrategyParams("forex_orb", inst, cfg.ForexORB.RiskReward)...)
			if !validBreakoutConfirmation[inst.BreakoutConfirmation] {
				errs = append(errs, fmt.Sprintf(
					"forex_orb instrument %q: breakout_confirmation %q must be one of stop_touch|close_1m|close_3m|retest_swing|retest_range",
					inst.Symbol, inst.BreakoutConfirmation))
			}
			// Close-position guard: the threshold is compared against CloseFrac, which is
			// normalised to [0,1]. A value >= 1 makes the buy condition (CloseFrac >= t)
			// unsatisfiable AND the sell condition (CloseFrac <= 1-t) unsatisfiable, silently
			// skipping EVERY session for the instrument — the classic "60 typed for 0.60" foot-gun.
			// A negative value is meaningless. Fail loud rather than halt trading in silence.
			if inst.CloseFracThreshold < 0 || inst.CloseFracThreshold >= 1 {
				errs = append(errs, fmt.Sprintf(
					"forex_orb instrument %q: close_frac_threshold %.3f must be in [0, 1) (0 = off; e.g. 0.60, not 60)",
					inst.Symbol, inst.CloseFracThreshold))
			}
			// Margin-aware sizing: a rate above 1 would SHRINK the notional cap below the
			// unleveraged baseline — almost certainly a leverage ratio typed where a fraction
			// belongs (33 instead of 0.033). Fail loud.
			if inst.MarginRate > 1 {
				errs = append(errs, fmt.Sprintf(
					"forex_orb instrument %q: margin_rate %.3f must be a fraction of notional in (0, 1] (e.g. 0.033 for 30:1 leverage), not a leverage ratio",
					inst.Symbol, inst.MarginRate))
			}
			// max_notional_usd caps entry×qty, which is denominated in the pair's QUOTE currency.
			// That equals USD only for USD-quoted pairs (…/USD). On a non-USD-quoted pair (e.g.
			// USD/JPY, EUR/GBP) entry×qty is JPY/GBP, so a USD cap would drastically under-size or
			// skip the instrument. Restrict the knob to USD-quoted symbols rather than pretend to
			// convert (sizeForexQty has no FX rate). The sweep sets instruments programmatically
			// and never goes through Validate, so it must uphold the same rule.
			if inst.MaxNotionalUSD > 0 && !strings.HasSuffix(inst.Symbol, "/USD") {
				errs = append(errs, fmt.Sprintf(
					"forex_orb instrument %q: max_notional_usd is only valid for USD-quoted pairs (…/USD); entry×qty on this pair is quote-currency value, not USD",
					inst.Symbol))
			}
			// The commission gate needs a commission estimate; a max without one would be a
			// silent no-op that quietly re-admits the high-cost trades it exists to block.
			if inst.CommissionBpsPerSide < 0 || inst.CommissionMinPerSide < 0 {
				errs = append(errs, fmt.Sprintf(
					"forex_orb instrument %q: commission_bps_per_side and commission_min_per_side must not be negative",
					inst.Symbol))
			}
			if inst.MaxCommissionToRisk > 0 && inst.CommissionPerSide <= 0 && inst.CommissionBpsPerSide <= 0 {
				errs = append(errs, fmt.Sprintf(
					"forex_orb instrument %q: max_commission_to_risk requires commission_per_side > 0 or commission_bps_per_side > 0",
					inst.Symbol))
			}
			// Split-bracket partial take-profit: a partial needs a fraction in (0,1), and its
			// target must sit nearer than the runner's (else the "partial" closes after the
			// runner — a nonsensical split that ScaleOutBrackets would still place).
			if inst.PartialTakeProfitR > 0 {
				if inst.PartialFraction <= 0 || inst.PartialFraction >= 1 {
					errs = append(errs, fmt.Sprintf(
						"forex_orb instrument %q: partial_fraction %.3f must be in (0,1) when partial_take_profit_r is set",
						inst.Symbol, inst.PartialFraction))
				}
				runnerR := inst.RunnerTakeProfitR
				if runnerR <= 0 {
					runnerR = cfg.ForexORB.RiskReward // unset runner keeps the base RR target
				}
				if runnerR > 0 && inst.PartialTakeProfitR >= runnerR {
					errs = append(errs, fmt.Sprintf(
						"forex_orb instrument %q: partial_take_profit_r %.2f must be less than the runner target %.2f",
						inst.Symbol, inst.PartialTakeProfitR, runnerR))
				}
			}
		}
	}

	// Futures ORB per-instrument breakout_confirmation — same modes as forex, all live-wired.
	for _, cfgFut := range []FuturesORBConfig{cfg.FuturesORB, cfg.FuturesORBMetals} {
		if !cfgFut.Enabled {
			continue
		}
		for _, inst := range cfgFut.Instruments {
			errs = append(errs, validateInstrumentStrategyParams("futures_orb", inst.ToInstrument(), cfgFut.RiskReward)...)
			if !validBreakoutConfirmation[inst.BreakoutConfirmation] {
				errs = append(errs, fmt.Sprintf(
					"futures_orb instrument %q: breakout_confirmation %q must be one of stop_touch|close_1m|close_3m|retest_swing|retest_range",
					inst.Symbol, inst.BreakoutConfirmation))
			}
		}
	}

	// Feedback ORB auto-throttle: the learn-from window must be one the reviewer actually
	// computes (else no risk_multiplier insight is ever stored), and the bounds must be sane.
	if at := cfg.Feedback.ORBAutoThrottle; at.Enabled {
		win := at.ThrottleWindow()
		found := false
		for _, w := range cfg.Feedback.Windows {
			if fmt.Sprintf("%dd", w) == win {
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, fmt.Sprintf(
				"feedback.orb_auto_throttle.window %q must be one of the configured feedback.windows", win))
		}
		if at.Floor < 0 || at.Cap < 0 || (at.Cap > 0 && at.Floor > at.Cap) {
			errs = append(errs, fmt.Sprintf(
				"feedback.orb_auto_throttle: floor %.2f / cap %.2f invalid (need 0 <= floor <= cap)", at.Floor, at.Cap))
		}
	}

	return errs
}

const weeklyLeaderMaxHistoryCalendarDays = 36525

func durationConfigFits(value int, unit time.Duration) bool {
	if value < 0 || unit <= 0 {
		return false
	}
	const maxDuration = time.Duration(1<<63 - 1)
	return uint64(value) <= uint64(maxDuration)/uint64(unit)
}

// weeklyLeaderHistoryCalendarDays converts a trading-session requirement to a
// conservative calendar fetch window using 252 sessions/year plus a two-week
// provider/holiday buffer.
func weeklyLeaderHistoryCalendarDays(sessions int) (int, bool) {
	if sessions <= 0 {
		return 0, true
	}
	maxInt := int(^uint(0) >> 1)
	maxInt64 := int64(^uint64(0) >> 1)
	if int64(sessions) > (maxInt64-251)/365 {
		return 0, false
	}
	required := (int64(sessions)*365+251)/252 + 14
	if required > int64(maxInt) {
		return 0, false
	}
	return int(required), true
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func parseHHMMConfig(field, raw string, errs *[]string) (int, bool) {
	parts := strings.Split(raw, ":")
	if len(parts) != 2 {
		*errs = append(*errs, fmt.Sprintf("%s must be HH:MM, got %q", field, raw))
		return 0, false
	}
	hour, errHour := strconv.Atoi(parts[0])
	minute, errMinute := strconv.Atoi(parts[1])
	if errHour != nil || errMinute != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		*errs = append(*errs, fmt.Sprintf("%s must be HH:MM, got %q", field, raw))
		return 0, false
	}
	return hour*60 + minute, true
}

func resolveEnvVars(cfg *Config) {
	// Registry entries.
	for key, entry := range cfg.Models.Registry {
		entry.ModelConfig.APIKey = resolveEnv(entry.ModelConfig.APIKeyEnv)
		cfg.Models.Registry[key] = entry
	}
	// Legacy primary field.
	cfg.Models.Primary.APIKey = resolveEnv(cfg.Models.Primary.APIKeyEnv)
	// Phase 12: analysts + arbiter.
	for i := range cfg.Models.Analysts {
		cfg.Models.Analysts[i].APIKey = resolveEnv(cfg.Models.Analysts[i].APIKeyEnv)
	}
	cfg.Models.Arbiter.APIKey = resolveEnv(cfg.Models.Arbiter.APIKeyEnv)
	cfg.Markets.Stocks.APIKey = resolveEnv(cfg.Markets.Stocks.APIKeyEnv)
	cfg.Markets.Stocks.Secret = resolveEnv(cfg.Markets.Stocks.SecretEnv)
	cfg.Markets.News.APIKey = resolveEnv(cfg.Markets.News.APIKeyEnv)
	cfg.Markets.OANDA.Token = resolveEnv(cfg.Markets.OANDA.TokenEnv)
	cfg.Notifications.Telegram.BotToken = resolveEnv(cfg.Notifications.Telegram.BotTokenEnv)
	cfg.Embeddings.APIKey = resolveEnv(cfg.Embeddings.APIKeyEnv)
}

func resolveEnv(key string) string {
	if key == "" {
		return ""
	}
	return os.Getenv(key)
}

func hasConfiguredModel(cfg *Config, provider, model string) bool {
	match := func(m ModelConfig) bool {
		if m.Provider == "" {
			return false
		}
		if m.Provider != provider {
			return false
		}
		if model != "" && m.Model != model {
			return false
		}
		return true
	}

	// Check legacy fields.
	for _, m := range cfg.Models.Analysts {
		if match(m) {
			return true
		}
	}
	if match(cfg.Models.Primary) || match(cfg.Models.Arbiter) {
		return true
	}
	for _, m := range cfg.Models.Secondary {
		if match(m) {
			return true
		}
	}

	// Also check registry-derived configs so validation works when the registry
	// is the sole source of model configuration.
	for _, m := range cfg.Models.AnalystConfigs() {
		if match(m) {
			return true
		}
	}
	if arbiter := cfg.Models.ArbiterConfig(); match(arbiter) {
		return true
	}

	// Check the news_enricher registry role. resolveEnricherProvider prefers this
	// over the analyst scan, so it is a valid runtime source for extraction even
	// when the model is not in the analyst list.
	if mc, err := cfg.Models.ResolveRole("news_enricher"); err == nil {
		if match(mc) {
			return true
		}
	}
	return false
}

func save(cfg *Config, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func scalarNode(value string) *yaml.Node {
	if b, err := strconv.ParseBool(value); err == nil {
		if b {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"}
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"}
	}
	if i, err := strconv.ParseInt(value, 10, 64); err == nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(i, 10)}
	}
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(f, 'f', -1, 64)}
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func yamlNodeFromAny(value any) *yaml.Node {
	switch v := value.(type) {
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	case bool:
		if v {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"}
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"}
	case int:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(v)}
	case int8, int16, int32, int64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", v)}
	case uint, uint8, uint16, uint32, uint64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", v)}
	case float32:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(float64(v), 'f', -1, 64)}
	case float64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(v, 'f', -1, 64)}
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(i, 10)}
		}
		if f, err := v.Float64(); err == nil {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(f, 'f', -1, 64)}
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v.String()}
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
	case []string:
		n := &yaml.Node{Kind: yaml.SequenceNode}
		for _, item := range v {
			n.Content = append(n.Content, yamlNodeFromAny(item))
		}
		return n
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode}
		for _, item := range v {
			n.Content = append(n.Content, yamlNodeFromAny(item))
		}
		return n
	default:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprintf("%v", v)}
	}
}

// setYAMLNode sets a dotted-path key in a YAML node tree.
// Supports both MappingNode (by key) and SequenceNode (by index).
func setYAMLNode(node *yaml.Node, parts []string, value any) {
	if len(parts) == 0 {
		return
	}
	key := parts[0]

	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content)-1; i += 2 {
			if node.Content[i].Value == key {
				if len(parts) == 1 {
					node.Content[i+1] = yamlNodeFromAny(value)
				} else {
					setYAMLNode(node.Content[i+1], parts[1:], value)
				}
				return
			}
		}
		// Key not found — append.
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
		if len(parts) == 1 {
			node.Content = append(node.Content, keyNode, yamlNodeFromAny(value))
		} else {
			child := &yaml.Node{Kind: yaml.MappingNode}
			node.Content = append(node.Content, keyNode, child)
			setYAMLNode(child, parts[1:], value)
		}
		return
	}

	if node.Kind == yaml.SequenceNode {
		idx, err := strconv.Atoi(key)
		if err != nil {
			return
		}
		if idx < 0 {
			return
		}
		// Ensure sequence is long enough
		for len(node.Content) <= idx {
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ""})
		}
		if len(parts) == 1 {
			node.Content[idx] = yamlNodeFromAny(value)
		} else {
			// Promote missing scalar node to MappingNode if we need to recurse
			if node.Content[idx].Kind == yaml.ScalarNode && node.Content[idx].Value == "" {
				node.Content[idx] = &yaml.Node{Kind: yaml.MappingNode}
			}
			setYAMLNode(node.Content[idx], parts[1:], value)
		}
		return
	}
}
