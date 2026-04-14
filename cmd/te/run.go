package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/embeddings"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/feedback"
	"github.com/romeo-folie/trade-engine/internal/portfolio"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/notify"
	"github.com/romeo-folie/trade-engine/internal/orb"
	"github.com/romeo-folie/trade-engine/internal/store"
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
  orb-forex       — Forex ORB mode via IBKR Client Portal (requires markets.ibkr.enabled=true)
  orb-futures     — Equity index futures ORB via IBKR (ES/NQ/YM/RTY; requires markets.ibkr.enabled=true)
  orb-futures-metals — Metals futures ORB via IBKR (GC/SI on COMEX; requires markets.ibkr.enabled=true)`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if runDetach {
			return startDetachedRun()
		}

		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if errs := config.Validate(cfg); len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "config error: %s\n", e)
			}
			return fmt.Errorf("invalid configuration (%d error(s)); fix config before starting", len(errs))
		}
		if err := logger.Init(cfg.Logging.Level, cfg.Logging.Format); err != nil {
			return err
		}

		// CLI --mode flag overrides config.
		mode := cfg.Engine.Mode
		if runMode != "" {
			mode = runMode
		}
		// CLI --session flag overrides forex_orb.session.
		if runSession != "" {
			cfg.ForexORB.Session = runSession
		}
		if runDryRun || cfg.Engine.DryRun {
			logger.Info("dry-run mode — no orders will be placed")
		}

		// Connect to DB and check for a DB-level mode override.
		if err := store.Connect(context.Background()); err != nil {
			logger.Warn("database unavailable — running without DB", "err", err.Error())
		} else {
			defer store.Close()
			if dbMode, ok, err := store.GetOverride(context.Background(), "engine.mode"); err == nil && ok {
				mode = dbMode
			}
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

		// Wire Alpaca broker if API key is set; otherwise PaperExecutor is used as fallback.
		var broker execution.Executor
		if cfg.Markets.Stocks.APIKey != "" {
			broker = execution.NewAlpacaBroker(cfg.Markets.Stocks)
		}
		eng := engine.New(cfg, marketProvider, newsProvider, broker, mode)

		// Tag portfolio snapshots with mode (live/paper) and broker name (Phase 18).
		portfolioMode := "paper"
		if !cfg.Markets.Stocks.Paper {
			portfolioMode = "live"
		}
		portfolioBroker := ""
		switch broker.(type) {
		case *execution.AlpacaBroker:
			portfolioBroker = "alpaca"
		case *execution.IBKRBroker:
			portfolioBroker = "ibkr"
		}
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
		if alpacaBroker, ok := broker.(*execution.AlpacaBroker); ok && alpacaBroker != nil {
			// One-time backfill runs in background; skips gracefully if DB unavailable.
			go portfolio.Backfill(ctx, alpacaBroker, portfolioMode, portfolioBroker)
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
				if mode == "orb-forex" && cfg.ForexORB.Session != "" {
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

			if runWeb {
				webSrv := web.NewServer(web.Config{
					Port:           runWebPort,
					Host:           "0.0.0.0",
					AllowedOrigins: []string{"*"},
					EngineConfig:   cfg,
				}, orbBus, nil)
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
				logger.Info("web observability server started (orb mode)", "port", runWebPort)
				fmt.Printf("🌐 Web UI: http://localhost:%s\n", runWebPort)
			}

			logger.Info("ORB scheduler starting")
			fmt.Println("ORB mode active. Waiting for pre-market scan window...")
			if err := orbScheduler.Run(ctx); err != nil && err != context.Canceled {
				return fmt.Errorf("orb scheduler: %w", err)
			}
			fmt.Println("TradeEngine (ORB) stopped.")
			return nil
		}

		// ORB-Forex mode: run the ORB scheduler with IBKR and forex-specific components.
		if mode == "orb-forex" {
			// 1. Validate IBKR config.
			if !cfg.Markets.IBKR.Enabled {
				return fmt.Errorf("orb-forex mode requires markets.ibkr.enabled=true")
			}
			if !cfg.ForexORB.Enabled {
				return fmt.Errorf("orb-forex mode requires forex_orb.enabled=true")
			}
			if cfg.Markets.IBKR.AccountID == "" {
				return fmt.Errorf("orb-forex mode requires markets.ibkr.account_id to be set")
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

			// 4. Create event bus and analyzer (same pattern as equities ORB).
			orbBus := events.New()
			analyzer := analysis.NewAnalyzer(cfg.Models, orbBus)

			// 5. Create notification adapter (same pattern as equities ORB).
			var orbNotifier orb.Notifier
			if tgBot != nil {
				orbNotifier = &orbTelegramNotifier{bot: tgBot}
			}

			// 6. Create portfolio valuer (same pattern as equities ORB).
			orbPortfolio := newDBPortfolioValuer(100_000)

			// 7. Build ORB config from ForexORBConfig for the scheduler.
			orbCfg := config.ORBConfig{
				Enabled:        cfg.ForexORB.Enabled,
				Mode:           "paper",
				MaxCandidates:  len(cfg.ForexORB.Instruments),
				RangeMinutes:   cfg.ForexORB.RangeMinutes,
				RiskReward:     cfg.ForexORB.RiskReward,
				MaxRiskPct:     cfg.ForexORB.MaxRiskPct,
				MaxPositionPct: cfg.ForexORB.MaxPositionPct,
			}

			// 8. Create ORB scheduler with IBKR components.
			orbScheduler := orb.NewORBScheduler(
				orbCfg, store.GetDB(), ibkrData, analyzer,
				ibkrBroker, orbPortfolio, orbNotifier,
			)

			// 9. Wire forex-specific config and scanner.
			forexScanner := &orb.ForexCandidateScanner{
				Instruments: cfg.ForexORB.Instruments,
				MarketData:  ibkrData,
				Cfg:         cfg.ForexORB,
			}
			orbScheduler.SetScanner(forexScanner)
			orbScheduler.SetForexConfig(&cfg.ForexORB)

			// 10. Wire optional news and bus (same as equities ORB).
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

			// 11. Start web server if requested.
			if runWeb {
				webSrv := web.NewServer(web.Config{
					Port:           runWebPort,
					Host:           "0.0.0.0",
					AllowedOrigins: []string{"*"},
					EngineConfig:   cfg,
				}, orbBus, nil)
				// Wire IBKR broker-direct portfolio view.
				pc := cfg.Portfolio.Cache
				bvCache := web.NewBrokerCache(
					ibkrBroker,
					time.Duration(pc.AccountTTLSec)*time.Second,
					time.Duration(pc.PositionsTTLSec)*time.Second,
					time.Duration(pc.HistoryTTLSec)*time.Second,
					time.Duration(pc.OrdersTTLSec)*time.Second,
				)
				webSrv.SetBrokerView(bvCache)
				if err := webSrv.Start(ctx); err != nil {
					return fmt.Errorf("web server: %w", err)
				}
				defer webSrv.Stop()
				logger.Info("web observability server started (orb-forex mode)", "port", runWebPort)
				fmt.Printf("🌐 Web UI: http://localhost:%s\n", runWebPort)
			}

			// 12. Run.
			logger.Info("ORB-Forex scheduler starting")
			fmt.Println("ORB-Forex mode active. Waiting for forex session open...")
			if err := orbScheduler.Run(ctx); err != nil && err != context.Canceled {
				return fmt.Errorf("orb-forex scheduler: %w", err)
			}
			fmt.Println("TradeEngine (ORB-Forex) stopped.")
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
			orbScheduler.SetFuturesConfig(futuresCfg)

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

			// 12. Start web server if requested.
			if runWeb {
				webSrv := web.NewServer(web.Config{
					Port:           runWebPort,
					Host:           "0.0.0.0",
					AllowedOrigins: []string{"*"},
					EngineConfig:   cfg,
				}, orbBus, nil)
				pc := cfg.Portfolio.Cache
				bvCache := web.NewBrokerCache(
					ibkrBroker,
					time.Duration(pc.AccountTTLSec)*time.Second,
					time.Duration(pc.PositionsTTLSec)*time.Second,
					time.Duration(pc.HistoryTTLSec)*time.Second,
					time.Duration(pc.OrdersTTLSec)*time.Second,
				)
				webSrv.SetBrokerView(bvCache)
				if err := webSrv.Start(ctx); err != nil {
					return fmt.Errorf("web server: %w", err)
				}
				defer webSrv.Stop()
				logger.Info("web observability server started", "mode", mode, "port", runWebPort)
				fmt.Printf("🌐 Web UI: http://localhost:%s\n", runWebPort)
			}

			// 13. Run.
			logger.Info("ORB-Futures scheduler starting", "mode", mode)
			fmt.Printf("%s mode active. Waiting for session open (%s ET)...\n", mode, futuresCfg.SessionOpenET)
			if err := orbScheduler.Run(ctx); err != nil && err != context.Canceled {
				return fmt.Errorf("%s scheduler: %w", mode, err)
			}
			fmt.Printf("TradeEngine (%s) stopped.\n", mode)
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

		if err := eng.Run(ctx); err != nil {
			return err
		}
		fmt.Println("TradeEngine stopped.")
		return nil
	},
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
			}
		}
		return nil
	},
}

var modeCmd = &cobra.Command{
	Use:   "mode <manual|auto|paper>",
	Short: "Switch engine mode at runtime (persists to DB)",
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
		fmt.Printf("Mode set to %q (DB override — takes effect on next engine tick).\n", mode)
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

// notifyIBKRAuthFailure sends an immediate Telegram alert when the IBKR gateway
// is not authenticated at startup. The send is bounded by a 10s timeout so a
// Telegram outage cannot stall the engine shutdown path.
func notifyIBKRAuthFailure(ctx context.Context, bot *notify.TelegramBot, err error, gatewayURL string) {
	if bot == nil {
		return
	}
	msg := fmt.Sprintf("🔐 IBKR auth failed on startup — engine did not start.\n%v\n\nRe-authenticate at %s and restart.", err, gatewayURL)
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
	runCmd.Flags().StringVar(&runMode, "mode", "", "Override engine mode (manual|auto|paper|orb|orb-forex)")
	runCmd.Flags().StringVar(&runSession, "session", "", "Override forex_orb.session (london|newyork)")
	runCmd.Flags().BoolVar(&runDryRun, "dry-run", false, "Dry run — no orders placed")
	runCmd.Flags().BoolVar(&runDetach, "detach", false, "Run engine in background and return immediately")
	runCmd.Flags().BoolVar(&runWeb, "web", false, "Start the web observability server in-process")
	runCmd.Flags().StringVar(&runWebPort, "web-port", "8080", "Port for the in-process web server")
}
