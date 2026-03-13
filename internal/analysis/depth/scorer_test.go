package depth_test

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/analysis/depth"
	"github.com/romeo-folie/trade-engine/internal/config"
)

func defaultCfg() config.AdaptiveDepthConfig {
	return config.AdaptiveDepthConfig{
		Enabled:       true,
		LowThreshold:  0.30,
		HighThreshold: 0.65,
		Weights: config.AdaptiveDepthWeights{
			Volatility:    0.30,
			NewsNovelty:   0.25,
			RiskProximity: 0.25,
			TrendStrength: 0.20,
		},
	}
}

func TestScoreDepth1BelowLow(t *testing.T) {
	cfg := defaultCfg()
	tc := depth.TickContext{ATRPct: 0, NewsChanged: false, RiskProximityPct: 0, ADX: 0}
	is := depth.Score(tc, cfg)
	if is.Score >= cfg.LowThreshold {
		t.Errorf("expected score < %.2f, got %.4f", cfg.LowThreshold, is.Score)
	}
	if is.Depth != 1 {
		t.Errorf("expected depth 1, got %d", is.Depth)
	}
}

func TestScoreDepth2BetweenThresholds(t *testing.T) {
	cfg := defaultCfg()
	// ATR 2% → volatility = 0.4; no other signals
	// score = 0.30 * 0.4 = 0.12 — still below low; add news changed
	// score = 0.12 + 0.25 = 0.37 → depth 2
	tc := depth.TickContext{ATRPct: 2.0, NewsChanged: true, RiskProximityPct: 0, ADX: 0}
	is := depth.Score(tc, cfg)
	if is.Depth != 2 {
		t.Errorf("expected depth 2, got %d (score=%.4f)", is.Depth, is.Score)
	}
	if is.Score < cfg.LowThreshold || is.Score >= cfg.HighThreshold {
		t.Errorf("score %.4f should be in [%.2f, %.2f)", is.Score, cfg.LowThreshold, cfg.HighThreshold)
	}
}

func TestScoreDepth3AboveHigh(t *testing.T) {
	cfg := defaultCfg()
	// Max all signals → score = 1.0
	tc := depth.TickContext{ATRPct: 10.0, NewsChanged: true, RiskProximityPct: 1.0, ADX: 100}
	is := depth.Score(tc, cfg)
	if is.Depth != 3 {
		t.Errorf("expected depth 3, got %d (score=%.4f)", is.Depth, is.Score)
	}
	if is.Score < cfg.HighThreshold {
		t.Errorf("score %.4f should be >= %.2f", is.Score, cfg.HighThreshold)
	}
}

func TestScoreNormalisationClamp(t *testing.T) {
	cfg := defaultCfg()
	// Extreme inputs should not produce score > 1.
	tc := depth.TickContext{ATRPct: 999, NewsChanged: true, RiskProximityPct: 999, ADX: 999}
	is := depth.Score(tc, cfg)
	if is.Score > 1.0 {
		t.Errorf("score %.4f exceeds 1.0", is.Score)
	}
}

func TestScoreInputsPopulated(t *testing.T) {
	cfg := defaultCfg()
	tc := depth.TickContext{ATRPct: 2.5, NewsChanged: false, RiskProximityPct: 0.5, ADX: 25}
	is := depth.Score(tc, cfg)
	for _, key := range []string{"volatility", "news_novelty", "risk_proximity", "trend_strength"} {
		if _, ok := is.Inputs[key]; !ok {
			t.Errorf("missing input key %q in ImpactScore.Inputs", key)
		}
	}
	if is.Inputs["news_novelty"] != 0.0 {
		t.Errorf("expected news_novelty=0 when unchanged, got %.4f", is.Inputs["news_novelty"])
	}
}

func TestScoreExactThresholdBoundaries(t *testing.T) {
	cfg := defaultCfg()
	// Construct a score that lands exactly on the low threshold (0.30).
	// volatility=1.0 (ATR=5%), no other signals: score = 0.30 * 1.0 = 0.30 → depth 2
	tc := depth.TickContext{ATRPct: 5.0}
	is := depth.Score(tc, cfg)
	if is.Depth != 2 {
		t.Errorf("score at low threshold should be depth 2, got %d (score=%.4f)", is.Depth, is.Score)
	}
}
