package engine

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/execution"
)

func TestPredictionRunner_DisabledWhenGlobalToggleOff(t *testing.T) {
	cfg := config.Defaults()
	cfg.Markets.Predictions.Enabled = false
	cfg.Markets.Predictions.Platforms = map[string]config.PredictionPlatform{
		"polymarket": {Enabled: true},
	}

	r := NewPredictionRunner(cfg, events.New(), "paper")
	if r.Enabled() {
		t.Fatal("expected prediction runner to be disabled when markets.predictions.enabled=false")
	}
}

func TestPredictionRunner_EnabledWhenGlobalToggleOnAndPlatformEnabled(t *testing.T) {
	cfg := config.Defaults()
	cfg.Markets.Predictions.Enabled = true
	cfg.Markets.Predictions.Platforms = map[string]config.PredictionPlatform{
		"polymarket": {Enabled: true},
	}

	r := NewPredictionRunner(cfg, events.New(), "paper")
	if !r.Enabled() {
		t.Fatal("expected prediction runner to be enabled when global toggle is on and platform enabled")
	}
}

func TestPredictionRunner_NoEnabledPlatforms(t *testing.T) {
	cfg := config.Defaults()
	cfg.Markets.Predictions.Enabled = true
	cfg.Markets.Predictions.Platforms = map[string]config.PredictionPlatform{
		"polymarket": {Enabled: false},
		"kalshi":     {Enabled: false},
	}

	r := NewPredictionRunner(cfg, events.New(), "paper")
	if r.Enabled() {
		t.Fatal("expected prediction runner to be disabled when no platform is enabled")
	}
}

func TestPredictionRunner_PolymarketUsesPaperExecutor(t *testing.T) {
	cfg := config.Defaults()
	cfg.Markets.Predictions.Enabled = true
	cfg.Markets.Predictions.Platforms = map[string]config.PredictionPlatform{
		"polymarket": {Enabled: true},
	}

	r := NewPredictionRunner(cfg, events.New(), "auto")

	exec, ok := r.executors["polymarket"]
	if !ok {
		t.Fatal("expected polymarket executor to be configured")
	}
	if _, ok := exec.(*execution.PredictionPaperExecutor); !ok {
		t.Fatalf("expected polymarket to use PredictionPaperExecutor, got %T", exec)
	}
}

func TestPredictionRunner_KalshiExecutorSelection(t *testing.T) {
	t.Run("paper mode always uses paper executor", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Markets.Predictions.Enabled = true
		cfg.Markets.Predictions.Platforms = map[string]config.PredictionPlatform{
			"kalshi": {
				Enabled: true,
				APIKey:  "live-key",
			},
		}

		r := NewPredictionRunner(cfg, events.New(), "paper")
		exec, ok := r.executors["kalshi"]
		if !ok {
			t.Fatal("expected kalshi executor to be configured")
		}
		if _, ok := exec.(*execution.PredictionPaperExecutor); !ok {
			t.Fatalf("expected paper mode to use PredictionPaperExecutor, got %T", exec)
		}
	})

	t.Run("auto mode with API key uses KalshiExecutor", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Markets.Predictions.Enabled = true
		cfg.Markets.Predictions.Platforms = map[string]config.PredictionPlatform{
			"kalshi": {
				Enabled: true,
				APIKey:  "live-key",
			},
		}

		r := NewPredictionRunner(cfg, events.New(), "auto")
		exec, ok := r.executors["kalshi"]
		if !ok {
			t.Fatal("expected kalshi executor to be configured")
		}
		if _, ok := exec.(*execution.KalshiExecutor); !ok {
			t.Fatalf("expected auto mode with API key to use KalshiExecutor, got %T", exec)
		}
	})

	t.Run("auto mode without API key falls back to paper executor", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Markets.Predictions.Enabled = true
		cfg.Markets.Predictions.Platforms = map[string]config.PredictionPlatform{
			"kalshi": {
				Enabled: true,
				APIKey:  "",
			},
		}

		r := NewPredictionRunner(cfg, events.New(), "auto")
		exec, ok := r.executors["kalshi"]
		if !ok {
			t.Fatal("expected kalshi executor to be configured")
		}
		if _, ok := exec.(*execution.PredictionPaperExecutor); !ok {
			t.Fatalf("expected fallback to PredictionPaperExecutor, got %T", exec)
		}
	})
}

func TestPredictionRunner_RunReturnsImmediatelyWhenDisabled(t *testing.T) {
	cfg := config.Defaults()
	cfg.Markets.Predictions.Enabled = false
	r := NewPredictionRunner(cfg, events.New(), "paper")

	done := make(chan struct{})
	go func() {
		r.Run(context.Background())
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected Run to return immediately when disabled")
	}
}
