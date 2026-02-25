// Package config handles loading, merging, and validating TradeEngine configuration.
// Merge order: defaults → YAML file → DB overrides → CLI flags.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// ── Top-level config ──────────────────────────────────────────────────────────

type Config struct {
	Engine        EngineConfig        `yaml:"engine"`
	Models        ModelsConfig        `yaml:"models"`
	Markets       MarketsConfig       `yaml:"markets"`
	Strategies    StrategiesConfig    `yaml:"strategies"`
	Risk          RiskConfig          `yaml:"risk"`
	Notifications NotificationsConfig `yaml:"notifications"`
	Logging       LoggingConfig       `yaml:"logging"`
	Database      DatabaseConfig      `yaml:"database"`
	Reports       ReportsConfig       `yaml:"reports"`
}

// ── Engine ────────────────────────────────────────────────────────────────────

type EngineConfig struct {
	Mode            string `yaml:"mode"`              // manual | auto | paper
	TickIntervalSec int    `yaml:"tick_interval_sec"` // default 300 (5 min)
	MaxWatchlist    int    `yaml:"max_watchlist"`
	DryRun          bool   `yaml:"dry_run"`
}

// ── Models ────────────────────────────────────────────────────────────────────

type ModelsConfig struct {
	Primary   ModelConfig   `yaml:"primary"`
	Secondary []ModelConfig `yaml:"secondary"`
	Consensus ConsensusConfig `yaml:"consensus"`
}

type ModelConfig struct {
	Provider   string  `yaml:"provider"`    // anthropic | openai | ollama
	Model      string  `yaml:"model"`
	APIKeyEnv  string  `yaml:"api_key_env"` // env var name
	APIKey     string  `yaml:"-"`           // resolved at runtime, never serialised
	MaxTokens  int     `yaml:"max_tokens"`
	Temperature float64 `yaml:"temperature"`
	TimeoutSec int     `yaml:"timeout_sec"`
}

type ConsensusConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Strategy string `yaml:"strategy"` // majority | unanimous | primary-with-veto
}

// ── Markets ───────────────────────────────────────────────────────────────────

type MarketsConfig struct {
	Stocks      StocksConfig      `yaml:"stocks"`
	Predictions PredictionsConfig `yaml:"predictions"`
	News        NewsAPIConfig     `yaml:"news"`
}

// NewsAPIConfig configures the news data provider (Finnhub or Benzinga).
type NewsAPIConfig struct {
	Provider      string `yaml:"provider"`       // finnhub | benzinga
	APIKeyEnv     string `yaml:"api_key_env"`    // env var name holding the API key
	APIKey        string `yaml:"-"`              // resolved at runtime
	FetchEveryMin int    `yaml:"fetch_every_min"` // cadence in minutes (default 15)
}

type StocksConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Paper     bool   `yaml:"paper"`
	APIKeyEnv string `yaml:"api_key_env"`
	APIKey    string `yaml:"-"`
	SecretEnv string `yaml:"secret_env"`
	Secret    string `yaml:"-"`
	BaseURL   string `yaml:"base_url"`
	DataURL   string `yaml:"data_url"`
}

type PredictionsConfig struct {
	Enabled   bool                          `yaml:"enabled"`
	Platforms map[string]PredictionPlatform `yaml:"platforms"`
}

type PredictionPlatform struct {
	Enabled      bool     `yaml:"enabled"`
	APIKeyEnv    string   `yaml:"api_key_env"`
	APIKey       string   `yaml:"-"`
	Categories   []string `yaml:"categories"`
	MinVolume    float64  `yaml:"min_volume"`
	MaxContracts int      `yaml:"max_contracts"`
}

// ── Strategies ────────────────────────────────────────────────────────────────

type StrategiesConfig struct {
	TrendFollowing   TrendFollowingConfig   `yaml:"trend_following"`
	MeanReversion    MeanReversionConfig    `yaml:"mean_reversion"`
	Momentum         MomentumConfig         `yaml:"momentum"`
	NewsSentiment    NewsSentimentConfig    `yaml:"news_sentiment"`
	PredictionMispricing PredictionMispricingConfig `yaml:"prediction_mispricing"`
}

type TrendFollowingConfig struct {
	Enabled   bool    `yaml:"enabled"`
	EMAPeriod int     `yaml:"ema_period"`
	ADXMin    float64 `yaml:"adx_min"`
	RSIMin    float64 `yaml:"rsi_min"`
	RSIMax    float64 `yaml:"rsi_max"`
}

type MeanReversionConfig struct {
	Enabled          bool    `yaml:"enabled"`
	BBPeriod         int     `yaml:"bb_period"`
	BBStdDev         float64 `yaml:"bb_std_dev"`
	RSIOversold      float64 `yaml:"rsi_oversold"`
	RSIOverbought    float64 `yaml:"rsi_overbought"`
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

type PredictionMispricingConfig struct {
	Enabled   bool    `yaml:"enabled"`
	MinEdge   float64 `yaml:"min_edge"`
	MinVolume float64 `yaml:"min_volume"`
}

// ── Risk ─────────────────────────────────────────────────────────────────────

type RiskConfig struct {
	ConfidenceThreshold float64 `yaml:"confidence_threshold"`
	MinRiskReward       float64 `yaml:"min_risk_reward"`
	MaxPositionPct      float64 `yaml:"max_position_pct"`
	MaxOpenPositions    int     `yaml:"max_open_positions"`
	DailyLossLimitPct   float64 `yaml:"daily_loss_limit_pct"`
	MaxDrawdownPct      float64 `yaml:"max_drawdown_pct"`
	CooldownAfterLoss   int     `yaml:"cooldown_after_loss_min"`
	DefaultStopLossPct  float64 `yaml:"default_stop_loss_pct"`
	DefaultTakeProfitPct float64 `yaml:"default_take_profit_pct"`
	SignalExpiryMin     int     `yaml:"signal_expiry_min"`
	StalePriceMovePct   float64 `yaml:"stale_price_move_pct"`
}

// ── Notifications ─────────────────────────────────────────────────────────────

type NotificationsConfig struct {
	Telegram TelegramConfig `yaml:"telegram"`
}

type TelegramConfig struct {
	Enabled     bool     `yaml:"enabled"`
	BotTokenEnv string   `yaml:"bot_token_env"`
	BotToken    string   `yaml:"-"`
	ChatID      string   `yaml:"chat_id"`
	NotifyOn    []string `yaml:"notify_on"`
}

// ── Logging ───────────────────────────────────────────────────────────────────

type LoggingConfig struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // json | text
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
func Load() (*Config, error) {
	mu.Lock()
	defer mu.Unlock()

	cfg := Defaults()
	path := ConfigPath()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// First run — create from defaults.
			if err := save(cfg, path); err != nil {
				return nil, fmt.Errorf("create default config: %w", err)
			}
		} else {
			return nil, fmt.Errorf("read config: %w", err)
		}
	} else {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}

	resolveEnvVars(cfg)
	current = cfg
	return cfg, nil
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

	parts := strings.SplitN(dotPath, ".", 2)
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

	validStrategies := map[string]bool{"majority": true, "unanimous": true, "primary-with-veto": true}
	if cfg.Models.Consensus.Enabled && !validStrategies[cfg.Models.Consensus.Strategy] {
		errs = append(errs, fmt.Sprintf("models.consensus.strategy must be one of majority|unanimous|primary-with-veto, got %q", cfg.Models.Consensus.Strategy))
	}

	if cfg.Models.Primary.APIKeyEnv != "" && os.Getenv(cfg.Models.Primary.APIKeyEnv) == "" {
		errs = append(errs, fmt.Sprintf("env var %s (models.primary.api_key_env) is not set", cfg.Models.Primary.APIKeyEnv))
	}

	if cfg.Markets.Stocks.APIKeyEnv != "" && os.Getenv(cfg.Markets.Stocks.APIKeyEnv) == "" {
		errs = append(errs, fmt.Sprintf("env var %s (markets.stocks.api_key_env) is not set", cfg.Markets.Stocks.APIKeyEnv))
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

	return errs
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func resolveEnvVars(cfg *Config) {
	cfg.Models.Primary.APIKey = resolveEnv(cfg.Models.Primary.APIKeyEnv)
	cfg.Markets.Stocks.APIKey = resolveEnv(cfg.Markets.Stocks.APIKeyEnv)
	cfg.Markets.Stocks.Secret = resolveEnv(cfg.Markets.Stocks.SecretEnv)
	cfg.Markets.News.APIKey = resolveEnv(cfg.Markets.News.APIKeyEnv)
	cfg.Notifications.Telegram.BotToken = resolveEnv(cfg.Notifications.Telegram.BotTokenEnv)
	for k, p := range cfg.Markets.Predictions.Platforms {
		p.APIKey = resolveEnv(p.APIKeyEnv)
		cfg.Markets.Predictions.Platforms[k] = p
	}
}

func resolveEnv(key string) string {
	if key == "" {
		return ""
	}
	return os.Getenv(key)
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

// setYAMLNode sets a dotted-path key in a YAML mapping node tree.
func setYAMLNode(node *yaml.Node, parts []string, value string) {
	if node.Kind != yaml.MappingNode {
		return
	}
	key := parts[0]
	for i := 0; i < len(node.Content)-1; i += 2 {
		if node.Content[i].Value == key {
			if len(parts) == 1 {
				node.Content[i+1].Value = value
			} else {
				setYAMLNode(node.Content[i+1], parts[1:], value)
			}
			return
		}
	}
	// Key not found — append.
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
	if len(parts) == 1 {
		valNode := &yaml.Node{Kind: yaml.ScalarNode, Value: value}
		node.Content = append(node.Content, keyNode, valNode)
	} else {
		child := &yaml.Node{Kind: yaml.MappingNode}
		node.Content = append(node.Content, keyNode, child)
		setYAMLNode(child, parts[1:], value)
	}
}
