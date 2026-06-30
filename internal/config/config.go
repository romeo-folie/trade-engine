// Package config handles loading, merging, and validating TradeEngine configuration.
// Merge order: defaults → YAML file → DB overrides → CLI flags.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ── Top-level config ──────────────────────────────────────────────────────────

type Config struct {
	Engine           EngineConfig        `yaml:"engine"`
	Models           ModelsConfig        `yaml:"models"`
	Markets          MarketsConfig       `yaml:"markets"`
	Strategies       StrategiesConfig    `yaml:"strategies"`
	Risk             RiskConfig          `yaml:"risk"`
	Notifications    NotificationsConfig `yaml:"notifications"`
	Logging          LoggingConfig       `yaml:"logging"`
	Database         DatabaseConfig      `yaml:"database"`
	Reports          ReportsConfig       `yaml:"reports"`
	Backtest         BacktestConfig      `yaml:"backtest"`
	Embeddings       EmbeddingsConfig    `yaml:"embeddings"`
	Analysis         AnalysisConfig      `yaml:"analysis"`
	Cost             CostConfig          `yaml:"cost"`
	ORB              ORBConfig           `yaml:"orb"`
	ForexORB         ForexORBConfig      `yaml:"forex_orb"`
	FuturesORB       FuturesORBConfig    `yaml:"futures_orb"`
	FuturesORBMetals FuturesORBConfig    `yaml:"futures_orb_metals"`
	Scanner          ScannerConfig       `yaml:"scanner"`
	Feedback         FeedbackConfig      `yaml:"feedback"`
	Portfolio        PortfolioConfig     `yaml:"portfolio"`
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

// ORBConfig controls the Opening Range Breakout operating mode (Phase 15).
type ORBConfig struct {
	Enabled            bool    `yaml:"enabled"`
	Mode               string  `yaml:"mode"`                 // "paper" | "live"
	GapThresholdPct    float64 `yaml:"gap_threshold_pct"`    // minimum gap % (e.g. 3.0)
	MaxCandidates      int     `yaml:"max_candidates"`       // 1–5
	RangeMinutes       int     `yaml:"range_minutes"`        // 5 or 15
	RiskReward         float64 `yaml:"risk_reward"`          // e.g. 2.0
	MaxRiskPct         float64 `yaml:"max_risk_pct"`         // max loss per trade as % of portfolio
	MaxPositionPct     float64 `yaml:"max_position_pct"`     // max notional per trade as % of portfolio (e.g. 10.0)
	NewsLookbackHours  int     `yaml:"news_lookback_hours"`  // e.g. 24
	ScanCadenceMinutes int     `yaml:"scan_cadence_minutes"` // minimum minutes between pre-market scans (default 2)
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
	MaxWatchlist    int     `yaml:"max_watchlist"`
	DryRun          bool    `yaml:"dry_run"`
	InitialCapital  float64 `yaml:"initial_capital"`  // starting cash for paper trading (default 100000)
	LogTimezone     string  `yaml:"log_timezone"`     // IANA timezone for display (default "UTC")
	SkipRecentTick  *bool   `yaml:"skip_recent_tick"` // skip startup tick if last analysis is within tick interval (default true)
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

	// MultiTimeframeBorderlineEnabled enables a second analyst pass on the
	// alternate timeframe when the arbiter resolves to WAIT inside a narrow
	// borderline confidence band. The combined arbiter then picks the final
	// action (Issue #321). Default: true.
	MultiTimeframeBorderlineEnabled bool `yaml:"multi_timeframe_borderline_enabled"`

	// MultiTimeframeAltMap maps a primary timeframe to its alternate. Keys and
	// values are timeframe strings (e.g. "1h", "4h", "1d"). When a primary
	// timeframe has no entry the second pass is skipped for that ticker.
	// Defaults: 1h → 4h, 4h → 1h, 1d → 1h.
	MultiTimeframeAltMap map[string]string `yaml:"multi_timeframe_alt_map"`

	// MultiTimeframeBorderlineConfMin is the inclusive lower bound of the
	// borderline confidence band. Defaults to 0.55.
	MultiTimeframeBorderlineConfMin float64 `yaml:"multi_timeframe_borderline_conf_min"`

	// MultiTimeframeBorderlineConfMax is the inclusive upper bound of the
	// borderline confidence band. Defaults to 0.70.
	MultiTimeframeBorderlineConfMax float64 `yaml:"multi_timeframe_borderline_conf_max"`

	// SpecialistModeEnabled toggles the 4-role specialist arbiter path
	// (Issue #323, inspired by Grok 4.20's Alpha Arena 4-agent debate).
	// When true, the engine routes signal-generation calls to
	// AnalyzeSpecialist instead of AnalyzeFull. AnalyzeFull itself is
	// unchanged. Default: false (safe-rollout).
	SpecialistModeEnabled bool `yaml:"specialist_mode_enabled"`

	// SpecialistRoles maps each specialist role name to a registry key in
	// models.registry. Recognised roles: fundamental, technical, sentiment,
	// risk_manager. Roles whose mapped provider is missing or unavailable
	// are skipped at runtime with a WARN log (no crash).
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

	// MaxHoldMinutes is the per-instrument hard time limit on a *filled* ORB trade.
	// When a filled position is older than this, the scheduler cancels its remaining
	// bracket legs, flattens at market, and records exit_type='time_exit'. This caps
	// the multi-day-hold tail that dominated realized losses. 0 inherits the
	// strategy-level ForexORBConfig.MaxHoldMinutes; when both are 0 time exits are
	// disabled for this instrument and trades run until their bracket resolves.
	MaxHoldMinutes int `yaml:"max_hold_minutes"`
}

// ForexORBConfig configures the Opening Range Breakout strategy for forex/CFD instruments via IBKR.
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
}

// FuturesORBConfig configures the Opening Range Breakout strategy for exchange-listed futures via IBKR.
// Re-used for different asset class groups (equity index, metals, energy) via separate top-level keys.
type FuturesORBConfig struct {
	Enabled        bool                `yaml:"enabled"`
	SessionOpenET  string              `yaml:"session_open_et"` // "HH:MM" in US/Eastern, e.g. "09:30"
	RangeMinutes   int                 `yaml:"range_minutes"`
	RiskReward     float64             `yaml:"risk_reward"`
	MaxRiskPct     float64             `yaml:"max_risk_pct"`
	MaxPositionPct float64             `yaml:"max_position_pct"`
	Instruments    []FuturesInstrument `yaml:"instruments"`
}

// ScannerConfig controls the autonomous market scanner that discovers swing-trade
// candidates and adds them to the watchlist with a configurable TTL.
type ScannerConfig struct {
	// Enabled turns the scanner on or off. Default: false.
	Enabled bool `yaml:"enabled"`

	// ScanIntervalHours is how often the scanner runs during market hours.
	// Default: 2 (runs at roughly 10am, 12pm, 2pm ET on a standard trading day).
	ScanIntervalHours int `yaml:"scan_interval_hours"`

	// CandidateCap is the maximum number of scanner-sourced tickers that may be
	// active on the watchlist at any one time. Lowest-conviction items are evicted
	// when the cap is reached. Default: 10.
	CandidateCap int `yaml:"candidate_cap"`

	// TTLDays is how many calendar days a scanner-added ticker stays on the watchlist
	// before being auto-expired (deactivated) if no signal fires. Default: 3.
	TTLDays int `yaml:"ttl_days"`

	// MinConviction is the minimum LLM conviction score (0–1) required for a
	// candidate to be added to the watchlist. Default: 0.65.
	MinConviction float64 `yaml:"min_conviction"`

	// NewsLookbackHours controls how far back to fetch news when enriching
	// screener candidates before LLM scoring. Default: 24.
	NewsLookbackHours int `yaml:"news_lookback_hours"`

	// TopMovers is the number of top movers (gainers + losers) to fetch per scan.
	// Default: 20.
	TopMovers int `yaml:"top_movers"`

	// TopActives is the number of most-active-by-volume stocks to fetch per scan.
	// Default: 20.
	TopActives int `yaml:"top_actives"`

	// GraphNeighbors configures sourcing scanner candidates from the knowledge
	// graph — tradeable companies one hop from a manually-watched ticker.
	GraphNeighbors GraphNeighborsConfig `yaml:"graph_neighbors"`
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
	Enabled             bool               `yaml:"enabled"`
	ReviewTimeUTC       string             `yaml:"review_time_utc"`
	Windows             []int              `yaml:"windows"`
	PromptEnrichment    bool               `yaml:"prompt_enrichment"`
	ORBPromptEnrichment bool               `yaml:"orb_prompt_enrichment"`
	Thresholds          FeedbackThresholds `yaml:"thresholds"`
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
	Enabled   bool   `yaml:"enabled"`
	Paper     bool   `yaml:"paper"`
	APIKeyEnv string `yaml:"api_key_env"`
	APIKey    string `yaml:"-" json:"-"`
	SecretEnv string `yaml:"secret_env"`
	Secret    string `yaml:"-" json:"-"`
	BaseURL   string `yaml:"base_url"`
	DataURL   string `yaml:"data_url"`
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
	// When Enabled, a goroutine periodically recomputes per-trade trailing
	// stops via `new_stop = max(current_stop, hwm - atr_multiple * ATR)` and
	// submits/replaces a broker STOP order. Disabled by default.
	TrailingStop TrailingStopConfig `yaml:"trailing_stop"`
}

// TrailingStopConfig governs the server-side trailing stop manager (#322).
// ATRMultiple is a unitless coefficient applied to absolute-price ATR; the
// trailing stop is `high_water_mark - ATRMultiple * ATR`. With a default of
// 1.5 a $50 stock with ATR=$1 trails at $48.50 from each new high-water mark.
// IntervalSec defaults to 60s when zero. ATRPeriod defaults to 14 (Wilder).
type TrailingStopConfig struct {
	Enabled     bool    `yaml:"enabled"`
	ATRMultiple float64 `yaml:"atr_multiple"`
	IntervalSec int     `yaml:"interval_sec"`
	ATRPeriod   int     `yaml:"atr_period"`
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
//  5. migrateModelsConfig  — legacy field migration
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

		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
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

// Get returns the current config. Panics if Load was not called.
func Get() *Config {
	mu.RLock()
	defer mu.RUnlock()
	if current == nil {
		panic("config.Load() must be called before config.Get()")
	}
	return current
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
func Validate(cfg *Config) []string {
	var errs []string

	validModes := map[string]bool{"manual": true, "auto": true, "paper": true}
	if !validModes[cfg.Engine.Mode] {
		errs = append(errs, fmt.Sprintf("engine.mode must be one of manual|auto|paper, got %q", cfg.Engine.Mode))
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

	if cfg.Markets.Stocks.APIKeyEnv != "" && os.Getenv(cfg.Markets.Stocks.APIKeyEnv) == "" {
		errs = append(errs, fmt.Sprintf("env var %s (markets.stocks.api_key_env) is not set", cfg.Markets.Stocks.APIKeyEnv))
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

	// ── ORB validation ──────────────────────────────────────────────────────
	if cfg.ORB.Enabled {
		if cfg.ORB.GapThresholdPct <= 0 {
			errs = append(errs, "orb.gap_threshold_pct must be > 0")
		}
		if cfg.ORB.RangeMinutes != 5 && cfg.ORB.RangeMinutes != 15 {
			errs = append(errs, "orb.range_minutes must be 5 or 15")
		}
		if cfg.ORB.MaxCandidates < 1 || cfg.ORB.MaxCandidates > 5 {
			errs = append(errs, "orb.max_candidates must be 1-5")
		}
		if cfg.ORB.RiskReward < 1.5 {
			errs = append(errs, "orb.risk_reward must be >= 1.5")
		}
		if cfg.ORB.MaxPositionPct <= 0 || cfg.ORB.MaxPositionPct > 50.0 {
			errs = append(errs, fmt.Sprintf("orb.max_position_pct must be > 0 and <= 50 (got %.1f)", cfg.ORB.MaxPositionPct))
		}
		if cfg.ORB.MaxRiskPct <= 0 || cfg.ORB.MaxRiskPct > 5.0 {
			errs = append(errs, "orb.max_risk_pct must be > 0 and <= 5.0")
		}
		if cfg.ORB.Mode != "paper" && cfg.ORB.Mode != "live" {
			errs = append(errs, "orb.mode must be paper or live")
		}
	}

	return errs
}

// ── Helpers ───────────────────────────────────────────────────────────────────

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
