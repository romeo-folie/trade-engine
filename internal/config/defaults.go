package config

// Defaults returns a Config pre-filled with sensible defaults.
// These are overridden by YAML, DB overrides, and CLI flags.
func Defaults() *Config {
	return &Config{
		Engine: EngineConfig{
			Mode:            "manual",
			TickIntervalSec: 3600, // 60 min default (cost-reduction Epic 1)
			MaxWatchlist:    50,
			DryRun:          false,
			InitialCapital:  100000,
			LogTimezone:     "UTC",
		},
		Models: ModelsConfig{
			Registry: ModelRegistry{
				"analyst_cheap": {
					ModelConfig: ModelConfig{
						Provider:    "openai",
						Model:       "gpt-4o",
						APIKeyEnv:   "OPENAI_API_KEY",
						MaxTokens:   2048,
						Temperature: 0.1,
						TimeoutSec:  60,
					},
				},
				"analyst_mid": {
					ModelConfig: ModelConfig{
						Provider:    "google",
						Model:       "gemini-3.1-pro-preview",
						APIKeyEnv:   "GEMINI_API_KEY",
						MaxTokens:   2048,
						Temperature: 0.1,
						TimeoutSec:  60,
					},
					Fallback: "gemini_flash",
				},
				"gemini_flash": {
					ModelConfig: ModelConfig{
						Provider:    "google",
						Model:       "gemini-2.5-flash",
						APIKeyEnv:   "GEMINI_API_KEY",
						MaxTokens:   2048,
						Temperature: 0.1,
						TimeoutSec:  60,
					},
				},
				"analyst_frontier": {
					ModelConfig: ModelConfig{
						Provider:    "anthropic",
						Model:       "claude-sonnet-4-6",
						APIKeyEnv:   "ANTHROPIC_API_KEY",
						MaxTokens:   2048,
						Temperature: 0.1,
						TimeoutSec:  60,
					},
				},
				"arbiter": {
					ModelConfig: ModelConfig{
						Provider:    "anthropic",
						Model:       "claude-sonnet-4-6",
						APIKeyEnv:   "ANTHROPIC_API_KEY",
						MaxTokens:   4096,
						Temperature: 0.1,
						TimeoutSec:  90,
					},
				},
				"classifier": {
					ModelConfig: ModelConfig{
						Provider:    "openai",
						Model:       "gpt-4o-mini",
						APIKeyEnv:   "OPENAI_API_KEY",
						MaxTokens:   256,
						Temperature: 0.0,
						TimeoutSec:  15,
					},
				},
				"news_enricher": {
					ModelConfig: ModelConfig{
						Provider:    "openai",
						Model:       "gpt-4o-mini",
						APIKeyEnv:   "OPENAI_API_KEY",
						MaxTokens:   2048,
						Temperature: 0.1,
						TimeoutSec:  60,
					},
				},
				"embedder": {
					ModelConfig: ModelConfig{
						Provider:    "ollama",
						Model:       "nomic-embed-text",
						MaxTokens:   0,
						Temperature: 0,
						TimeoutSec:  30,
					},
				},
			},
			Roles: ModelRoles{
				Analysts:     []string{"analyst_cheap", "analyst_mid", "analyst_frontier"},
				Arbiter:      "arbiter",
				Classifier:   "classifier",
				NewsEnricher: "news_enricher",
				Embedder:     "embedder",
			},
			// Legacy fields kept for backward compatibility with existing configs.
			Primary:   ModelConfig{},
			Secondary: []ModelConfig{},
			// Consensus disabled by default: AnalyzeEnsemble requires >=2 working
			// analysts. Users with only one API key configured would get failures
			// on every tick. Enable explicitly once multiple analysts are set up.
			Consensus: ConsensusConfig{
				Enabled:            false,
				FastPathConfidence: 0.68,
				FastPathMaxSpread:  0.15,
				Strategy:           "majority",
			},
		},
		Markets: MarketsConfig{
			Stocks: StocksConfig{
				Enabled:   true,
				Paper:     true,
				APIKeyEnv: "ALPACA_API_KEY",
				SecretEnv: "ALPACA_API_SECRET",
				BaseURL:   "https://paper-api.alpaca.markets",
				DataURL:   "https://data.alpaca.markets",
			},
			IBKR: IBKRConfig{
				Enabled:              false,
				GatewayURL:           "https://localhost:5000",
				TickleSec:            240,
				InsecureTLS:          true,
				AuthMonitorSec:       60,
				AuthAlertDebounceMin: 30,
			},
			News: NewsAPIConfig{
				Provider:                     "finnhub",
				APIKeyEnv:                    "FINNHUB_API_KEY",
				FetchEveryMin:                15,
				ExtractionEnabled:            true,
				ExtractionProvider:           "openai",
				ExtractionModel:              "gpt-4o",
				ExtractionMaxArticlesPerTick: 10,
				ExtractionRPM:                20,
			},
		},
		Strategies: StrategiesConfig{
			TrendFollowing: TrendFollowingConfig{
				Enabled:   true,
				EMAPeriod: 20,
				ADXMin:    25,
				RSIMin:    45,
				RSIMax:    65,
			},
			MeanReversion: MeanReversionConfig{
				Enabled:       true,
				BBPeriod:      20,
				BBStdDev:      2.0,
				RSIOversold:   30,
				RSIOverbought: 70,
			},
			Momentum: MomentumConfig{
				Enabled:        true,
				MACDFast:       12,
				MACDSlow:       26,
				MACDSignal:     9,
				VolumeMultiple: 1.5,
			},
			NewsSentiment: NewsSentimentConfig{
				Enabled:      false,
				MinSentiment: 0.6,
				MaxAgeHours:  48,
			},
		},
		Risk: RiskConfig{
			ConfidenceThreshold: 0.55,
			// ConfidenceTiers defaults to nil so existing configs that omit
			// `risk.confidence_tiers` keep using the static
			// `confidence_threshold` floor (#317 review). Pre-filling tiers
			// here would silently overlay tiered gating onto operators who
			// explicitly raised confidence_threshold (same shape as the #312
			// MaxPerTicker default-overlay bug). Opt in via YAML;
			// config.example.yaml carries the recommended tier ladder.
			ConfidenceTiers:      nil,
			MinRiskReward:        1.2,
			MaxPositionPct:       5.0,
			MaxOpenPositions:     20,
			MaxPerTicker:         0,
			MaxDollarRiskPct:     1.0,
			PyramidEnabled:       false,
			PyramidMinGainPct:    5.0,
			DailyLossLimitPct:    3.0,
			MaxDrawdownPct:       15.0,
			CooldownAfterLoss:    60,
			DefaultStopLossPct:   2.0,
			DefaultTakeProfitPct: 4.0,
			SignalExpiryMin:      120,
			StalePriceMovePct:    2.0,
		},
		Notifications: NotificationsConfig{
			Telegram: TelegramConfig{
				Enabled:               false,
				BotTokenEnv:           "TELEGRAM_BOT_TOKEN",
				ChatID:                "",
				NotifyOn:              []string{"signal", "trade", "error", "daily_summary"},
				BatchEnabled:          true,
				BatchWindowSec:        60,
				MaxItemsPerBatch:      25,
				IncludeHolds:          false,
				MinConfidenceForAlert: 0.65,
			},
		},
		Logging: LoggingConfig{
			Level:      "info",
			Format:     "text",
			MaxSizeMB:  50,
			MaxBackups: 7,
			MaxAgeDays: 30,
			Compress:   true,
		},
		Database: DatabaseConfig{
			URLEnv: "DATABASE_URL",
		},
		Reports: ReportsConfig{
			OutputDir:       "~/.tradeengine/reports",
			AutoOpenBrowser: true,
		},
		Backtest: BacktestConfig{
			InitialCash:  100_000,
			RiskFreeRate: 0.04,
		},
		Analysis: AnalysisConfig{
			MaterialChangeOnly:       true,
			HoldCooldownMin:          120,
			RSIDeltaThreshold:        3.0,
			EMAGapDeltaPct:           0.5,
			PriceMoveThresholdPct:    0.75,
			NewsNoveltyEnabled:       true,
			MaxNewsArticlesPerPrompt: 5,
			MaxCandlesPerPrompt:      20, // must be >= largest HistoryWindowByStrategy value
			HistoryWindowByStrategy: map[string]int{
				"trend_following": 20,
				"mean_reversion":  20,
				"momentum":        14,
			},
			Classifier: ClassifierConfig{
				Mode:                  "shadow",
				RSIDeltaThreshold:     3.0,
				EMAGapDeltaPct:        0.5,
				PriceMoveThresholdPct: 0.75,
				HoldCooldownMin:       120,
				NewsNoveltyEnabled:    true,
				LLMFallbackEnabled:    false,
				LLMFallbackModel:      "classifier",
			},
			AdaptiveDepth: AdaptiveDepthConfig{
				Enabled:       false, // opt-in; requires analyst tier roles configured
				LowThreshold:  0.30,
				HighThreshold: 0.65,
				Weights: AdaptiveDepthWeights{
					Volatility:    0.30,
					NewsNovelty:   0.25,
					RiskProximity: 0.25,
					TrendStrength: 0.20,
				},
			},
			// Multi-timeframe borderline arbiter (Issue #321) — defaults are
			// deliberately narrow so only a small fraction of WAITs incur the
			// 2× token cost. 1d → 1h gives the daily view a near-term sanity
			// check; 1h ↔ 4h cross-checks intra-day vs swing perspectives.
			MultiTimeframeBorderlineEnabled: true,
			MultiTimeframeAltMap: map[string]string{
				"1h": "4h",
				"4h": "1h",
				"1d": "1h",
			},
			MultiTimeframeBorderlineConfMin: 0.55,
			MultiTimeframeBorderlineConfMax: 0.70,
		},
		Embeddings: EmbeddingsConfig{
			Enabled:                  false,
			Provider:                 "openai",
			Dimension:                1536,
			APIKeyEnv:                "OPENAI_API_KEY",
			OllamaBaseURL:            "http://localhost:11434",
			Model:                    "", // uses provider default
			WorkerIntervalSec:        30,
			BatchSize:                50,
			SimilarAnalysesEnabled:   false,
			SimilarAnalysesThreshold: 0.80,
			SimilarAnalysesLimit:     3,
		},
		Cost: CostConfig{
			Profile:                "balanced",
			DailyTokenBudget:       200000,
			WeeklyTokenBudget:      1200000,
			AutoDegradeOnBudgetHit: true,
			// CacheTTLMin is set by ApplyProfile (called before yaml.Unmarshal
			// in Load) so first-run saves write the correct profile-driven value.
			CacheTTLMin: 0,
		},
		ORB: ORBConfig{
			Mode:               "paper",
			GapThresholdPct:    2.5,
			MaxCandidates:      2,
			RangeMinutes:       15,
			RiskReward:         2.0,
			MaxRiskPct:         1.0,
			MaxPositionPct:     10.0, // hard cap: notional <= 10% of portfolio per trade
			NewsLookbackHours:  24,
			ScanCadenceMinutes: 2,
		},
		ForexORB: ForexORBConfig{
			Enabled: false,
			Instruments: []Instrument{
				{Symbol: "EUR/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, MaxHoldMinutes: 240},
				{Symbol: "GBP/USD", SecType: "CASH", MinLotSize: 5000, PipBuffer: 0.0002, MaxHoldMinutes: 180},
			},
			Session:        "london",
			Timezone:       "UTC",
			RangeMinutes:   15,
			RiskReward:     2.0,
			MaxRiskPct:     1.0,
			MaxPositionPct: 10.0,
			MaxHoldMinutes: 240, // strategy-level fallback: no ORB trade holds past 4h (prevents overnight holds)
		},
		FuturesORB: FuturesORBConfig{
			Enabled:        false,
			SessionOpenET:  "09:30", // US equity index futures open with the equity market
			RangeMinutes:   15,
			RiskReward:     2.0,
			MaxRiskPct:     1.0,
			MaxPositionPct: 20.0, // one ES contract is ~$255k notional — 20% of $100k = $20k cap (< 1 contract)
			Instruments: []FuturesInstrument{
				// US equity index futures — quarterly expiry (Mar/Jun/Sep/Dec)
				{Symbol: "ES", Exchange: "CME", AssetClass: "equity_index", ExpiryCycle: "quarterly", TickSize: 0.25, TickValue: 12.50, TickBuffer: 2},
				{Symbol: "NQ", Exchange: "CME", AssetClass: "equity_index", ExpiryCycle: "quarterly", TickSize: 0.25, TickValue: 5.00, TickBuffer: 2},
				{Symbol: "YM", Exchange: "CBOT", AssetClass: "equity_index", ExpiryCycle: "quarterly", TickSize: 1.00, TickValue: 5.00, TickBuffer: 4},
				{Symbol: "RTY", Exchange: "CME", AssetClass: "equity_index", ExpiryCycle: "quarterly", TickSize: 0.10, TickValue: 5.00, TickBuffer: 4},
			},
		},
		FuturesORBMetals: FuturesORBConfig{
			Enabled:        false,
			SessionOpenET:  "08:20", // COMEX pit open (US/Eastern)
			RangeMinutes:   15,
			RiskReward:     2.0,
			MaxRiskPct:     1.0,
			MaxPositionPct: 30.0,
			Instruments: []FuturesInstrument{
				// Gold futures (COMEX) — monthly expiry; 100 troy oz per contract
				{Symbol: "GC", Exchange: "COMEX", AssetClass: "metal", ExpiryCycle: "monthly", TickSize: 0.10, TickValue: 10.00, TickBuffer: 3},
				// Silver futures (COMEX) — monthly expiry; 5000 troy oz per contract
				{Symbol: "SI", Exchange: "COMEX", AssetClass: "metal", ExpiryCycle: "monthly", TickSize: 0.005, TickValue: 25.00, TickBuffer: 5},
			},
		},
		Portfolio: PortfolioConfig{
			Poller: PollerConfig{
				Enabled:              true,
				IntervalSec:          60,
				SignificantChangePct: 0.05,
			},
			Cache: CacheConfig{
				AccountTTLSec:   3,
				PositionsTTLSec: 3,
				HistoryTTLSec:   30,
				OrdersTTLSec:    10,
			},
		},
		Scanner: ScannerConfig{
			Enabled:           false,
			ScanIntervalHours: 2,
			CandidateCap:      10,
			TTLDays:           3,
			MinConviction:     0.65,
			NewsLookbackHours: 24,
			TopMovers:         20,
			TopActives:        20,
			GraphNeighbors: GraphNeighborsConfig{
				Enabled:          false,
				MaxPerScan:       5,
				MinConfidence:    0.80,
				MinCorroboration: 2,
				MinEdgeStrength:  0.6,
			},
		},
		Feedback: FeedbackConfig{
			Enabled:             false,
			ReviewTimeUTC:       "18:00",
			Windows:             []int{1, 7, 30},
			PromptEnrichment:    true,
			ORBPromptEnrichment: true,
			Thresholds: FeedbackThresholds{
				ModelWinRateMin:       0.40,
				ModelMinTrades:        20,
				InstrumentWinRateMin:  0.35,
				InstrumentMinTrades:   10,
				ArbiterInvokedRateMax: 0.50,
			},
		},
	}
}
