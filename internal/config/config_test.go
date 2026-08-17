package config

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if cfg.Risk.ConfidenceThreshold != 0.55 {
		t.Errorf("default confidence_threshold = %f, want 0.55", cfg.Risk.ConfidenceThreshold)
	}
}

func TestDefaultsTrailingRulePreservesATRBehavior(t *testing.T) {
	cfg := Defaults()
	if cfg.Risk.TrailingStop.Rule != "atr" {
		t.Fatalf("default trailing rule = %q, want atr", cfg.Risk.TrailingStop.Rule)
	}
	if cfg.Risk.UncapTakeProfit {
		t.Fatal("take-profit uncapping must default off")
	}
	if cfg.Risk.ExitConfMin != 0 {
		t.Fatalf("default exit confidence minimum = %v, want 0", cfg.Risk.ExitConfMin)
	}
	if cfg.Risk.EntryGates.TrendAlignmentEnabled {
		t.Fatal("entry trend-alignment gate must default off")
	}
	if cfg.Risk.EntryGates.PullbackStrengthEnabled {
		t.Fatal("complete pullback-strength gate must default off")
	}
}

func TestDefaultsScannerUniverseDisabled(t *testing.T) {
	universe := Defaults().Scanner.Universe
	if universe.Enabled {
		t.Fatal("full-market scanner universe must default off")
	}
	if universe.Provider != "alpaca" ||
		universe.SnapshotFeed != "delayed_sip" ||
		universe.HistoricalFeed != "sip" ||
		universe.SIPRecentDataDelayMinutes != 16 ||
		universe.AfterCloseDelayMinutes != 30 ||
		universe.RunLeaseMinutes != 120 ||
		universe.MinMarketDataCoverage != 0.90 ||
		universe.MinPrice != 5 ||
		universe.MinMarketCapMM != 100 ||
		universe.MinMedianDollarVolume != 1_000_000 ||
		universe.DollarVolumeLookbackDays != 20 ||
		universe.MaxSpreadBPS != 0 ||
		universe.SnapshotTTLHours != 24 ||
		universe.TaxonomyMaxAgeHours != 168 ||
		universe.BatchSize != 200 ||
		universe.MaxConcurrency != 4 ||
		universe.TaxonomyRefreshLimit != 200 {
		t.Fatalf("unexpected scanner universe defaults: %+v", universe)
	}
	if len(universe.AllowedSecurityTypes) != 1 ||
		universe.AllowedSecurityTypes[0] != "us_equity" {
		t.Fatalf("unexpected default security types: %v", universe.AllowedSecurityTypes)
	}
	if len(universe.AllowedExchanges) != 3 ||
		universe.AllowedExchanges[0] != "NYSE" ||
		universe.AllowedExchanges[1] != "NASDAQ" ||
		universe.AllowedExchanges[2] != "AMEX" {
		t.Fatalf("unexpected default exchanges: %v", universe.AllowedExchanges)
	}
	if !universe.RequireActive || !universe.RequireTradable || !universe.RequireMarketData {
		t.Fatalf("default universe eligibility must fail closed: %+v", universe)
	}
	if universe.MissingFundamentalsPolicy != "exclude" ||
		universe.MissingMarketDataPolicy != "exclude" {
		t.Fatalf("default missing-data policies must exclude: %+v", universe)
	}
}

func TestDefaultsWeeklyLeaderDisabled(t *testing.T) {
	leader := Defaults().Scanner.WeeklyLeader
	if leader.Enabled || leader.PromoteCandidates {
		t.Fatal("weekly leader discovery and promotion must default off")
	}
	if leader.AfterCloseDelayMinutes != 15 ||
		leader.MaxPromotionsPerRun != 20 ||
		leader.UniverseMaxAgeHours != 36 ||
		leader.HistoryCalendarDays != 420 ||
		leader.MinHistorySessions != 253 ||
		leader.Benchmark != "SPY" ||
		leader.IndustryMinPeers != 5 ||
		leader.SelectionPercentile != 0.90 ||
		leader.MinCompositeScore != 0.70 ||
		leader.MinFeatureCoverage != 0.90 ||
		leader.BatchSize != 200 ||
		leader.RunLeaseMinutes != 60 {
		t.Fatalf("unexpected weekly leader defaults: %+v", leader)
	}
}

func TestDefaultsPromotionCorrelationDisabled(t *testing.T) {
	scanner := Defaults().Scanner
	if scanner.PromotionCorrelationEnabled || scanner.PromotionMaxAbsCorrelation != .85 ||
		scanner.PromotionCorrelationLookbackDays != 60 || scanner.PromotionCorrelationMinOverlap != 30 {
		t.Fatalf("unexpected promotion correlation defaults: %+v", scanner)
	}
}

func TestValidatePromotionCorrelationRequiresScanner(t *testing.T) {
	cfg := Defaults()
	cfg.Scanner.PromotionCorrelationEnabled = true
	if errs := Validate(cfg); !containsStr(errs, "promotion_correlation_enabled") {
		t.Fatalf("expected scanner dependency validation error, got %v", errs)
	}
	cfg.Scanner.Enabled = true
	cfg.Scanner.PromotionCorrelationMinOverlap = cfg.Scanner.PromotionCorrelationLookbackDays + 1
	if errs := Validate(cfg); !containsStr(errs, "promotion_correlation_min_overlap") {
		t.Fatalf("expected overlap validation error, got %v", errs)
	}
}

func TestValidatePromotionCorrelationRequiresHistoricalFeed(t *testing.T) {
	cfg := Defaults()
	cfg.Scanner.Enabled = true
	cfg.Scanner.PromotionCorrelationEnabled = true
	cfg.Scanner.Universe.HistoricalFeed = ""
	if errs := Validate(cfg); !containsStr(errs, "scanner.universe.historical_feed") {
		t.Fatalf("missing correlation history feed accepted: %v", errs)
	}
	cfg.Scanner.Universe.HistoricalFeed = "sip"
	cfg.Scanner.Universe.SIPRecentDataDelayMinutes = -1
	if errs := Validate(cfg); !containsStr(errs, "scanner.universe.sip_recent_data_delay_minutes") {
		t.Fatalf("invalid correlation SIP entitlement delay accepted: %v", errs)
	}
}

func TestValidatePromotionCorrelationThresholdEvenWhenDisabled(t *testing.T) {
	cfg := Defaults()
	cfg.Scanner.PromotionMaxAbsCorrelation = 0
	if errs := Validate(cfg); !containsStr(errs, "promotion_max_abs_correlation") {
		t.Fatalf("expected threshold validation error while disabled, got %v", errs)
	}
}

func TestValidateWeeklyLeaderConfig(t *testing.T) {
	valid := Defaults()
	valid.Scanner.Enabled = true
	valid.Scanner.Universe.Enabled = true
	valid.Scanner.WeeklyLeader.Enabled = true
	if errs := Validate(valid); containsStr(errs, "scanner.weekly_leader") {
		t.Fatalf("valid weekly leader config rejected: %v", errs)
	}

	disabled := Defaults()
	disabled.Scanner.WeeklyLeader = WeeklyLeaderConfig{}
	if errs := Validate(disabled); containsStr(errs, "scanner.weekly_leader") {
		t.Fatalf("disabled zero-value weekly leader must preserve existing configs: %v", errs)
	}

	tests := []struct {
		name  string
		field string
		apply func(*Config)
	}{
		{"scanner master", "requires scanner.enabled", func(c *Config) { c.Scanner.Enabled = false }},
		{"universe master", "requires scanner.universe.enabled", func(c *Config) { c.Scanner.Universe.Enabled = false }},
		{"delay", "after_close_delay_minutes", func(c *Config) { c.Scanner.WeeklyLeader.AfterCloseDelayMinutes = -1 }},
		{"delay overflow", "after_close_delay_minutes", func(c *Config) {
			c.Scanner.WeeklyLeader.AfterCloseDelayMinutes = int((time.Duration(1<<63-1) / time.Minute) + 1)
		}},
		{"promotion cap", "max_promotions_per_run", func(c *Config) { c.Scanner.WeeklyLeader.MaxPromotionsPerRun = 0 }},
		{"universe age", "universe_max_age_hours", func(c *Config) { c.Scanner.WeeklyLeader.UniverseMaxAgeHours = 0 }},
		{"universe age overflow", "universe_max_age_hours", func(c *Config) {
			c.Scanner.WeeklyLeader.UniverseMaxAgeHours = int((time.Duration(1<<63-1) / time.Hour) + 1)
		}},
		{"calendar history", "history_calendar_days", func(c *Config) { c.Scanner.WeeklyLeader.HistoryCalendarDays = 399 }},
		{"calendar/session mismatch", "history_calendar_days", func(c *Config) {
			c.Scanner.WeeklyLeader.MinHistorySessions = 500
			c.Scanner.WeeklyLeader.HistoryCalendarDays = 500
		}},
		{"calendar history too large", "history_calendar_days", func(c *Config) {
			c.Scanner.WeeklyLeader.HistoryCalendarDays = weeklyLeaderMaxHistoryCalendarDays + 1
		}},
		{"session history overflow", "min_history_sessions", func(c *Config) {
			c.Scanner.WeeklyLeader.MinHistorySessions = int(^uint(0) >> 1)
			c.Scanner.WeeklyLeader.HistoryCalendarDays = int(^uint(0) >> 1)
		}},
		{"session history", "min_history_sessions", func(c *Config) { c.Scanner.WeeklyLeader.MinHistorySessions = 252 }},
		{"benchmark", "benchmark", func(c *Config) { c.Scanner.WeeklyLeader.Benchmark = " " }},
		{"peers", "industry_min_peers", func(c *Config) { c.Scanner.WeeklyLeader.IndustryMinPeers = 0 }},
		{"selection", "selection_percentile", func(c *Config) { c.Scanner.WeeklyLeader.SelectionPercentile = 1 }},
		{"score", "min_composite_score", func(c *Config) { c.Scanner.WeeklyLeader.MinCompositeScore = 1.1 }},
		{"coverage", "min_feature_coverage", func(c *Config) { c.Scanner.WeeklyLeader.MinFeatureCoverage = 0 }},
		{"batch", "batch_size", func(c *Config) { c.Scanner.WeeklyLeader.BatchSize = 0 }},
		{"lease", "run_lease_minutes", func(c *Config) { c.Scanner.WeeklyLeader.RunLeaseMinutes = 0 }},
		{"lease overflow", "run_lease_minutes", func(c *Config) {
			c.Scanner.WeeklyLeader.RunLeaseMinutes = int((time.Duration(1<<63-1) / time.Minute) + 1)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Scanner.Enabled = true
			cfg.Scanner.Universe.Enabled = true
			cfg.Scanner.WeeklyLeader.Enabled = true
			tt.apply(cfg)
			if !containsStr(Validate(cfg), "scanner.weekly_leader."+tt.field) &&
				!containsStr(Validate(cfg), "scanner.weekly_leader "+tt.field) {
				t.Fatalf("invalid %s not rejected: %v", tt.field, Validate(cfg))
			}
		})
	}

	t.Run("duration boundaries", func(t *testing.T) {
		cfg := Defaults()
		cfg.Scanner.Enabled = true
		cfg.Scanner.Universe.Enabled = true
		cfg.Scanner.WeeklyLeader.Enabled = true
		cfg.Scanner.WeeklyLeader.AfterCloseDelayMinutes = int(
			time.Duration(1<<63-1) / time.Minute,
		)
		cfg.Scanner.WeeklyLeader.UniverseMaxAgeHours = int(
			time.Duration(1<<63-1) / time.Hour,
		)
		cfg.Scanner.WeeklyLeader.RunLeaseMinutes = int(
			time.Duration(1<<63-1) / time.Minute,
		)
		if errs := Validate(cfg); containsStr(errs, "overflows time.Duration") {
			t.Fatalf("safe duration boundary rejected: %v", errs)
		}
	})

	t.Run("calendar session boundary", func(t *testing.T) {
		cfg := Defaults()
		cfg.Scanner.Enabled = true
		cfg.Scanner.Universe.Enabled = true
		cfg.Scanner.WeeklyLeader.Enabled = true
		cfg.Scanner.WeeklyLeader.MinHistorySessions = 500
		required, ok := weeklyLeaderHistoryCalendarDays(500)
		if !ok {
			t.Fatal("ordinary session count unexpectedly overflowed")
		}
		cfg.Scanner.WeeklyLeader.HistoryCalendarDays = required
		if errs := Validate(cfg); containsStr(errs, "scanner.weekly_leader") {
			t.Fatalf("exact calendar/session boundary rejected: %v", errs)
		}
		cfg.Scanner.WeeklyLeader.HistoryCalendarDays = required - 1
		if !containsStr(Validate(cfg), "history_calendar_days") {
			t.Fatalf("undersized calendar window accepted: %v", Validate(cfg))
		}
	})
}

func TestValidateScannerUniverseConfig(t *testing.T) {
	cfg := Defaults()
	cfg.Scanner.Enabled = true
	cfg.Scanner.Universe.Enabled = true
	if errs := Validate(cfg); containsStr(errs, "scanner.universe") {
		t.Fatalf("valid scanner universe config rejected: %v", errs)
	}

	disabled := Defaults()
	disabled.Scanner.Universe = UniverseConfig{}
	if errs := Validate(disabled); containsStr(errs, "scanner.universe") {
		t.Fatalf("disabled zero-value universe must preserve existing configs: %v", errs)
	}

	tests := []struct {
		name   string
		mutate func(*UniverseConfig)
		field  string
	}{
		{"provider missing", func(c *UniverseConfig) { c.Provider = "" }, "provider"},
		{"after-close delay", func(c *UniverseConfig) { c.AfterCloseDelayMinutes = -1 }, "after_close_delay_minutes"},
		{"consolidated feed delay", func(c *UniverseConfig) { c.AfterCloseDelayMinutes = 15 }, "after_close_delay_minutes"},
		{"run lease", func(c *UniverseConfig) { c.RunLeaseMinutes = 0 }, "run_lease_minutes"},
		{"market-data coverage zero", func(c *UniverseConfig) { c.MinMarketDataCoverage = 0 }, "min_market_data_coverage"},
		{"market-data coverage high", func(c *UniverseConfig) { c.MinMarketDataCoverage = 1.01 }, "min_market_data_coverage"},
		{"market-data coverage NaN", func(c *UniverseConfig) { c.MinMarketDataCoverage = math.NaN() }, "min_market_data_coverage"},
		{"market-data coverage +Inf", func(c *UniverseConfig) { c.MinMarketDataCoverage = math.Inf(1) }, "min_market_data_coverage"},
		{"market-data coverage -Inf", func(c *UniverseConfig) { c.MinMarketDataCoverage = math.Inf(-1) }, "min_market_data_coverage"},
		{"provider unknown", func(c *UniverseConfig) { c.Provider = "unknown" }, "provider"},
		{"snapshot feed missing", func(c *UniverseConfig) { c.SnapshotFeed = "" }, "snapshot_feed"},
		{"snapshot feed unknown", func(c *UniverseConfig) { c.SnapshotFeed = "boats" }, "snapshot_feed"},
		{"historical feed missing", func(c *UniverseConfig) { c.HistoricalFeed = "" }, "historical_feed"},
		{"historical delayed feed", func(c *UniverseConfig) { c.HistoricalFeed = "delayed_sip" }, "historical_feed"},
		{"SIP recent-data delay", func(c *UniverseConfig) { c.SIPRecentDataDelayMinutes = -1 }, "sip_recent_data_delay_minutes"},
		{"min price", func(c *UniverseConfig) { c.MinPrice = -1 }, "min_price"},
		{"market cap", func(c *UniverseConfig) { c.MinMarketCapMM = -1 }, "min_market_cap_mm"},
		{"dollar volume", func(c *UniverseConfig) { c.MinMedianDollarVolume = -1 }, "min_median_dollar_volume"},
		{"lookback", func(c *UniverseConfig) { c.DollarVolumeLookbackDays = 0 }, "dollar_volume_lookback_days"},
		{"security types empty", func(c *UniverseConfig) { c.AllowedSecurityTypes = nil }, "allowed_security_types"},
		{"security types blank", func(c *UniverseConfig) { c.AllowedSecurityTypes = []string{""} }, "allowed_security_types"},
		{"exchanges empty", func(c *UniverseConfig) { c.AllowedExchanges = nil }, "allowed_exchanges"},
		{"exchanges blank", func(c *UniverseConfig) { c.AllowedExchanges = []string{" "} }, "allowed_exchanges"},
		{"spread", func(c *UniverseConfig) { c.MaxSpreadBPS = -1 }, "max_spread_bps"},
		{"snapshot ttl", func(c *UniverseConfig) { c.SnapshotTTLHours = 0 }, "snapshot_ttl_hours"},
		{"taxonomy age", func(c *UniverseConfig) { c.TaxonomyMaxAgeHours = 0 }, "taxonomy_max_age_hours"},
		{"batch size", func(c *UniverseConfig) { c.BatchSize = 0 }, "batch_size"},
		{"concurrency", func(c *UniverseConfig) { c.MaxConcurrency = 0 }, "max_concurrency"},
		{"taxonomy limit", func(c *UniverseConfig) { c.TaxonomyRefreshLimit = 0 }, "taxonomy_refresh_limit"},
		{"fundamentals policy", func(c *UniverseConfig) { c.MissingFundamentalsPolicy = "keep" }, "missing_fundamentals_policy"},
		{"market data policy", func(c *UniverseConfig) { c.MissingMarketDataPolicy = "keep" }, "missing_market_data_policy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Scanner.Enabled = true
			cfg.Scanner.Universe.Enabled = true
			tt.mutate(&cfg.Scanner.Universe)
			if !containsStr(Validate(cfg), "scanner.universe."+tt.field) {
				t.Fatalf("invalid %s not rejected: %v", tt.field, Validate(cfg))
			}
		})
	}
}

func TestValidateScannerUniverseAllowsRealTimeSIPEntitlement(t *testing.T) {
	cfg := Defaults()
	cfg.Scanner.Enabled = true
	cfg.Scanner.Universe.Enabled = true
	cfg.Scanner.Universe.SnapshotFeed = "sip"
	cfg.Scanner.Universe.HistoricalFeed = "sip"
	cfg.Scanner.Universe.SIPRecentDataDelayMinutes = 0
	cfg.Scanner.Universe.AfterCloseDelayMinutes = 0
	if errs := Validate(cfg); containsStr(errs, "scanner.universe") {
		t.Fatalf("real-time SIP entitlement config rejected: %v", errs)
	}
}

func TestValidateScannerUniverseScheduleCoversConfiguredSIPDelay(t *testing.T) {
	cfg := Defaults()
	cfg.Scanner.Enabled = true
	cfg.Scanner.Universe.Enabled = true
	cfg.Scanner.Universe.SnapshotFeed = "sip"
	cfg.Scanner.Universe.SIPRecentDataDelayMinutes = 20
	cfg.Scanner.Universe.AfterCloseDelayMinutes = 19
	if errs := Validate(cfg); !containsStr(errs, "scanner.universe.after_close_delay_minutes") {
		t.Fatalf("schedule shorter than configured SIP delay accepted: %v", errs)
	}
}

func TestValidateScannerUniverseScheduleCoversDelayedSnapshots(t *testing.T) {
	cfg := Defaults()
	cfg.Scanner.Enabled = true
	cfg.Scanner.Universe.Enabled = true
	cfg.Scanner.Universe.SnapshotFeed = "delayed_sip"
	cfg.Scanner.Universe.HistoricalFeed = "iex"
	cfg.Scanner.Universe.SIPRecentDataDelayMinutes = 0
	cfg.Scanner.Universe.AfterCloseDelayMinutes = 15
	if errs := Validate(cfg); !containsStr(errs, "scanner.universe.after_close_delay_minutes") {
		t.Fatalf("schedule shorter than delayed snapshot boundary accepted: %v", errs)
	}
}

func TestValidateScannerPromotionConstraints(t *testing.T) {
	cfg := Defaults()
	cfg.Scanner.PromotionMaxPerIndustry = 2
	cfg.Scanner.Universe.Enabled = true
	if errs := Validate(cfg); containsStr(errs, "scanner.max_") {
		t.Fatalf("valid promotion constraints rejected: %v", errs)
	}

	cfg.Scanner.PromotionMaxPerIndustry = -1
	if !containsStr(Validate(cfg), "scanner.max_promotions_per_industry") {
		t.Fatal("negative per-industry cap should be rejected")
	}

	cfg = Defaults()
	cfg.Scanner.PromotionMaxPerIndustry = 2
	if !containsStr(Validate(cfg), "requires scanner.universe.enabled=true") {
		t.Fatal("promotion cap should require point-in-time universe")
	}

}

func TestValidateExitPolicyConfig(t *testing.T) {
	cfg := Defaults()
	cfg.Risk.TrailingStop.Rule = "ema9"
	cfg.Risk.ExitConfMin = 0.72
	if containsStr(Validate(cfg), "risk.trailing_stop.rule") ||
		containsStr(Validate(cfg), "risk.exit_conf_min") {
		t.Fatalf("valid exit policy rejected: %v", Validate(cfg))
	}

	cfg.Risk.TrailingStop.Rule = "bollinger"
	if !containsStr(Validate(cfg), "risk.trailing_stop.rule") {
		t.Fatalf("invalid trailing rule not rejected: %v", Validate(cfg))
	}
	cfg.Risk.TrailingStop.Rule = "atr"
	cfg.Risk.ExitConfMin = 1.01
	if !containsStr(Validate(cfg), "risk.exit_conf_min") {
		t.Fatalf("invalid confidence minimum not rejected: %v", Validate(cfg))
	}
}

func TestValidatePullbackStrengthConfig(t *testing.T) {
	cfg := Defaults()
	cfg.Risk.EntryGates.PullbackStrengthEnabled = true
	cfg.Risk.EntryGates.PullbackMinMomentumReturnPct = 30
	cfg.Risk.EntryGates.PullbackConsolidationSessions = 40
	cfg.Risk.EntryGates.PullbackSupportTolerancePct = 1.5
	cfg.Risk.EntryGates.PullbackMaxStopWidthPct = 4
	if errs := Validate(cfg); containsStr(errs, "risk.entry_gates.pullback") {
		t.Fatalf("valid pullback config rejected: %v", errs)
	}

	cfg.Risk.EntryGates.PullbackConsolidationSessions = 10
	if !containsStr(Validate(cfg), "risk.entry_gates.pullback_consolidation_sessions") {
		t.Fatal("short consolidation window should be rejected")
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

func TestValidateStockBrokerConfigPaperLiveHostMatrix(t *testing.T) {
	tests := []struct {
		name    string
		paper   bool
		baseURL string
		wantErr bool
	}{
		{"paper canonical", true, AlpacaPaperTradingURL, false},
		{"live canonical", false, AlpacaLiveTradingURL, false},
		{"paper pointed live", true, AlpacaLiveTradingURL, true},
		{"live pointed paper", false, AlpacaPaperTradingURL, true},
		{"unknown host", true, "https://example.com", true},
		{"non HTTPS", true, "http://paper-api.alpaca.markets", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStockBrokerConfig(StocksConfig{Enabled: true, Paper: tt.paper, BaseURL: tt.baseURL})
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateStockBrokerConfig() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestValidateStockBrokerConfigDisabledSkipsHostValidation(t *testing.T) {
	if err := ValidateStockBrokerConfig(StocksConfig{Enabled: false, BaseURL: "not a URL"}); err != nil {
		t.Fatalf("disabled stocks config rejected: %v", err)
	}
}

func TestValidateStockWorkflowConfig(t *testing.T) {
	if err := ValidateStockWorkflowConfig("paper", StocksConfig{Enabled: true, Paper: true}); err != nil {
		t.Fatalf("paper workflow + paper Alpaca rejected: %v", err)
	}
	if err := ValidateStockWorkflowConfig("paper", StocksConfig{Enabled: true, Paper: false}); err == nil {
		t.Fatal("paper workflow + live Alpaca accepted")
	}
	if err := ValidateStockWorkflowConfig("auto", StocksConfig{Enabled: true, Paper: true}); err != nil {
		t.Fatalf("auto paper testing rejected: %v", err)
	}
	if err := ValidateStockWorkflowConfig("auto", StocksConfig{Enabled: true, Paper: false}); err != nil {
		t.Fatalf("auto live execution rejected: %v", err)
	}
	for _, paper := range []bool{true, false} {
		if err := ValidateStockWorkflowConfig("manual", StocksConfig{Enabled: true, Paper: paper}); err != nil {
			t.Fatalf("manual paper=%t rejected: %v", paper, err)
		}
	}
}

func TestValidateStocksCredentialsRequireBothWhenEnabled(t *testing.T) {
	keyEnv, secretEnv := "TE_TEST_ALPACA_KEY", "TE_TEST_ALPACA_SECRET"
	t.Setenv(keyEnv, "")
	t.Setenv(secretEnv, "")
	cfg := Defaults()
	cfg.Markets.Stocks.Enabled = true
	cfg.Markets.Stocks.APIKeyEnv = keyEnv
	cfg.Markets.Stocks.SecretEnv = secretEnv
	errs := Validate(cfg)
	if !containsStr(errs, keyEnv) || !containsStr(errs, secretEnv) {
		t.Fatalf("missing stock credentials were not both reported: %v", errs)
	}

	cfg.Markets.Stocks.Enabled = false
	errs = Validate(cfg)
	if containsStr(errs, keyEnv) || containsStr(errs, secretEnv) {
		t.Fatalf("disabled stocks still required credentials: %v", errs)
	}
}

func TestValidateValidModes(t *testing.T) {
	for _, mode := range []string{"manual", "auto", "paper", "forex", "orb-forex", "orb-futures", "orb-futures-metals", "orb-newyork", "orb-options"} {
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

func TestValidateORBAutoThrottle(t *testing.T) {
	base := func() *Config {
		cfg := Defaults()
		cfg.Feedback.Windows = []int{1, 7, 30}
		cfg.Feedback.ORBAutoThrottle = ORBAutoThrottleConfig{
			Enabled: true, Window: "30d", Floor: 0.5, Cap: 1.5,
		}
		return cfg
	}

	// Valid config → no auto-throttle error.
	for _, e := range Validate(base()) {
		if strings.Contains(e, "orb_auto_throttle") {
			t.Errorf("unexpected auto-throttle error on valid config: %s", e)
		}
	}

	// Window not among configured feedback.windows → error.
	cfg := base()
	cfg.Feedback.ORBAutoThrottle.Window = "14d"
	if !containsStr(Validate(cfg), "orb_auto_throttle.window") {
		t.Error("expected window-not-in-windows error")
	}

	// floor > cap → error.
	cfg = base()
	cfg.Feedback.ORBAutoThrottle.Floor = 2.0
	cfg.Feedback.ORBAutoThrottle.Cap = 1.0
	if !containsStr(Validate(cfg), "orb_auto_throttle") {
		t.Error("expected floor>cap error")
	}

	// Disabled → not validated even if nonsensical.
	cfg = base()
	cfg.Feedback.ORBAutoThrottle.Enabled = false
	cfg.Feedback.ORBAutoThrottle.Window = "999d"
	for _, e := range Validate(cfg) {
		if strings.Contains(e, "orb_auto_throttle") {
			t.Errorf("disabled throttle should not be validated: %s", e)
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

func TestValidateSpecialistModeRequiresEveryRole(t *testing.T) {
	cfg := Defaults()
	cfg.Analysis.SpecialistModeEnabled = true
	cfg.Analysis.SpecialistRoles = map[string]string{
		"fundamental": "analyst_frontier",
		"technical":   "analyst_mid",
		"sentiment":   "analyst_cheap",
		// risk_manager intentionally missing
	}
	errs := Validate(cfg)
	if !containsStr(errs, "analysis.specialist_roles.risk_manager is required") {
		t.Fatalf("expected mandatory risk-manager validation error, got: %v", errs)
	}

	cfg.Analysis.SpecialistRoles["risk_manager"] = "does_not_exist"
	errs = Validate(cfg)
	if !containsStr(errs, `analysis.specialist_roles.risk_manager: key "does_not_exist" not found`) {
		t.Fatalf("expected unresolved risk-manager registry error, got: %v", errs)
	}
}

func TestValidateSpecialistModeChecksRoleAPIKeysAndFallbacks(t *testing.T) {
	cfg := Defaults()
	cfg.Analysis.SpecialistModeEnabled = true
	cfg.Analysis.SpecialistRoles = map[string]string{
		"fundamental":  "analyst_frontier",
		"technical":    "analyst_mid",
		"sentiment":    "analyst_cheap",
		"risk_manager": "specialist_risk",
	}
	cfg.Models.Registry["specialist_risk"] = ModelRegistryEntry{
		ModelConfig: ModelConfig{
			Provider:  "special-provider",
			Model:     "risk-model",
			APIKeyEnv: "TE_TEST_SPECIALIST_KEY_NOTSET",
		},
		Fallback: "specialist_fallback",
	}
	cfg.Models.Registry["specialist_fallback"] = ModelRegistryEntry{
		ModelConfig: ModelConfig{
			Provider:  "fallback-provider",
			Model:     "fallback-model",
			APIKeyEnv: "TE_TEST_SPECIALIST_FALLBACK_KEY_NOTSET",
		},
	}
	os.Unsetenv("TE_TEST_SPECIALIST_KEY_NOTSET")
	os.Unsetenv("TE_TEST_SPECIALIST_FALLBACK_KEY_NOTSET")

	errs := Validate(cfg)
	for _, envName := range []string{"TE_TEST_SPECIALIST_KEY_NOTSET", "TE_TEST_SPECIALIST_FALLBACK_KEY_NOTSET"} {
		if !containsStr(errs, envName) {
			t.Errorf("expected missing specialist API key error for %s, got: %v", envName, errs)
		}
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

func TestAnalystNodeIDIsStableAcrossRegistryAndLegacyConfigs(t *testing.T) {
	registry := ModelsConfig{Roles: ModelRoles{Analysts: []string{"cheap", "frontier"}}}
	if got := registry.AnalystNodeID(0); got != "analyst-cheap" {
		t.Fatalf("registry primary ID = %q, want analyst-cheap", got)
	}
	if got := registry.AnalystNodeID(1); got != "analyst-frontier" {
		t.Fatalf("registry secondary ID = %q, want analyst-frontier", got)
	}

	legacy := ModelsConfig{}
	if got := legacy.AnalystNodeID(0); got != "llm_analysis" {
		t.Fatalf("legacy primary ID = %q, want llm_analysis", got)
	}
	if got := legacy.AnalystNodeID(1); got != "analyst-1" {
		t.Fatalf("legacy secondary ID = %q, want analyst-1", got)
	}
}

func TestValidateOptionsORB(t *testing.T) {
	cfg := Defaults()
	cfg.OptionsORB.Enabled = true
	cfg.OptionsORB.Underlyings = []string{"SPY"}
	if errs := Validate(cfg); containsStr(errs, "options_orb") {
		t.Fatalf("unexpected options ORB validation error: %v", errs)
	}
	cfg.OptionsORB.RangeMinutes = 10
	if !containsStr(Validate(cfg), "options_orb.range_minutes") {
		t.Fatalf("expected options range validation error")
	}
	cfg.OptionsORB.RangeMinutes = 15
	cfg.OptionsORB.RiskReward = 1
	if !containsStr(Validate(cfg), "options_orb.risk_reward") {
		t.Fatalf("expected options risk/reward validation error")
	}
	cfg.OptionsORB.RiskReward = 2
	cfg.OptionsORB.Underlyings = nil
	if !containsStr(Validate(cfg), "options_orb.underlyings") {
		t.Fatalf("expected options underlyings validation error")
	}
}

func TestValidateCurrencyStrengthValidDefaults(t *testing.T) {
	cfg := Defaults()
	cfg.CurrencyStrength.Enabled = true
	errs := Validate(cfg)
	for _, e := range errs {
		if strings.Contains(e, "currency_strength") {
			t.Errorf("unexpected currency_strength validation error with defaults: %s", e)
		}
	}
}

func TestValidateCurrencyStrengthRejectsLiveMode(t *testing.T) {
	cfg := Defaults()
	cfg.CurrencyStrength.Enabled = true
	cfg.CurrencyStrength.Mode = "live"
	errs := Validate(cfg)
	if !containsStr(errs, "currency_strength.mode live is not supported yet") {
		t.Errorf("expected currency_strength live-mode error, got: %v", errs)
	}
}

func TestValidateCurrencyStrengthTradePairsMustBeRanked(t *testing.T) {
	cfg := Defaults()
	cfg.CurrencyStrength.Enabled = true
	cfg.CurrencyStrength.TradeInstruments = []string{"AUD/USD"}
	errs := Validate(cfg)
	if !containsStr(errs, "currency_strength.trade_instruments pair") {
		t.Errorf("expected currency_strength trade-instrument subset error, got: %v", errs)
	}
}

func TestValidateCurrencyStrengthTimesMustBeOrdered(t *testing.T) {
	cfg := Defaults()
	cfg.CurrencyStrength.Enabled = true
	cfg.CurrencyStrength.StrengthStart = "09:00"
	cfg.CurrencyStrength.StrengthEnd = "06:00"
	errs := Validate(cfg)
	if !containsStr(errs, "currency_strength times") {
		t.Errorf("expected currency_strength time-order error, got: %v", errs)
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

func TestForexORBEffectiveMaxHoldMinutes(t *testing.T) {
	cfg := ForexORBConfig{
		MaxHoldMinutes: 240, // strategy-level default
		Instruments: []Instrument{
			{Symbol: "EUR/USD", MaxHoldMinutes: 120}, // per-instrument override
			{Symbol: "GBP/USD", MaxHoldMinutes: 0},   // inherits the strategy default
		},
	}
	cases := []struct {
		symbol string
		want   int
	}{
		{"EUR/USD", 120}, // instrument override wins
		{"GBP/USD", 240}, // zero override falls back to strategy default
		{"XAU/USD", 240}, // unknown symbol falls back to strategy default (stray position still capped)
	}
	for _, c := range cases {
		if got := cfg.EffectiveMaxHoldMinutes(c.symbol); got != c.want {
			t.Errorf("EffectiveMaxHoldMinutes(%q) = %d; want %d", c.symbol, got, c.want)
		}
	}

	// With no strategy default and no override, time exits are disabled (0).
	none := ForexORBConfig{Instruments: []Instrument{{Symbol: "EUR/USD"}}}
	if got := none.EffectiveMaxHoldMinutes("EUR/USD"); got != 0 {
		t.Errorf("EffectiveMaxHoldMinutes with no config = %d; want 0 (disabled)", got)
	}
}

func TestValidateForexBreakoutConfirmation(t *testing.T) {
	cfg := Defaults()
	cfg.ForexORB.Enabled = true
	cfg.ForexORB.Instruments = []Instrument{
		{Symbol: "EUR/USD", BreakoutConfirmation: "close_1m"}, // valid
		{Symbol: "GBP/USD", BreakoutConfirmation: "close_3"},  // typo → must error
	}
	errs := Validate(cfg)
	found := false
	for _, e := range errs {
		if strings.Contains(e, "GBP/USD") && strings.Contains(e, "breakout_confirmation") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a validation error for the invalid breakout_confirmation; got %v", errs)
	}

	// A valid set produces no confirmation error — including retest modes, now live-wired.
	for _, mode := range []string{"stop_touch", "close_1m", "close_3m", "retest_swing", "retest_range", ""} {
		cfg.ForexORB.Instruments = []Instrument{{Symbol: "EUR/USD", BreakoutConfirmation: mode}}
		for _, e := range Validate(cfg) {
			if strings.Contains(e, "breakout_confirmation") {
				t.Errorf("mode %q wrongly rejected: %s", mode, e)
			}
		}
	}
}

func TestValidateFuturesBreakoutConfirmation(t *testing.T) {
	cfg := Defaults()
	cfg.FuturesORB.Enabled = true
	cfg.FuturesORB.Instruments = []FuturesInstrument{
		{Symbol: "MNQ", BreakoutConfirmation: "retest_range"}, // valid, live-wired
		{Symbol: "MES", BreakoutConfirmation: "close_9m"},     // typo → must error
	}
	var mnqErr, mesErr bool
	for _, e := range Validate(cfg) {
		if strings.Contains(e, "breakout_confirmation") && strings.Contains(e, "MNQ") {
			mnqErr = true
		}
		if strings.Contains(e, "breakout_confirmation") && strings.Contains(e, "MES") {
			mesErr = true
		}
	}
	if mnqErr {
		t.Error("MNQ retest_range wrongly rejected (it is live-wired)")
	}
	if !mesErr {
		t.Error("MES close_9m typo should be rejected")
	}
}

func TestValidateForexMaxNotionalUSD(t *testing.T) {
	cfg := Defaults()
	cfg.ForexORB.Enabled = true
	cfg.ForexORB.Instruments = []Instrument{
		{Symbol: "XAG/USD", MaxNotionalUSD: 95000}, // USD-quoted → valid
		{Symbol: "EUR/GBP", MaxNotionalUSD: 95000}, // GBP-quoted → entry×qty is GBP, must error
	}
	errs := Validate(cfg)
	found := false
	for _, e := range errs {
		if strings.Contains(e, "EUR/GBP") && strings.Contains(e, "max_notional_usd") {
			found = true
		}
		if strings.Contains(e, "XAG/USD") && strings.Contains(e, "max_notional_usd") {
			t.Errorf("USD-quoted pair wrongly rejected: %s", e)
		}
	}
	if !found {
		t.Errorf("expected a max_notional_usd error for the non-USD-quoted pair; got %v", errs)
	}
}

func TestValidateForexCloseFracThreshold(t *testing.T) {
	cfg := Defaults()
	cfg.ForexORB.Enabled = true
	cfg.ForexORB.Instruments = []Instrument{
		{Symbol: "XAU/USD", CloseFracThreshold: 0.6}, // valid
		{Symbol: "XAG/USD", CloseFracThreshold: 0},   // off → valid
		{Symbol: "EUR/USD", CloseFracThreshold: 60},  // 0.60 typo → both sides unsatisfiable, must error
	}
	errs := Validate(cfg)
	found := false
	for _, e := range errs {
		if strings.Contains(e, "EUR/USD") && strings.Contains(e, "close_frac_threshold") {
			found = true
		}
		if (strings.Contains(e, "XAU/USD") || strings.Contains(e, "XAG/USD")) && strings.Contains(e, "close_frac_threshold") {
			t.Errorf("valid close_frac_threshold wrongly rejected: %s", e)
		}
	}
	if !found {
		t.Errorf("expected a close_frac_threshold error for the out-of-range value; got %v", errs)
	}

	// A threshold of exactly 1 is also unsatisfiable and must error.
	cfg.ForexORB.Instruments = []Instrument{{Symbol: "XAU/USD", CloseFracThreshold: 1.0}}
	if !containsStr(Validate(cfg), "close_frac_threshold") {
		t.Error("expected close_frac_threshold=1.0 to be rejected (>= 1 is unsatisfiable)")
	}

	// A negative threshold is meaningless and must error.
	cfg.ForexORB.Instruments = []Instrument{{Symbol: "XAU/USD", CloseFracThreshold: -0.1}}
	if !containsStr(Validate(cfg), "close_frac_threshold") {
		t.Error("expected negative close_frac_threshold to be rejected")
	}
}

func TestFuturesInstrument_ToInstrument(t *testing.T) {
	fi := FuturesInstrument{
		Symbol: "MNQ", TickSize: 0.25, TickValue: 0.50, TickBuffer: 2, MarginRate: 0.10,
		Strategy: "opening_shock", RangeMinutes: 1, SideMode: "long",
		MaxHoldMinutes: 240, BreakoutConfirmation: "retest_range", RetestTolerance: 0.25,
		RetestTimeoutMinutes: 30, DirectionEligibility: true, CommissionPerSide: 0.85,
		MaxCommissionToRisk: 0.1, RiskMultiplier: 1.5, MaxNotionalUSD: 95000,
		OpeningShockWindowMinutes: 30, OpeningShockLookback: 40, OpeningShockPercentile: 70,
		OpeningShockStopMult: 1, OpeningShockEntryDelayMin: 5,
		MinRangeATR: 0.5, MaxRangeATR: 3.0, MaxSpreadToRisk: 0.08,
		BreakEvenR: 1.0, TrailATRMult: 1.5, TrailAfterR: 1.0,
		PartialTakeProfitR: 1.0, PartialFraction: 0.5, RunnerTakeProfitR: 2.0,
	}
	got := fi.ToInstrument()

	// Derived fields.
	if got.SecType != "FUT" || got.MinLotSize != 1 {
		t.Errorf("SecType/MinLotSize = %q/%d; want FUT/1", got.SecType, got.MinLotSize)
	}
	if got.PipBuffer != 0.5 { // tick_buffer 2 × tick_size 0.25
		t.Errorf("PipBuffer = %v; want 0.5", got.PipBuffer)
	}
	if got.PointValue != 2.0 { // tick_value 0.50 / tick_size 0.25
		t.Errorf("PointValue = %v; want 2.0", got.PointValue)
	}
	if got.TickSize != 0.25 {
		t.Errorf("TickSize = %v; want 0.25", got.TickSize)
	}
	// Strategy fields copied straight across.
	if got.BreakoutConfirmation != "retest_range" || got.DirectionEligibility != true ||
		got.Strategy != "opening_shock" || got.RangeMinutes != 1 || got.SideMode != "long" ||
		got.MaxHoldMinutes != 240 || got.CommissionPerSide != 0.85 || got.RiskMultiplier != 1.5 ||
		got.PartialFraction != 0.5 || got.RunnerTakeProfitR != 2.0 || got.MaxNotionalUSD != 95000 ||
		got.OpeningShockWindowMinutes != 30 || got.OpeningShockLookback != 40 ||
		got.OpeningShockPercentile != 70 || got.OpeningShockEntryDelayMin != 5 {
		t.Errorf("strategy fields not copied through: %+v", got)
	}

	// Zero tick size must not divide-by-zero the point value.
	z := FuturesInstrument{Symbol: "X", TickSize: 0}.ToInstrument()
	if z.PointValue != 0 {
		t.Errorf("zero tick_size: PointValue = %v; want 0", z.PointValue)
	}
}

func TestValidateFuturesStrategyFamilies(t *testing.T) {
	cfg := Defaults()
	cfg.FuturesORB.Enabled = true
	cfg.FuturesORB.RiskReward = 2
	cfg.FuturesORB.Instruments = []FuturesInstrument{
		{Symbol: "MES", TickSize: 0.25, TickValue: 1.25, TickBuffer: 2, Strategy: "trend_continuation", SideMode: "short", RangeMinutes: 30, ContinuationDelayMinutes: 15, ContinuationConfirmBars: 3, ContinuationDepthMult: 2},
		{Symbol: "MNQ", TickSize: 0.25, TickValue: 0.50, TickBuffer: 2, Strategy: "opening_shock", SideMode: "long", RangeMinutes: 1, OpeningShockWindowMinutes: 30, OpeningShockLookback: 40, OpeningShockPercentile: 70, OpeningShockStopMult: 1},
		{Symbol: "MYM", TickSize: 1.00, TickValue: 0.50, TickBuffer: 4, Strategy: "sweep_reclaim", RangeMinutes: 1, SweepReclaimLevelSet: "overnight", SweepReclaimDepthMult: 8, SweepReclaimEntryWindowMinutes: 240},
	}
	for _, e := range Validate(cfg) {
		if strings.Contains(e, "futures_orb instrument") {
			t.Fatalf("unexpected futures strategy validation error: %s", e)
		}
	}

	cfg.FuturesORB.Instruments = []FuturesInstrument{{Symbol: "MNQ", Strategy: "opening_shock", SideMode: "up"}}
	errs := Validate(cfg)
	if !containsStr(errs, "side_mode") {
		t.Fatalf("expected invalid side_mode error, got: %v", errs)
	}
}

func TestFuturesORBConfig_AsForexConfig(t *testing.T) {
	c := FuturesORBConfig{
		RangeMinutes: 15, RiskReward: 2.0, MaxRiskPct: 0.5, MaxPositionPct: 20,
		MaxHoldMinutes: 240, ATRPeriod: 14, ATRTimeframe: "1h", TrendTimeframe: "1h", TrendEMAPeriod: 20,
	}
	f := c.AsForexConfig()
	if f.RangeMinutes != 15 || f.RiskReward != 2.0 || f.MaxRiskPct != 0.5 || f.MaxPositionPct != 20 ||
		f.MaxHoldMinutes != 240 || f.ATRPeriod != 14 || f.ATRTimeframe != "1h" ||
		f.TrendTimeframe != "1h" || f.TrendEMAPeriod != 20 {
		t.Errorf("AsForexConfig mismapped: %+v", f)
	}
	if f.Session != "" { // session stays empty — scheduler branches it independently
		t.Errorf("Session = %q; want empty", f.Session)
	}
}
