package engine

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
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
