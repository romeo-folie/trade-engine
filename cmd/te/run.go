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

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/embeddings"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/execution"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/notify"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

var (
	runMode   string
	runDryRun bool
	runDetach bool
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Start the TradeEngine",
	Long: `Start the TradeEngine in the configured mode.

The engine polls the watchlist on each tick, runs LLM analysis, and
manages signals and trades according to the active mode.

Modes:
  manual  — signals are queued as pending; require approval
  auto    — signals that pass risk checks execute automatically
  paper   — paper trades on Alpaca paper API (default)`,
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

		// Graceful shutdown on SIGTERM / SIGINT.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Start prediction market runner (24/7, runs in parallel with stock engine).
		predRunner := engine.NewPredictionRunner(cfg, eng.Bus(), mode, eng.Portfolio())
		if predRunner.Enabled() {
			go predRunner.Run(ctx)
			logger.Info("prediction market runner started")
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
	if runDryRun {
		args = append(args, "--dry-run")
	}

	child := exec.Command(os.Args[0], args...)
	child.Stdout = f
	child.Stderr = f
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

func init() {
	runCmd.Flags().StringVar(&runMode, "mode", "", "Override engine mode (manual|auto|paper)")
	runCmd.Flags().BoolVar(&runDryRun, "dry-run", false, "Dry run — no orders placed")
	runCmd.Flags().BoolVar(&runDetach, "detach", false, "Run engine in background and return immediately")
}
