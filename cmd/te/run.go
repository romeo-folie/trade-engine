package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/embeddings"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/execution/trailing"
	"github.com/romeo-folie/trade-engine/internal/feedback"
	"github.com/romeo-folie/trade-engine/internal/knowledge"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/notify"
	"github.com/romeo-folie/trade-engine/internal/orb"
	"github.com/romeo-folie/trade-engine/internal/portfolio"
	"github.com/romeo-folie/trade-engine/internal/scanner"
	"github.com/romeo-folie/trade-engine/internal/stockorders"
	"github.com/romeo-folie/trade-engine/internal/store"
	swinguniverse "github.com/romeo-folie/trade-engine/internal/universe"
	"github.com/romeo-folie/trade-engine/internal/web"
	"github.com/spf13/cobra"
)

var (
	runMode    string
	runSession string
	runDryRun  bool
	runDetach  bool
	runWeb     bool
	runWebPort string
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Start the TradeEngine",
	Long: `Start the TradeEngine in the configured mode.

The engine polls the watchlist on each tick, runs LLM analysis, and
manages signals and trades according to the active mode.

Modes:
  manual     — signals are queued as pending; require approval
  auto       — signals that pass risk checks execute automatically
  paper      — paper trades on Alpaca paper API (default)
  orb             — Opening Range Breakout mode (LLM-enhanced gap-and-go)
  forex           — Forex strategies via IBKR Client Portal (ORB + optional currency strength)
  orb-forex       — Backward-compatible alias for forex
  orb-futures     — Equity index futures ORB via IBKR (ES/NQ/YM/RTY; requires markets.ibkr.enabled=true)
  orb-futures-metals — Metals futures ORB via IBKR (GC/SI on COMEX; requires markets.ibkr.enabled=true)
  orb-newyork     — Combined equities + forex ORB for the NY session (Alpaca + IBKR; requires both configured)`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if runDetach {
			return startDetachedRun()
		}

		cfg, err := config.Load()
		if err != nil {
			return err
		}

		// Resolve runtime values before validation so the final configuration uses
		// the documented YAML → DB → CLI precedence.
		startupCtx := context.Background()
		var overrideLookup runtimeOverrideLookup
		var dbUnavailableErr error
		if err := store.Connect(startupCtx); err != nil {
			dbUnavailableErr = err
		} else {
			defer store.Close()
			overrideLookup = store.GetOverride
		}
		mode, err := resolveRunMode(startupCtx, cfg.Engine.Mode, runMode, overrideLookup)
		if err != nil {
			return err
		}
		cfg.Engine.Mode = mode
		if errs := config.Validate(cfg); len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "config error: %s\n", e)
			}
			return fmt.Errorf("invalid configuration (%d error(s)); fix config before starting", len(errs))
		}
		if err := config.ValidateStockBrokerConfig(cfg.Markets.Stocks); err != nil {
			return err
		}
		if err := config.ValidateStockWorkflowConfig(mode, cfg.Markets.Stocks); err != nil {
			return err
		}
		if err := logger.InitWithRotation(cfg.Logging.Level, cfg.Logging.Format, logger.RotationConfig{
			MaxSizeMB:  cfg.Logging.MaxSizeMB,
			MaxBackups: cfg.Logging.MaxBackups,
			MaxAgeDays: cfg.Logging.MaxAgeDays,
			Compress:   cfg.Logging.Compress,
		}); err != nil {
			return err
		}
		if dbUnavailableErr != nil {
			logger.Warn("database unavailable — running without DB", "err", dbUnavailableErr.Error())
		}

		// CLI --session flag overrides forex_orb.session.
		if runSession != "" {
			cfg.ForexORB.Session = runSession
		}
		if runDryRun || cfg.Engine.DryRun {
			logger.Info("dry-run mode — no orders will be placed")
		}

		// Write PID file.
		if err := writePID(); err != nil {
			logger.Warn("could not write PID file", "err", err.Error())
		}
		defer removePID()

		logger.Info("TradeEngine starting",
			"version", "0.1.0",
			"mode", mode,
			"tick_interval_sec", cfg.Engine.TickIntervalSec,
			"pid", os.Getpid(),
		)
		fmt.Printf("TradeEngine started in %s mode. Press Ctrl+C to stop.\n", mode)

		// Build market data and news providers from config.
		var marketProvider data.MarketDataProvider
		if cfg.Markets.Stocks.Enabled && cfg.Markets.Stocks.APIKey != "" {
			marketProvider = data.NewAlpacaClient(
				cfg.Markets.Stocks.APIKey,
				cfg.Markets.Stocks.Secret,
				cfg.Markets.Stocks.DataURL,
			)
		} else {
			logger.Warn("Alpaca API key not set — market data fetching disabled")
		}

		var newsProvider data.NewsProvider
		if cfg.Markets.News.APIKey != "" {
			newsProvider = data.NewFinnhubClient(cfg.Markets.News.APIKey)
		} else {
			logger.Warn("news API key not set — news fetching disabled")
		}

		// Wire Alpaca broker only when stock execution is enabled; otherwise the
		// engine's local PaperExecutor fallback remains in use.
		var broker execution.Executor
		if cfg.Markets.Stocks.Enabled && cfg.Markets.Stocks.APIKey != "" {
			broker = execution.NewAlpacaBroker(cfg.Markets.Stocks)
		}
		// Standard stock mutations run through the durable coordinator. Keep the
		// dry-run wrapper outermost so it cannot create an intent. ORB/options
		// have their independent lifecycle and are intentionally excluded.
		if broker != nil && isStandardStockMode(mode) {
			broker = stockorders.New(broker)
		}
		broker = protectDryRunStockExecutor(broker, runDryRun || cfg.Engine.DryRun)
		eng := engine.New(cfg, marketProvider, newsProvider, broker, mode)
		if coordinator, ok := broker.(*stockorders.Coordinator); ok {
			coordinator.SetRecoveryObserver(eng)
		}

		// Tag portfolio snapshots with mode (live/paper) and broker name (Phase 18).
		portfolioMode := "paper"
		if !cfg.Markets.Stocks.Paper {
			portfolioMode = "live"
		}
		portfolioBroker := brokerName(broker)
		eng.Portfolio().SetTags(portfolioMode, portfolioBroker)

		// Graceful shutdown on SIGTERM / SIGINT.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Start independent portfolio poller for any broker that implements BrokerView.
		// Backfill is Alpaca-only (IBKR has no history endpoint).
		if bv, ok := broker.(execution.BrokerView); ok && bv != nil {
			if cfg.Portfolio.Poller.Enabled {
				interval := time.Duration(cfg.Portfolio.Poller.IntervalSec) * time.Second
				poller := portfolio.NewPoller(
					bv, interval,
					cfg.Portfolio.Poller.SignificantChangePct,
					portfolioMode, portfolioBroker,
				)
				go poller.Run(ctx)
			}
		}
		if alpacaBroker, ok := alpacaBrokerView(broker); ok {
			// One-time backfill runs in background; skips gracefully if DB unavailable.
			go portfolio.Backfill(ctx, alpacaBroker, portfolioMode, portfolioBroker)
		}

		// Company-name backfill: ensure every watchlist/analysis ticker has a
		// knowledge-graph entity so the UI shows the organization's full name
		// instead of the model-string fallback. One-shot per boot; the Finnhub
		// client's throttle paces the profile calls.
		if fh, ok := newsProvider.(*data.FinnhubClient); ok && fh != nil {
			go func() {
				if _, err := knowledge.BackfillCompanyNames(ctx, fh); err != nil && ctx.Err() == nil {
					logger.Warn("company-name backfill failed", "err", err.Error())
				}
			}()
		}

		// Server-side trailing stop manager (#322). Opt-in; disabled by
		// default so existing operators keep current behaviour. Walks the
		// stop_loss up using `new_stop = max(current_stop, hwm − atr_multiple × ATR)`.
		//
		// Requires a live DB connection — the manager reads open trades and
		// persists HWM/stop updates. If store.Connect() failed earlier we
		// skip startup so the LiveStore adapter never panics in production.
		var trailingMgr *trailing.Manager
		if cfg.Risk.TrailingStop.Enabled && marketProvider != nil && broker != nil {
			if !store.IsConnected() {
				logger.Warn("trailing-stop manager requested but DB not connected; skipping startup")
			} else {
				tsCfg := trailing.Config{
					Enabled:     true,
					Rule:        cfg.Risk.TrailingStop.Rule,
					ATRMultiple: cfg.Risk.TrailingStop.ATRMultiple,
					Interval:    time.Duration(cfg.Risk.TrailingStop.IntervalSec) * time.Second,
					ATRPeriod:   cfg.Risk.TrailingStop.ATRPeriod,

					ActivateAtGainPct: cfg.Risk.TrailingStop.ActivateAtGainPct,
					MarketOpen:        engine.IsMarketOpen,
					RecordLoss:        eng.Portfolio().RecordLoss,
					OnTradeClosed: func(payload events.TradeClosedPayload) {
						eng.Bus().Publish(events.TradeClosed, payload)
					},
				}
				trailingMgr = trailing.NewManager(tsCfg, trailing.LiveStore(), marketProvider, broker)
			}
		}

		// Start background embedding worker if enabled.
		if cfg.Embeddings.Enabled {
			if worker, err := embeddings.NewWorker(cfg.Embeddings); err != nil {
				logger.Warn("embedding worker disabled", "err", err.Error())
			} else {
				go worker.Run(ctx)
				logger.Info("embedding worker started")
			}
		}

		// Start Telegram notifications if enabled.
		var tgBot *notify.TelegramBot
		if cfg.Notifications.Telegram.Enabled && cfg.Notifications.Telegram.BotToken != "" {
			tgBot = notify.NewTelegramBot(cfg.Notifications.Telegram, eng.Bus())
			tgBot.SetCallbackHandler(notify.NewEngineCallbackHandler(eng))
			if err := tgBot.Start(ctx); err != nil {
				logger.Warn("telegram bot failed to start", "error", err)
			} else {
				defer tgBot.Stop()
				// Register batch flusher so the engine calls FlushBatch() at tick end.
				eng.SetBatchFlusher(tgBot)
				// Wire TelegramBot as the cost guardrail's degrade notifier (#178).
				// Auto-degrade alerts bypass the batch queue via SendImmediate.
				eng.SetCostNotifier(tgBot)
				logger.Info("telegram notifications enabled", "batch_enabled", cfg.Notifications.Telegram.BatchEnabled)

				startMsg := fmt.Sprintf("🟢 *TradeEngine started*\nMode: `%s`", mode)
				if (mode == "forex" || mode == "orb-forex") && cfg.ForexORB.Session != "" {
					startMsg += fmt.Sprintf(" · Session: `%s`", cfg.ForexORB.Session)
				}
				startMsg += fmt.Sprintf("\nTick: `%ds`", cfg.Engine.TickIntervalSec)
				if err := tgBot.SendImmediate(ctx, startMsg); err != nil {
					logger.Warn("telegram: failed to send startup notification", "err", err)
				}
			}
		}

		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

		go func() {
			sig := <-sigs
			logger.Info("received signal, shutting down", "signal", sig.String())
			fmt.Println("\nTradeEngine stopping...")
			cancel()
		}()
		specializedMode := mode == "orb" || mode == "forex" || mode == "orb-forex" ||
			mode == "orb-futures" || mode == "orb-futures-metals" || mode == "orb-newyork"
		trailingStarted := false
		if trailingMgr != nil && specializedMode {
			// Specialized schedulers return before the normal-engine startup
			// reconciliation gate and own their separate reconciliation flows.
			eng.SetTrailingManagerAvailable(true)
			go trailingMgr.Run(ctx)
			trailingStarted = true
		}

		// ORB mode: run the ORB scheduler instead of the normal engine loop.
		if mode == "orb" {
			if !cfg.ORB.Enabled {
				return fmt.Errorf("ORB mode is disabled in config — set orb.enabled: true")
			}

			alpacaClient, ok := marketProvider.(*data.AlpacaClient)
			if !ok || alpacaClient == nil {
				return fmt.Errorf("ORB mode requires Alpaca market data provider")
			}

			orbBus := events.New()
			analyzer := analysis.NewAnalyzer(cfg.Models, orbBus)
			var orbNotifier orb.Notifier
			if tgBot != nil {
				orbNotifier = &orbTelegramNotifier{bot: tgBot}
			}

			orbPortfolio := newDBPortfolioValuer(100_000)
			orbScheduler := orb.NewORBScheduler(
				cfg.ORB, store.GetDB(), alpacaClient, analyzer,
				broker, orbPortfolio, orbNotifier,
			)
			if newsProvider != nil {
				orbScheduler.SetNewsProvider(newsProvider)
			}
			orbScheduler.SetBus(orbBus)
			if cfg.Feedback.Enabled {
				orbScheduler.SetFeedbackConfig(&cfg.Feedback)
				reviewer := feedback.NewReviewer(cfg.Feedback, orbBus, store.GetDB())
				go func() {
					if err := reviewer.Run(ctx); err != nil && err != context.Canceled {
						fmt.Printf("feedback reviewer: %v\n", err)
					}
				}()
			}
			// Options-ORB also runs in the standalone equities mode.
			wireOptionsORB(orbScheduler, cfg.OptionsORB, broker)

			logger.Info("ORB scheduler starting")
			fmt.Println("ORB mode active. Waiting for pre-market scan window...")
			if err := orbScheduler.Run(ctx); err != nil && err != context.Canceled {
				return fmt.Errorf("orb scheduler: %w", err)
			}
			fmt.Println("TradeEngine (ORB) stopped.")
			return nil
		}

		// Forex mode: run enabled forex strategies with shared IBKR data/session components.
		// "orb-forex" remains an alias so existing launch scripts keep working.
		if mode == "forex" || mode == "orb-forex" {
			// 1. Validate IBKR config.
			if !cfg.Markets.IBKR.Enabled {
				return fmt.Errorf("forex mode requires markets.ibkr.enabled=true")
			}
			if !cfg.ForexORB.Enabled && !cfg.CurrencyStrength.Enabled {
				return fmt.Errorf("forex mode requires at least one enabled strategy: forex_orb.enabled=true or currency_strength.enabled=true")
			}
			if cfg.Markets.IBKR.AccountID == "" {
				return fmt.Errorf("forex mode requires markets.ibkr.account_id to be set")
			}

			// 2. Create IBKR broker.
			ibkrBroker := execution.NewIBKRBroker(cfg.Markets.IBKR)
			if err := ibkrBroker.CheckAuth(ctx); err != nil {
				notifyIBKRAuthFailure(ctx, tgBot, err, cfg.Markets.IBKR.GatewayURL)
				return fmt.Errorf("IBKR gateway not authenticated: %w", err)
			}
			stopKeepalive := ibkrBroker.StartKeepalive(ctx, cfg.Markets.IBKR.TickleSec)
			defer stopKeepalive()

			stopAuthMonitor := ibkrBroker.StartAuthMonitor(
				ctx,
				cfg.Markets.IBKR.AuthMonitorSec,
				cfg.Markets.IBKR.AuthAlertDebounceMin,
				func(gatewayURL string) {
					if tgBot == nil {
						return
					}
					msg := fmt.Sprintf(
						"🔐 *IBKR session expired*\nGateway: `%s`\nPlease re-authenticate via the Client Portal.",
						gatewayURL,
					)
					if err := tgBot.SendImmediate(ctx, msg); err != nil {
						logger.Warn("ibkr auth monitor: failed to send alert", "err", err)
					}
				},
				func(gatewayURL string) {
					if tgBot == nil {
						return
					}
					msg := fmt.Sprintf("✅ *IBKR session restored*\nGateway: `%s`", gatewayURL)
					if err := tgBot.SendImmediate(ctx, msg); err != nil {
						logger.Warn("ibkr auth monitor: failed to send recovery alert", "err", err)
					}
				},
			)
			defer stopAuthMonitor()

			// 3. Create IBKR market data client.
			ibkrData := data.NewIBKRClient(cfg.Markets.IBKR.GatewayURL, cfg.Markets.IBKR.InsecureTLS)

			eg, egCtx := errgroup.WithContext(ctx)
			var active []string

			if cfg.ForexORB.Enabled {
				// Create event bus/analyzer/notification adapter for ORB.
				orbBus := events.New()
				analyzer := analysis.NewAnalyzer(cfg.Models, orbBus)
				var orbNotifier orb.Notifier
				if tgBot != nil {
					orbNotifier = &orbTelegramNotifier{bot: tgBot}
				}

				orbCfg := config.ORBConfig{
					Enabled:        cfg.ForexORB.Enabled,
					Mode:           "paper",
					MaxCandidates:  len(cfg.ForexORB.Instruments),
					RangeMinutes:   cfg.ForexORB.RangeMinutes,
					RiskReward:     cfg.ForexORB.RiskReward,
					MaxRiskPct:     cfg.ForexORB.MaxRiskPct,
					MaxPositionPct: cfg.ForexORB.MaxPositionPct,
				}
				orbScheduler := orb.NewORBScheduler(
					orbCfg, store.GetDB(), ibkrData, analyzer,
					ibkrBroker, newDBPortfolioValuer(100_000), orbNotifier,
				)
				forexScanner := &orb.ForexCandidateScanner{
					Instruments: cfg.ForexORB.Instruments,
					MarketData:  ibkrData,
					Cfg:         cfg.ForexORB,
				}
				orbScheduler.SetScanner(forexScanner)
				orbScheduler.SetForexConfig(&cfg.ForexORB)
				if newsProvider != nil {
					orbScheduler.SetNewsProvider(newsProvider)
				}
				orbScheduler.SetBus(orbBus)
				if cfg.Feedback.Enabled {
					orbScheduler.SetFeedbackConfig(&cfg.Feedback)
					reviewer := feedback.NewReviewer(cfg.Feedback, orbBus, store.GetDB())
					go func() {
						if err := reviewer.Run(egCtx); err != nil && err != context.Canceled {
							fmt.Printf("feedback reviewer: %v\n", err)
						}
					}()
				}

				active = append(active, "orb")
				eg.Go(func() error {
					logger.Info("forex: ORB scheduler starting")
					if err := orbScheduler.Run(egCtx); err != nil && err != context.Canceled {
						return fmt.Errorf("forex orb scheduler: %w", err)
					}
					return nil
				})
			}

			if cfg.CurrencyStrength.Enabled {
				if shouldRunCurrencyStrengthForForexSession(cfg.ForexORB.Session) {
					if shouldRunCurrencyStrengthPaperOrders(cfg.CurrencyStrength.Mode) {
						if err := validateCurrencyStrengthPaperConfig(cfg, runDryRun); err != nil {
							return err
						}
						runner := newCurrencyStrengthPaperRunner(
							cfg.CurrencyStrength, cfg.Markets.IBKR.AccountID, ibkrData, ibkrBroker,
						)
						if store.IsConnected() {
							runner.SetJournal(dbCurrencyStrengthJournal{
								accountID: normalizeCurrencyStrengthAccountID(cfg.Markets.IBKR.AccountID),
							})
						}
						if tgBot != nil {
							runner.SetNotifier(&orbTelegramNotifier{bot: tgBot})
						}
						if err := loadCurrencyStrengthRecovery(runner); err != nil {
							return err
						}
						active = append(active, "currency_strength")
						eg.Go(func() error {
							logger.Info("forex: currency-strength runner starting")
							if err := runner.Run(egCtx); err != nil && err != context.Canceled {
								return fmt.Errorf("currency-strength runner: %w", err)
							}
							return nil
						})
					} else {
						logger.Info("forex: currency-strength skipped in shared launcher", "mode", cfg.CurrencyStrength.Mode)
					}
				} else {
					logger.Info("forex: currency-strength skipped outside london session", "session", cfg.ForexORB.Session)
				}
			}

			if len(active) == 0 {
				return fmt.Errorf("forex mode has no strategies active for session %q", cfg.ForexORB.Session)
			}
			logger.Info("forex mode starting", "strategies", strings.Join(active, ","))
			fmt.Printf("Forex mode active (%s). Waiting for forex session work...\n", strings.Join(active, " + "))
			if err := eg.Wait(); err != nil && err != context.Canceled {
				return err
			}
			fmt.Println("TradeEngine (forex) stopped.")
			return nil
		}

		// ORB-Futures mode: equity index futures (ES/NQ/YM/RTY) or metals (GC/SI) via IBKR.
		if mode == "orb-futures" || mode == "orb-futures-metals" {
			isMetals := mode == "orb-futures-metals"

			// 1. Validate IBKR config.
			if !cfg.Markets.IBKR.Enabled {
				return fmt.Errorf("%s mode requires markets.ibkr.enabled=true", mode)
			}
			if cfg.Markets.IBKR.AccountID == "" {
				return fmt.Errorf("%s mode requires markets.ibkr.account_id to be set", mode)
			}

			futuresCfg := &cfg.FuturesORB
			if isMetals {
				futuresCfg = &cfg.FuturesORBMetals
			}
			if !futuresCfg.Enabled {
				return fmt.Errorf("%s mode requires %s.enabled=true in config", mode, map[bool]string{true: "futures_orb_metals", false: "futures_orb"}[isMetals])
			}

			// 2. Create IBKR broker and authenticate.
			ibkrBroker := execution.NewIBKRBroker(cfg.Markets.IBKR)
			if err := ibkrBroker.CheckAuth(ctx); err != nil {
				notifyIBKRAuthFailure(ctx, tgBot, err, cfg.Markets.IBKR.GatewayURL)
				return fmt.Errorf("IBKR gateway not authenticated: %w", err)
			}
			stopKeepalive := ibkrBroker.StartKeepalive(ctx, cfg.Markets.IBKR.TickleSec)
			defer stopKeepalive()

			stopAuthMonitor := ibkrBroker.StartAuthMonitor(
				ctx,
				cfg.Markets.IBKR.AuthMonitorSec,
				cfg.Markets.IBKR.AuthAlertDebounceMin,
				func(gatewayURL string) {
					if tgBot == nil {
						return
					}
					msg := fmt.Sprintf(
						"🔐 *IBKR session expired*\nGateway: `%s`\nPlease re-authenticate via the Client Portal.",
						gatewayURL,
					)
					if err := tgBot.SendImmediate(ctx, msg); err != nil {
						logger.Warn("ibkr auth monitor: failed to send alert", "err", err)
					}
				},
				func(gatewayURL string) {
					if tgBot == nil {
						return
					}
					msg := fmt.Sprintf("✅ *IBKR session restored*\nGateway: `%s`", gatewayURL)
					if err := tgBot.SendImmediate(ctx, msg); err != nil {
						logger.Warn("ibkr auth monitor: failed to send recovery alert", "err", err)
					}
				},
			)
			defer stopAuthMonitor()

			// 3. Warm conid cache — resolves front-month contracts at startup.
			// Instruments in the static FuturesConids map cost no network call;
			// monthly-expiry instruments (GC, SI) are resolved dynamically here.
			if err := ibkrBroker.WarmFuturesConidCache(ctx, futuresCfg.Instruments); err != nil {
				return fmt.Errorf("%s startup: %w", mode, err)
			}

			// 4. Create IBKR market data client.
			ibkrData := data.NewIBKRClient(cfg.Markets.IBKR.GatewayURL, cfg.Markets.IBKR.InsecureTLS)

			// 5. Create event bus and analyzer.
			orbBus := events.New()
			analyzer := analysis.NewAnalyzer(cfg.Models, orbBus)

			// 6. Create notification adapter.
			var orbNotifier orb.Notifier
			if tgBot != nil {
				orbNotifier = &orbTelegramNotifier{bot: tgBot}
			}

			// 7. Create portfolio valuer.
			orbPortfolio := newDBPortfolioValuer(cfg.Engine.InitialCapital)

			// 8. Build ORBConfig shim for the scheduler.
			orbCfg := config.ORBConfig{
				Enabled:        true,
				Mode:           "paper",
				MaxCandidates:  len(futuresCfg.Instruments),
				RangeMinutes:   futuresCfg.RangeMinutes,
				RiskReward:     futuresCfg.RiskReward,
				MaxRiskPct:     futuresCfg.MaxRiskPct,
				MaxPositionPct: futuresCfg.MaxPositionPct,
			}

			// 9. Create ORB scheduler with IBKR components.
			orbScheduler := orb.NewORBScheduler(
				orbCfg, store.GetDB(), ibkrData, analyzer,
				ibkrBroker, orbPortfolio, orbNotifier,
			)

			// 10. Wire futures-specific config and scanner.
			futuresScanner := &orb.FuturesCandidateScanner{
				Instruments: futuresCfg.Instruments,
				MarketData:  ibkrData,
				Cfg:         *futuresCfg,
			}
			orbScheduler.SetScanner(futuresScanner)
			owner := store.ORBOwnerFuturesIndex
			if isMetals {
				owner = store.ORBOwnerFuturesMetals
			}
			orbScheduler.SetFuturesConfigOwned(futuresCfg, owner)

			// 11. Wire optional news and bus.
			if newsProvider != nil {
				orbScheduler.SetNewsProvider(newsProvider)
			}
			orbScheduler.SetBus(orbBus)
			if cfg.Feedback.Enabled {
				orbScheduler.SetFeedbackConfig(&cfg.Feedback)
				reviewer := feedback.NewReviewer(cfg.Feedback, orbBus, store.GetDB())
				go func() {
					if err := reviewer.Run(ctx); err != nil && err != context.Canceled {
						fmt.Printf("feedback reviewer: %v\n", err)
					}
				}()
			}

			// 12. Run.
			logger.Info("ORB-Futures scheduler starting", "mode", mode)
			fmt.Printf("%s mode active. Waiting for session open (%s ET)...\n", mode, futuresCfg.SessionOpenET)
			if err := orbScheduler.Run(ctx); err != nil && err != context.Canceled {
				return fmt.Errorf("%s scheduler: %w", mode, err)
			}
			fmt.Printf("TradeEngine (%s) stopped.\n", mode)
			return nil
		}

		// ORB-NewYork mode: run equities ORB (Alpaca) and forex ORB (IBKR, newyork
		// session) concurrently. Both schedulers share only the DB connection.
		// Starts 30 min before NY open (13:30 UTC); both sessions open together.
		if mode == "orb-newyork" {
			// 1. Validate prerequisites.
			if !store.IsConnected() {
				return fmt.Errorf("orb-newyork mode requires a database connection")
			}
			alpacaClient, ok := marketProvider.(*data.AlpacaClient)
			if !ok || alpacaClient == nil {
				return fmt.Errorf("orb-newyork mode requires Alpaca market data provider")
			}
			if !cfg.Markets.IBKR.Enabled {
				return fmt.Errorf("orb-newyork mode requires markets.ibkr.enabled=true")
			}
			if !cfg.ForexORB.Enabled {
				return fmt.Errorf("orb-newyork mode requires forex_orb.enabled=true")
			}
			if cfg.Markets.IBKR.AccountID == "" {
				return fmt.Errorf("orb-newyork mode requires markets.ibkr.account_id to be set")
			}
			if !cfg.ORB.Enabled {
				return fmt.Errorf("orb-newyork mode requires orb.enabled=true")
			}

			// 2. Set up IBKR broker (shared by the forex scheduler).
			ibkrBroker := execution.NewIBKRBroker(cfg.Markets.IBKR)
			if err := ibkrBroker.CheckAuth(ctx); err != nil {
				notifyIBKRAuthFailure(ctx, tgBot, err, cfg.Markets.IBKR.GatewayURL)
				return fmt.Errorf("IBKR gateway not authenticated: %w", err)
			}
			stopKeepalive := ibkrBroker.StartKeepalive(ctx, cfg.Markets.IBKR.TickleSec)
			defer stopKeepalive()
			stopAuthMonitor := ibkrBroker.StartAuthMonitor(
				ctx,
				cfg.Markets.IBKR.AuthMonitorSec,
				cfg.Markets.IBKR.AuthAlertDebounceMin,
				func(gatewayURL string) {
					if tgBot == nil {
						return
					}
					if err := tgBot.SendImmediate(ctx, fmt.Sprintf(
						"🔐 *IBKR session expired*\nGateway: `%s`\nPlease re-authenticate via the Client Portal.",
						gatewayURL,
					)); err != nil {
						logger.Warn("ibkr auth monitor: failed to send alert", "err", err)
					}
				},
				func(gatewayURL string) {
					if tgBot == nil {
						return
					}
					if err := tgBot.SendImmediate(ctx, fmt.Sprintf(
						"✅ *IBKR session restored*\nGateway: `%s`", gatewayURL,
					)); err != nil {
						logger.Warn("ibkr auth monitor: failed to send recovery alert", "err", err)
					}
				},
			)
			defer stopAuthMonitor()

			ibkrData := data.NewIBKRClient(cfg.Markets.IBKR.GatewayURL, cfg.Markets.IBKR.InsecureTLS)

			// 3. Build equities ORB scheduler (Alpaca data + broker).
			equitiesBus := events.New()
			equitiesAnalyzer := analysis.NewAnalyzer(cfg.Models, equitiesBus)
			var equitiesNotifier orb.Notifier
			if tgBot != nil {
				equitiesNotifier = &orbTelegramNotifier{bot: tgBot}
			}
			equitiesScheduler := orb.NewORBScheduler(
				cfg.ORB, store.GetDB(), alpacaClient, equitiesAnalyzer,
				broker, newDBPortfolioValuer(100_000), equitiesNotifier,
			)
			if newsProvider != nil {
				equitiesScheduler.SetNewsProvider(newsProvider)
			}
			equitiesScheduler.SetBus(equitiesBus)
			// A single Reviewer covers both schedulers — it queries all trades
			// globally and is not scoped to a bus. Running two would duplicate
			// flag computation and event publishing.
			if cfg.Feedback.Enabled {
				equitiesScheduler.SetFeedbackConfig(&cfg.Feedback)
				reviewer := feedback.NewReviewer(cfg.Feedback, equitiesBus, store.GetDB())
				go func() {
					if err := reviewer.Run(ctx); err != nil && err != context.Canceled {
						fmt.Printf("feedback reviewer: %v\n", err)
					}
				}()
			}

			// Options-ORB (issue #418): when enabled and the broker is Alpaca, execute
			// the configured underlyings via long options on the equities/newyork
			// session. No-op otherwise — the equity bracket path is unchanged.
			wireOptionsORB(equitiesScheduler, cfg.OptionsORB, broker)

			// 4. Build forex ORB scheduler (IBKR data + broker, newyork session).
			cfg.ForexORB.Session = "newyork"
			forexBus := events.New()
			forexAnalyzer := analysis.NewAnalyzer(cfg.Models, forexBus)
			var forexNotifier orb.Notifier
			if tgBot != nil {
				forexNotifier = &orbTelegramNotifier{bot: tgBot}
			}
			forexORBCfg := config.ORBConfig{
				Enabled:        cfg.ForexORB.Enabled,
				Mode:           "paper",
				MaxCandidates:  len(cfg.ForexORB.Instruments),
				RangeMinutes:   cfg.ForexORB.RangeMinutes,
				RiskReward:     cfg.ForexORB.RiskReward,
				MaxRiskPct:     cfg.ForexORB.MaxRiskPct,
				MaxPositionPct: cfg.ForexORB.MaxPositionPct,
			}
			forexScheduler := orb.NewORBScheduler(
				forexORBCfg, store.GetDB(), ibkrData, forexAnalyzer,
				ibkrBroker, newDBPortfolioValuer(100_000), forexNotifier,
			)
			forexScanner := &orb.ForexCandidateScanner{
				Instruments: cfg.ForexORB.Instruments,
				MarketData:  ibkrData,
				Cfg:         cfg.ForexORB,
			}
			forexScheduler.SetScanner(forexScanner)
			forexScheduler.SetForexConfig(&cfg.ForexORB)
			if newsProvider != nil {
				forexScheduler.SetNewsProvider(newsProvider)
			}
			forexScheduler.SetBus(forexBus)
			if cfg.Feedback.Enabled {
				forexScheduler.SetFeedbackConfig(&cfg.Feedback)
			}

			// 5. Optionally build a futures ORB scheduler (micro index futures via
			// IBKR) alongside equities+forex. Index futures open at 09:30 ET — the
			// same NY open as this mode — so they belong in this process rather than a
			// colliding cron slot (te-switch runs one engine at a time). Opt-in via
			// futures_orb.enabled; reuses the IBKR broker/data already set up above. A
			// conid warm-up failure is NON-fatal: it disables futures for the session
			// but never takes down the equities/forex bot.
			var futuresScheduler *orb.ORBScheduler
			if cfg.FuturesORB.Enabled {
				if err := ibkrBroker.WarmFuturesConidCache(ctx, cfg.FuturesORB.Instruments); err != nil {
					logger.Warn("orb-newyork: futures conid warm-up failed — futures disabled this session", "err", err.Error())
				} else {
					futuresBus := events.New()
					futuresAnalyzer := analysis.NewAnalyzer(cfg.Models, futuresBus)
					var futuresNotifier orb.Notifier
					if tgBot != nil {
						futuresNotifier = &orbTelegramNotifier{bot: tgBot}
					}
					futuresORBCfg := config.ORBConfig{
						Enabled:        true,
						Mode:           "paper",
						MaxCandidates:  len(cfg.FuturesORB.Instruments),
						RangeMinutes:   cfg.FuturesORB.RangeMinutes,
						RiskReward:     cfg.FuturesORB.RiskReward,
						MaxRiskPct:     cfg.FuturesORB.MaxRiskPct,
						MaxPositionPct: cfg.FuturesORB.MaxPositionPct,
					}
					futuresScheduler = orb.NewORBScheduler(
						futuresORBCfg, store.GetDB(), ibkrData, futuresAnalyzer,
						// Match standalone orb-futures: size risk/margin caps off the
						// configured initial capital, not a hard-coded $100k, so combined
						// and standalone modes size futures identically.
						ibkrBroker, newDBPortfolioValuer(cfg.Engine.InitialCapital), futuresNotifier,
					)
					futuresScheduler.SetScanner(&orb.FuturesCandidateScanner{
						Instruments: cfg.FuturesORB.Instruments,
						MarketData:  ibkrData,
						Cfg:         cfg.FuturesORB,
					})
					futuresScheduler.SetFuturesConfigOwned(&cfg.FuturesORB, store.ORBOwnerFuturesIndex)
					if newsProvider != nil {
						futuresScheduler.SetNewsProvider(newsProvider)
					}
					futuresScheduler.SetBus(futuresBus)
					// No second feedback Reviewer: the equities scheduler already runs
					// one that queries all trades globally (see comment above).
					if cfg.Feedback.Enabled {
						futuresScheduler.SetFeedbackConfig(&cfg.Feedback)
					}
					logger.Info("orb-newyork: futures scheduler enabled", "instruments", len(cfg.FuturesORB.Instruments))
				}
			}

			// 6. Run the schedulers concurrently. A fatal error in any one cancels the
			// shared context, which unblocks the others via their ctx.Done() check.
			domains := "equities + forex"
			if futuresScheduler != nil {
				domains += " + futures"
			}
			logger.Info("ORB-NewYork combined scheduler starting", "domains", domains)
			fmt.Printf("ORB-NewYork mode active (%s). Waiting for pre-market scan window...\n", domains)
			eg, egCtx := errgroup.WithContext(ctx)
			eg.Go(func() error {
				if err := equitiesScheduler.Run(egCtx); err != nil && err != context.Canceled {
					return fmt.Errorf("equities orb: %w", err)
				}
				return nil
			})
			eg.Go(func() error {
				if err := forexScheduler.Run(egCtx); err != nil && err != context.Canceled {
					return fmt.Errorf("forex orb: %w", err)
				}
				return nil
			})
			if futuresScheduler != nil {
				eg.Go(func() error {
					if err := futuresScheduler.Run(egCtx); err != nil && err != context.Canceled {
						return fmt.Errorf("futures orb: %w", err)
					}
					return nil
				})
			}
			if err := eg.Wait(); err != nil {
				return err
			}
			fmt.Println("TradeEngine (ORB-NewYork) stopped.")
			return nil
		}

		if runWeb {
			webSrv := web.NewServer(web.Config{
				Port:           runWebPort,
				Host:           "0.0.0.0",
				AllowedOrigins: []string{"*"},
				EngineConfig:   cfg,
			}, eng.Bus(), eng.Portfolio())
			// Wire broker-direct portfolio view for any broker implementing BrokerView.
			if bv, ok := broker.(execution.BrokerView); ok && bv != nil {
				pc := cfg.Portfolio.Cache
				bvCache := web.NewBrokerCache(
					bv,
					time.Duration(pc.AccountTTLSec)*time.Second,
					time.Duration(pc.PositionsTTLSec)*time.Second,
					time.Duration(pc.HistoryTTLSec)*time.Second,
					time.Duration(pc.OrdersTTLSec)*time.Second,
				)
				webSrv.SetBrokerView(bvCache)
			}
			if err := webSrv.Start(ctx); err != nil {
				return fmt.Errorf("web server: %w", err)
			}
			defer webSrv.Stop()
			logger.Info("web observability server started", "port", runWebPort)
			fmt.Printf("🌐 Web UI: http://localhost:%s\n", runWebPort)
		}

		// Start autonomous market scanner if enabled (non-ORB modes only).
		if cfg.Scanner.Enabled {
			alpacaClient, ok := marketProvider.(*data.AlpacaClient)
			if !ok || alpacaClient == nil {
				logger.Warn("scanner disabled — requires Alpaca market data provider")
			} else {
				scanAnalyzer := analysis.NewAnalyzer(cfg.Models, eng.Bus())
				mscanner := &scanner.MarketScanner{
					AlpacaClient:                 alpacaClient,
					DailyBars:                    data.NewAlpacaUniverseClient(alpacaClient, cfg.Markets.Stocks.BaseURL),
					Analyzer:                     scanAnalyzer,
					News:                         newsProvider,
					Bus:                          eng.Bus(),
					Cfg:                          cfg.Scanner,
					PullbackUniverseEnabled:      cfg.Risk.EntryGates.PullbackStrengthEnabled,
					PullbackMinMomentumReturnPct: cfg.Risk.EntryGates.PullbackMinMomentumReturnPct,
				}
				if fh, ok := newsProvider.(*data.FinnhubClient); ok {
					mscanner.Fundamentals = fh
				}
				if tgBot != nil {
					mscanner.Notifier = tgBot
				}
				universeProvider := data.NewAlpacaUniverseClient(
					alpacaClient, cfg.Markets.Stocks.BaseURL,
				)
				var afterCloseJobs []swinguniverse.ApprovedUniverseJob
				if cfg.Scanner.WeeklyLeader.Enabled && store.IsConnected() {
					leaderRunner := &scanner.WeeklyLeaderRunner{
						Provider: universeProvider,
						Cfg:      cfg.Scanner.WeeklyLeader,
					}
					afterCloseJobs = append(afterCloseJobs, leaderRunner)
					logger.Info("weekly leader scanner registered with universe pipeline",
						"promote_candidates",
						cfg.Scanner.WeeklyLeader.PromoteCandidates,
					)
				} else if cfg.Scanner.WeeklyLeader.Enabled {
					logger.Warn("weekly leader scanner requested but DB not connected; skipping startup")
				}
				if cfg.Scanner.PotentIndustry.Enabled && store.IsConnected() {
					industryRunner := &scanner.PotentIndustryRunner{
						Provider: universeProvider,
						Cfg:      cfg.Scanner.PotentIndustry,
					}
					afterCloseJobs = append(afterCloseJobs, industryRunner)
					logger.Info("potent industry scanner registered with universe pipeline",
						"promote_candidates",
						cfg.Scanner.PotentIndustry.PromoteCandidates,
					)
				} else if cfg.Scanner.PotentIndustry.Enabled {
					logger.Warn("potent industry scanner requested but DB not connected; skipping startup")
				}
				if cfg.Scanner.Universe.Enabled && store.IsConnected() {
					fundamentals, err := universeFundamentalsProvider(cfg.Markets.News)
					if err != nil {
						return fmt.Errorf("configure swing universe pipeline: %w", err)
					}
					sessionRunner := &swinguniverse.SessionRunner{
						Config: cfg.Scanner.Universe,
						Builder: swinguniverse.Builder{
							Config: cfg.Scanner.Universe, Assets: universeProvider,
							Snapshots: universeProvider, Bars: universeProvider,
							Fundamentals: fundamentals,
						},
					}
					universeScheduler := &swinguniverse.SessionScheduler{
						Cfg: cfg.Scanner.Universe, Builder: sessionRunner,
						Jobs: afterCloseJobs,
					}
					go func() {
						if err := universeScheduler.Run(ctx); err != nil &&
							err != context.Canceled {
							logger.Warn("swing universe pipeline stopped with error",
								"err", err.Error())
						}
					}()
					logger.Info("swing universe pipeline started",
						"after_close_delay_minutes", cfg.Scanner.Universe.AfterCloseDelayMinutes,
						"min_market_data_coverage", cfg.Scanner.Universe.MinMarketDataCoverage,
						"dependent_scanners", len(afterCloseJobs))
				} else if cfg.Scanner.Universe.Enabled {
					logger.Warn("swing universe pipeline requested but DB not connected; skipping startup")
				}
				if cfg.Scanner.PremarketEpisodic.Enabled && store.IsConnected() {
					premarketRunner := &scanner.PremarketEpisodicRunner{
						Provider: data.NewAlpacaPremarketClient(alpacaClient),
						News:     newsProvider,
						Cfg:      cfg.Scanner.PremarketEpisodic,
					}
					premarketScheduler := &scanner.PremarketEpisodicScheduler{
						Cfg: cfg.Scanner.PremarketEpisodic,
						Job: premarketRunner,
					}
					go func() {
						if err := premarketScheduler.Run(ctx); err != nil &&
							err != context.Canceled {
							logger.Warn("premarket episodic scheduler stopped with error",
								"err", err.Error())
						}
					}()
					logger.Info("premarket episodic scanner started",
						"checkpoints", cfg.Scanner.PremarketEpisodic.Checkpoints,
						"hard_cutoff", cfg.Scanner.PremarketEpisodic.HardCutoff,
						"feed", cfg.Scanner.PremarketEpisodic.Feed,
						"promote_candidates",
						cfg.Scanner.PremarketEpisodic.PromoteCandidates,
					)
				} else if cfg.Scanner.PremarketEpisodic.Enabled {
					logger.Warn("premarket episodic scanner requested but DB not connected; skipping startup")
				}
				go func() {
					if err := mscanner.Run(ctx); err != nil && err != context.Canceled {
						logger.Warn("market scanner stopped with error", "err", err.Error())
					}
				}()
				logger.Info("market scanner started",
					"interval_hours", cfg.Scanner.ScanIntervalHours,
					"ttl_days", cfg.Scanner.TTLDays,
					"candidate_cap", cfg.Scanner.CandidateCap,
				)
			}
		}

		// Resolve broker-confirmed exits before comparing broker positions with
		// DB ownership. Otherwise a stale exiting row can produce a false boot
		// drift alert immediately before the exit sweep closes it.
		if recoverer := stockOrderRecoveryForStartup(store.IsConnected(), broker); recoverer != nil {
			if err := recoverer.Recover(ctx); err != nil {
				return fmt.Errorf("startup stock order-intent recovery: %w", err)
			}
			go recoverStockOrdersPeriodically(ctx, recoverer, time.Minute)
		}
		exitReconcileOK := eng.ReconcileExitsNow(ctx)
		if !exitReconcileOK {
			logger.Warn("startup exit reconciliation incomplete — blocking trading until broker exit truth is complete")
			exitReconcileOK = retryExitReconciliation(ctx, 5*time.Second, eng.ReconcileExitsNow)
			if !exitReconcileOK {
				return ctx.Err()
			}
		}
		if trailingMgr != nil && !trailingStarted {
			// Manager.Run evaluates exits immediately. Start it only after event
			// subscribers are registered and startup reconciliation has proved
			// broker exit truth complete.
			eng.SetTrailingManagerAvailable(true)
			go trailingMgr.Run(ctx)
		}

		// Startup broker-vs-DB reconciler (Issue #313). Emits WARN per ticker
		// where broker quantity diverges from summed DB open-trade quantity.
		// Never crashes boot — broker errors are logged and swallowed inside
		// the reconciler so a stale dev DB or paused broker can't block start.
		//
		// Drift no longer dies as a log line (NOK: 178 orphaned shares logged
		// on every boot while bleeding −29%): it is alerted via Telegram, and
		// with engine.adopt_broker_orphans the untracked positions are adopted
		// as recovery trade rows so stops and analyst exits manage them again.
		var brokerRecon execution.BrokerView
		if bv, ok := broker.(execution.BrokerView); ok && bv != nil {
			brokerRecon = bv
		} else {
			engine.ReconcileBrokerVsDBWithStoreStatus(ctx, nil, 0)
		}
		sendDriftAlert := func(alertCtx context.Context, items []engine.BrokerDrift, adopted []string) {
			if tgBot == nil || len(items) == 0 {
				return
			}
			msg := fmt.Sprintf("⚠️ *Broker/DB drift*: %d ticker(s) diverged", len(items))
			for i, d := range items {
				if i == 8 {
					msg += fmt.Sprintf("\n… and %d more (see engine.log)", len(items)-i)
					break
				}
				msg += fmt.Sprintf("\n`%s` broker=%.4f db=%.4f (%s)", d.Ticker, d.BrokerQty, d.DBQty, d.Source)
			}
			if len(adopted) > 0 {
				msg += fmt.Sprintf("\n✅ adopted as recovery trades: `%s`", strings.Join(adopted, ", "))
			} else if !cfg.Engine.AdoptBrokerOrphans {
				msg += "\n(adoption off — set engine.adopt_broker_orphans to recover orphans)"
			}
			if err := tgBot.SendImmediate(alertCtx, msg); err != nil {
				logger.Warn("telegram: failed to send drift alert", "err", err)
			}
		}
		if brokerRecon != nil {
			initializeBrokerReconciliation := func(reconcileCtx context.Context) ([]engine.BrokerDrift, bool) {
				drifts, reconcileOK := engine.ReconcileBrokerVsDBWithStoreStatus(reconcileCtx, brokerRecon, 0)
				if !reconcileOK {
					return nil, false
				}
				alertDrifts := engine.AlertableBrokerDrifts(drifts)
				if len(alertDrifts) > 0 {
					var adopted []string
					if cfg.Engine.AdoptBrokerOrphans {
						adopted = engine.AdoptBrokerOrphansWithStore(reconcileCtx, alertDrifts,
							cfg.Risk.DefaultStopLossPct, cfg.Risk.DefaultTakeProfitPct)
					}
					sendDriftAlert(reconcileCtx, alertDrifts, adopted)
				}
				engine.PersistReconciliation(reconcileCtx, drifts)
				return drifts, true
			}
			startBrokerReconciliationLoop := func(reconcileCtx context.Context, initial []engine.BrokerDrift) {
				engine.RunReconcileLoop(reconcileCtx, brokerRecon, engine.BrokerReconcileInterval, engine.ReconcileLoopOptions{
					InitialDrifts: initial,
					OnNewDrifts: func(alertCtx context.Context, fresh []engine.BrokerDrift) {
						sendDriftAlert(alertCtx, fresh, nil)
					},
				})
			}
			if initial, ok := initializeBrokerReconciliation(ctx); ok {
				go startBrokerReconciliationLoop(ctx, initial)
			} else {
				go retryBrokerReconciliation(ctx, 5*time.Second, initializeBrokerReconciliation, startBrokerReconciliationLoop)
			}
		}

		if err := eng.Run(ctx); err != nil {
			return err
		}
		fmt.Println("TradeEngine stopped.")
		return nil
	},
}

func retryBrokerReconciliation(
	ctx context.Context,
	interval time.Duration,
	reconcile func(context.Context) ([]engine.BrokerDrift, bool),
	onComplete func(context.Context, []engine.BrokerDrift),
) {
	if interval <= 0 || reconcile == nil || onComplete == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if drifts, ok := reconcile(ctx); ok {
				onComplete(ctx, drifts)
				return
			}
		}
	}
}

func retryExitReconciliation(
	ctx context.Context,
	interval time.Duration,
	reconcile func(context.Context) bool,
) bool {
	if interval <= 0 || reconcile == nil {
		return false
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if reconcile(ctx) {
				return true
			}
		}
	}
}

func startDetachedRun() error {
	logPath := logger.LogFilePath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0750); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer f.Close()

	args := []string{"run"}
	if runMode != "" {
		args = append(args, "--mode", runMode)
	}
	if runSession != "" {
		args = append(args, "--session", runSession)
	}
	if runDryRun {
		args = append(args, "--dry-run")
	}
	if runWeb {
		args = append(args, "--web", "--web-port", runWebPort)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open devnull: %w", err)
	}
	defer devNull.Close()

	child := exec.Command(os.Args[0], args...)
	child.Stdout = devNull // logger writes to log file directly; stdout redirect would duplicate every line
	child.Stderr = f       // capture panics and non-logger stderr in the log file
	child.Stdin = nil
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := child.Start(); err != nil {
		return fmt.Errorf("start detached engine: %w", err)
	}
	fmt.Printf("TradeEngine started in background (PID %d).\n", child.Process.Pid)
	fmt.Printf("Logs: %s\n", logPath)
	fmt.Println("Use 'te status' to check health and 'te stop' to stop it.")
	return nil
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running TradeEngine",
	RunE: func(cmd *cobra.Command, args []string) error {
		pid, err := readPID()
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Println("Engine not running.")
				return nil
			}
			return fmt.Errorf("read PID file: %w", err)
		}

		proc, err := os.FindProcess(pid)
		if err != nil {
			fmt.Printf("Process %d not found — removing stale PID file.\n", pid)
			removePID()
			return nil
		}

		fmt.Printf("Sending SIGTERM to engine (PID %d)...\n", pid)
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			fmt.Printf("Process %d not responding: %v\n", pid, err)
			removePID()
			return nil
		}

		// Wait up to 10 seconds for clean exit.
		for i := 0; i < 10; i++ {
			time.Sleep(time.Second)
			if err := proc.Signal(syscall.Signal(0)); err != nil {
				// Process is gone.
				fmt.Println("Engine stopped.")
				return nil
			}
		}
		fmt.Println("Engine did not stop within 10s. Use SIGKILL if needed.")
		return nil
	},
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show engine and database status",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}

		// Engine process.
		pid, err := readPID()
		if err == nil {
			proc, procErr := os.FindProcess(pid)
			if procErr == nil && proc.Signal(syscall.Signal(0)) == nil {
				fmt.Printf("Engine:    running (PID %d, mode: %s)\n", pid, cfg.Engine.Mode)
			} else {
				fmt.Println("Engine:    not running (stale PID file)")
			}
		} else {
			fmt.Println("Engine:    not running")
		}

		// Database.
		dbEnv := cfg.Database.URLEnv
		if dbEnv == "" {
			dbEnv = "DATABASE_URL"
		}
		if os.Getenv(dbEnv) == "" {
			fmt.Printf("Database:  ✗ %s not set\n", dbEnv)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := store.Connect(ctx); err != nil {
				fmt.Printf("Database:  ✗ unreachable (%v)\n", err)
			} else {
				defer store.Close()
				var count int
				err := store.GetDB().QueryRow(ctx,
					`SELECT COUNT(*) FROM watchlist WHERE active = true`,
				).Scan(&count)
				if err != nil {
					fmt.Println("Database:  ✓ connected (watchlist: schema not yet migrated)")
				} else {
					fmt.Printf("Database:  ✓ connected (watchlist: %d active items)\n", count)
				}
				// Active mode from DB overrides.
				if dbMode, ok, _ := store.GetOverride(ctx, "engine.mode"); ok {
					fmt.Printf("Mode:      %s (DB override)\n", dbMode)
				} else {
					fmt.Printf("Mode:      %s (config)\n", cfg.Engine.Mode)
				}
				if control, controlErr := store.GetBuyControl(ctx, store.BuyControlStocks); controlErr != nil {
					fmt.Printf("New buys:  ✗ status unavailable (%v)\n", controlErr)
				} else if control.Enabled {
					fmt.Println("New buys:  enabled")
				} else {
					fmt.Printf("New buys:  paused (by %s at %s)\n", control.UpdatedBy,
						control.UpdatedAt.UTC().Format(time.RFC3339))
				}
			}
		}
		return nil
	},
}

var modeCmd = &cobra.Command{
	Use:   "mode <manual|auto|paper>",
	Short: "Set the engine mode startup override (persists to DB)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		mode := args[0]
		switch mode {
		case "manual", "auto", "paper":
		default:
			return fmt.Errorf("invalid mode %q — must be manual, auto, or paper", mode)
		}

		ctx := context.Background()
		if err := store.Connect(ctx); err != nil {
			return fmt.Errorf("connect to database: %w", err)
		}
		defer store.Close()

		if err := store.SetOverride(ctx, "engine.mode", mode); err != nil {
			return fmt.Errorf("set mode override: %w", err)
		}
		fmt.Printf("Mode set to %q (DB override — restart the engine and dashboard to apply).\n", mode)
		return nil
	},
}

// ── PID helpers ───────────────────────────────────────────────────────────────

func pidFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".tradeengine", "engine.pid")
}

func writePID() error {
	if err := os.MkdirAll(filepath.Dir(pidFilePath()), 0750); err != nil {
		return err
	}
	return os.WriteFile(pidFilePath(), []byte(strconv.Itoa(os.Getpid())), 0640)
}

func readPID() (int, error) {
	data, err := os.ReadFile(pidFilePath())
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(data))
}

func removePID() {
	os.Remove(pidFilePath())
}

// dbPortfolioValuer reads the latest portfolio snapshot using an injected fetch
// function. Constructor injection keeps the package-level store import out of
// the hot path and makes the type easily testable.
type dbPortfolioValuer struct {
	defaultValue float64
	// fetchSnapshot is the function used to read the latest portfolio value.
	// Injected at construction time — defaults to store.GetLatestPortfolioSnapshot.
	fetchSnapshot func(ctx context.Context) (*store.PortfolioSnapshot, error)
}

// newDBPortfolioValuer creates a dbPortfolioValuer backed by the real DB store.
func newDBPortfolioValuer(defaultValue float64) *dbPortfolioValuer {
	return &dbPortfolioValuer{
		defaultValue:  defaultValue,
		fetchSnapshot: store.GetLatestPortfolioSnapshot,
	}
}

// wireOptionsORB enables options-ORB on an equities ORB scheduler when configured
// and the stocks broker is Alpaca. Called from EVERY equities entrypoint (--mode
// orb and orb-newyork) so no run mode silently ignores options_orb config and
// places stock brackets on configured underlyings.
func wireOptionsORB(scheduler *orb.ORBScheduler, optCfg config.OptionsORBConfig, broker execution.Executor) {
	if !optCfg.Enabled {
		return
	}
	optionData, ok := broker.(interface {
		GetOptionChain(context.Context, string, execution.OptionChainFilter) ([]execution.OptionSnapshot, error)
		GetOptionSnapshots(context.Context, []string) ([]execution.OptionSnapshot, error)
	})
	if ok {
		scheduler.SetupOptionsORB(optCfg, optionData, optionData, broker)
		logger.Info("options-ORB enabled",
			"underlyings", optCfg.Underlyings, "premium_budget", optCfg.PremiumBudget)
	} else {
		logger.Warn("options-ORB enabled but stocks broker is not Alpaca; skipping")
	}
}

func (p *dbPortfolioValuer) PortfolioValue() float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	snap, err := p.fetchSnapshot(ctx)
	if err != nil {
		// Warn loudly — position sizing will fall back to the default value.
		// In a live trading context this could lead to oversized positions.
		logger.Warn("orb: portfolio snapshot fetch failed — using default value for position sizing",
			"default", p.defaultValue, "err", err.Error())
		return p.defaultValue
	}
	if snap == nil {
		logger.Warn("orb: no portfolio snapshot found — using default value for position sizing",
			"default", p.defaultValue)
		return p.defaultValue
	}
	if snap.TotalValue <= 0 {
		logger.Warn("orb: portfolio snapshot has non-positive TotalValue — using default value for position sizing",
			"total_value", snap.TotalValue, "default", p.defaultValue)
		return p.defaultValue
	}
	return snap.TotalValue
}

// notifyIBKRAuthFailure sends a Telegram alert when the IBKR gateway is not
// authenticated at engine startup. A 10-second timeout context is used so that
// a slow or unreachable Telegram API cannot stall the startup failure path.
// Dynamic values are wrapped in backticks to avoid Markdown parse errors.
func notifyIBKRAuthFailure(ctx context.Context, bot *notify.TelegramBot, err error, gatewayURL string) {
	if bot == nil {
		return
	}
	msg := fmt.Sprintf(
		"🔐 IBKR auth failed on startup — engine did not start.\n`%v`\n\nRe-authenticate at `%s` and restart.",
		err, gatewayURL,
	)
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if sendErr := bot.SendImmediate(sendCtx, msg); sendErr != nil {
		logger.Warn("telegram: failed to send IBKR auth alert", "err", sendErr)
	}
}

// orbTelegramNotifier adapts notify.TelegramBot to the orb.Notifier interface.
type orbTelegramNotifier struct {
	bot *notify.TelegramBot
}

func (n *orbTelegramNotifier) Notify(ctx context.Context, msg string) error {
	return n.bot.SendImmediate(ctx, msg)
}

func init() {
	runCmd.Flags().StringVar(&runMode, "mode", "", "Override engine mode (manual|auto|paper|orb|forex|orb-forex|orb-futures|orb-futures-metals|orb-newyork)")
	runCmd.Flags().StringVar(&runSession, "session", "", "Override forex_orb.session (london|newyork)")
	runCmd.Flags().BoolVar(&runDryRun, "dry-run", false, "Dry run — no orders placed")
	runCmd.Flags().BoolVar(&runDetach, "detach", false, "Run engine in background and return immediately")
	runCmd.Flags().BoolVar(&runWeb, "web", false, "Start the web observability server in-process")
	runCmd.Flags().StringVar(&runWebPort, "web-port", "8080", "Port for the in-process web server")
}
