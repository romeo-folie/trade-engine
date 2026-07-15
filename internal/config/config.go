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
	OptionsORB       OptionsORBConfig    `yaml:"options_orb"`
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
	Enabled                 bool    `yaml:"enabled"`
	Mode                    string  `yaml:"mode"`                      // "paper" | "live"
	GapThresholdPct         float64 `yaml:"gap_threshold_pct"`         // minimum gap % (e.g. 3.0)
	MinPrice                float64 `yaml:"min_price"`                 // drop gap movers trading below this $ price before scoring (omit for default 5.0; explicit 0 disables). Kills sub-$5 penny stocks / warrants that dominate a %-ranked movers list.
	MinDollarVolume         float64 `yaml:"min_dollar_volume"`         // drop gap movers whose price×volume is below this before scoring (default 0 = off). Snapshot volume is iex-only and under-reports consolidated liquidity, so a hard floor can drop genuinely liquid names — only enable on a SIP-backed deployment.
	MaxCandidates           int     `yaml:"max_candidates"`            // 1–5 (how many scored candidates to actually trade)
	MaxScanCandidates       int     `yaml:"max_scan_candidates"`       // how many top gap movers to LLM-score per scan (caps rate-limited LLM calls; 0 = default 15). Gap movers are ranked liquidity-first (|gap%| × log10 dollar-volume), so this keeps the most tradeable ones.
	ScanConfidenceThreshold float64 `yaml:"scan_confidence_threshold"` // min aggregated analyst conviction for a gap candidate to be tradeable (0 = default 0.68). The equities scan aggregates whatever analysts are reachable by confidence — no arbiter required — so scoring survives a provider/arbiter outage.
	RangeMinutes            int     `yaml:"range_minutes"`             // 5 or 15
	RiskReward              float64 `yaml:"risk_reward"`               // e.g. 2.0
	MaxRiskPct              float64 `yaml:"max_risk_pct"`              // max loss per trade as % of portfolio
	MaxPositionPct          float64 `yaml:"max_position_pct"`          // max notional per trade as % of portfolio (e.g. 10.0)
	NewsLookbackHours       int     `yaml:"news_lookback_hours"`       // e.g. 24
	ScanCadenceMinutes      int     `yaml:"scan_cadence_minutes"`      // minimum minutes between pre-market scans (default 2)
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

	Dukascopy DukascopyConfig `yaml:"dukascopy"`
	HistData  HistDataConfig  `yaml:"histdata"`
	// BacktestProvider selects the ORB backtest data source: "histdata" (default; free bulk
	// tick ZIPs, no account/key/throttle), "dukascopy", or "oanda". Overridable per-run with
	// `te backtest orb --provider`.
	BacktestProvider string `yaml:"backtest_provider"`
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

	// MaxCommissionToRisk rejects a session when the estimated round-trip commission is too
	// large relative to the planned DOLLAR risk: 2×commission_per_side / (|entry−stop|×qty).
	// The spread gate (max_spread_to_risk) can't catch this: spread scales with position size
	// while commission has a fixed floor, so it only bites at small dollar risk. 0 disables.
	// Requires commission_per_side > 0 (validated).
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
		Symbol:               fi.Symbol,
		SecType:              "FUT",
		MinLotSize:           1,
		PipBuffer:            pipBuffer,
		PointValue:           pointValue,
		TickSize:             fi.TickSize,
		MarginRate:           fi.MarginRate,
		MaxHoldMinutes:       fi.MaxHoldMinutes,
		MinRangeATR:          fi.MinRangeATR,
		MaxRangeATR:          fi.MaxRangeATR,
		MaxSpreadToRisk:      fi.MaxSpreadToRisk,
		BreakoutConfirmation: fi.BreakoutConfirmation,
		RetestTolerance:      fi.RetestTolerance,
		RetestTimeoutMinutes: fi.RetestTimeoutMinutes,
		DirectionEligibility: fi.DirectionEligibility,
		CommissionPerSide:    fi.CommissionPerSide,
		MaxCommissionToRisk:  fi.MaxCommissionToRisk,
		RiskMultiplier:       fi.RiskMultiplier,
		MaxNotionalUSD:       fi.MaxNotionalUSD,
		BreakEvenR:           fi.BreakEvenR,
		TrailATRMult:         fi.TrailATRMult,
		TrailAfterR:          fi.TrailAfterR,
		PartialTakeProfitR:   fi.PartialTakeProfitR,
		PartialFraction:      fi.PartialFraction,
		RunnerTakeProfitR:    fi.RunnerTakeProfitR,
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

// OptionsORBConfig configures options-ORB: running the existing ORB signal on a
// curated set of underlyings but executing via long single-leg equity options on
// Alpaca. Default-off. The gate thresholds mirror internal/options.GateConfig; the
// sizing/exit knobs feed the router and app-managed exit manager.
type OptionsORBConfig struct {
	Enabled bool `yaml:"enabled"`
	// Underlyings is the set of options-mode symbols. A ticker outside this set runs
	// the normal equity/forex/futures bracket path unchanged.
	Underlyings []string `yaml:"underlyings"`
	// ETFUnderlyings are earnings-free (SPY/QQQ) and exempt from the earnings gate.
	// A single-name underlying not listed here is subject to the earnings blackout,
	// and — with no earnings provider wired yet — fails closed (never trades) until
	// one is configured. v1 enables ETFs first for exactly this reason.
	ETFUnderlyings []string `yaml:"etf_underlyings"`

	// Sizing (dollars).
	PremiumBudget   float64 `yaml:"premium_budget"`     // premium to deploy per trade
	MaxLossPerTrade float64 `yaml:"max_loss_per_trade"` // hard cap on premium at risk (0 = none)

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

	// NewsSympathy configures news-triggered sympathy sourcing: fresh,
	// sentiment-strong news on a manually-watched ticker fans out to its graph
	// neighbours as candidates, carrying the triggering headline as the thesis.
	NewsSympathy NewsSympathyConfig `yaml:"news_sympathy"`
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
	Enabled             bool                  `yaml:"enabled"`
	ReviewTimeUTC       string                `yaml:"review_time_utc"`
	Windows             []int                 `yaml:"windows"`
	PromptEnrichment    bool                  `yaml:"prompt_enrichment"`
	ORBPromptEnrichment bool                  `yaml:"orb_prompt_enrichment"`
	Thresholds          FeedbackThresholds    `yaml:"thresholds"`
	ORBAutoThrottle     ORBAutoThrottleConfig `yaml:"orb_auto_throttle"`
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
	Enabled   bool   `yaml:"enabled"`
	Paper     bool   `yaml:"paper"`
	APIKeyEnv string `yaml:"api_key_env"`
	APIKey    string `yaml:"-" json:"-"`
	SecretEnv string `yaml:"secret_env"`
	Secret    string `yaml:"-" json:"-"`
	BaseURL   string `yaml:"base_url"`
	DataURL   string `yaml:"data_url"`
	// OptionFeed selects the Alpaca options market-data feed: "opra" (real-time,
	// requires the Algo Trader Plus subscription) or "indicative" (free, 15-min
	// delayed). Empty defaults to "indicative". Options-ORB (issue #414) requires
	// "opra" to trade the open; the adapter logs when it falls back to indicative.
	OptionFeed string `yaml:"option_feed"`
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

	// BracketExitsEnabled submits stock-path BUY entries as broker bracket
	// orders (GTC, whole shares): the take-profit and stop-loss live at the
	// broker and fire intrabar, instead of the engine's hourly candle-close
	// checks (measured ~1.5%/trade of exit slippage, PR #433 study).
	// Requires stop_loss AND take_profit on the signal; entries sized under
	// one whole share fall back to app-managed exits. Default off.
	BracketExitsEnabled bool `yaml:"bracket_exits_enabled"`

	// EntryGates are evidence-driven entry-quality filters from the
	// May–Jul 2026 execution study: news_sentiment entries the tape never
	// confirmed ran a 16% win rate, momentum entries chasing >2% run-ups
	// ran 14% vs 55% when not chasing, and the session's first analysis
	// tick (the overnight-headline burst) was the worst recurring entry
	// cohort. All gates default OFF; each is independent.
	EntryGates EntryGatesConfig `yaml:"entry_gates"`
}

// EntryGatesConfig holds per-strategy entry-quality gates. Zero values
// disable every gate so existing configs upgrade without behavior change.
// Gates apply to BUY signals only and fail OPEN when the prior daily close
// is unavailable — they are alpha filters, not safety guards.
type EntryGatesConfig struct {
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
		// Analyst confidences are on a 0-1 scale, so a threshold > 1 (e.g. the typo 68
		// for 0.68) can never pass and would silently skip EVERY candidate. 0 means "use
		// the default"; anything else must be in (0, 1].
		if cfg.ORB.ScanConfidenceThreshold < 0 || cfg.ORB.ScanConfidenceThreshold > 1 {
			errs = append(errs, fmt.Sprintf("orb.scan_confidence_threshold must be in [0,1] (0 = default 0.68); got %.3f", cfg.ORB.ScanConfidenceThreshold))
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

	// Validate forex ORB per-instrument breakout_confirmation so a typo (e.g. "close_3")
	// fails loudly instead of silently disabling the confirmation gate and re-enabling the
	// wick-touch entries Phase 3 is meant to filter.
	if cfg.ForexORB.Enabled {
		// Reject an unknown breakout_confirmation so a typo (e.g. "close_3") fails loud instead
		// of silently disabling the confirmation gate. All five modes are now live-wired
		// (retest_swing/retest_range entered the live path alongside stop_touch/close_*).
		for _, inst := range cfg.ForexORB.Instruments {
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
			// silent no-op that quietly re-admits the min-commission trades it exists to block.
			if inst.MaxCommissionToRisk > 0 && inst.CommissionPerSide <= 0 {
				errs = append(errs, fmt.Sprintf(
					"forex_orb instrument %q: max_commission_to_risk requires commission_per_side > 0",
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
