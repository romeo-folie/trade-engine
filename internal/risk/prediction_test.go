package risk

import (
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// defaultPredCfg returns a RiskConfig with all prediction checks enabled.
func defaultPredCfg() config.RiskConfig {
	return config.RiskConfig{
		ConfidenceThreshold: 0.65,
		MaxPositionPct:      5.0,
		MaxOpenPositions:    10,
		DailyLossLimitPct:   3.0,
		MaxDrawdownPct:      15.0,
		CooldownAfterLoss:   0, // disabled
		Prediction: config.PredictionRiskConfig{
			ConfidenceThreshold:      0.70,
			MaxSpread:                0.05,
			MinLiquidity:             1000,
			MinTimeToResolutionHours: 4,
		},
	}
}

func passingInput() PredictionCheckInput {
	return PredictionCheckInput{
		Cfg:                defaultPredCfg(),
		Action:             "BUY",
		Confidence:         0.80,
		SizePct:            2.0,
		PortfolioValue:     100_000,
		Cash:               50_000,
		OpenPositions:      3,
		DailyPnL:           0,
		PeakValue:          100_000,
		Spread:             0.02,
		Volume24h:          5000,
		ResolutionDeadline: time.Now().Add(48 * time.Hour),
	}
}

// ── Full gate (CheckAllPrediction) ────────────────────────────────────────────

func TestCheckAllPrediction_AllPass(t *testing.T) {
	_, ok := CheckAllPrediction(passingInput())
	if !ok {
		t.Error("expected all checks to pass")
	}
}

func TestCheckAllPrediction_BlockedByLowConfidence(t *testing.T) {
	in := passingInput()
	in.Confidence = 0.60 // below prediction threshold of 0.70
	results, ok := CheckAllPrediction(in)
	if ok {
		t.Fatal("expected block on low confidence")
	}
	if BlockReason(results) == "" {
		t.Error("expected non-empty block reason")
	}
}

func TestCheckAllPrediction_ConfidenceOverrideUsed(t *testing.T) {
	in := passingInput()
	in.Confidence = 0.68 // above stock threshold (0.65) but below prediction threshold (0.70)
	_, ok := CheckAllPrediction(in)
	if ok {
		t.Error("prediction-specific confidence threshold should block 0.68")
	}
}

func TestCheckAllPrediction_FallsBackToBaseConfidenceWhenZero(t *testing.T) {
	in := passingInput()
	in.Cfg.Prediction.ConfidenceThreshold = 0 // disabled — use base 0.65
	in.Confidence = 0.68                       // above 0.65, should pass
	_, ok := CheckAllPrediction(in)
	if !ok {
		t.Error("should fall back to base confidence threshold and pass")
	}
}

func TestCheckAllPrediction_BlockedByTooManyPositions(t *testing.T) {
	in := passingInput()
	in.OpenPositions = 10 // equals limit → blocked
	_, ok := CheckAllPrediction(in)
	if ok {
		t.Error("expected block on max open positions")
	}
}

func TestCheckAllPrediction_BlockedByLowLiquidity(t *testing.T) {
	in := passingInput()
	in.Volume24h = 500 // below MinLiquidity of 1000
	_, ok := CheckAllPrediction(in)
	if ok {
		t.Error("expected block on low liquidity")
	}
}

func TestCheckAllPrediction_BlockedByHighSpread(t *testing.T) {
	in := passingInput()
	in.Spread = 0.10 // 10%, above MaxSpread of 5%
	_, ok := CheckAllPrediction(in)
	if ok {
		t.Error("expected block on high spread")
	}
}

func TestCheckAllPrediction_BlockedByTimeToResolution(t *testing.T) {
	in := passingInput()
	in.ResolutionDeadline = time.Now().Add(2 * time.Hour) // 2h < 4h minimum
	_, ok := CheckAllPrediction(in)
	if ok {
		t.Error("expected block on insufficient time to resolution")
	}
}

// ── Individual prediction checks ──────────────────────────────────────────────

func TestCheckPredictionLiquidity_Pass(t *testing.T) {
	in := passingInput()
	r := checkPredictionLiquidity(in)
	if !r.Passed {
		t.Errorf("expected pass, got: %s", r.Reason)
	}
}

func TestCheckPredictionLiquidity_Fail(t *testing.T) {
	in := passingInput()
	in.Volume24h = 100
	r := checkPredictionLiquidity(in)
	if r.Passed {
		t.Error("expected fail on low volume")
	}
}

func TestCheckPredictionLiquidity_Disabled(t *testing.T) {
	in := passingInput()
	in.Cfg.Prediction.MinLiquidity = 0
	in.Volume24h = 0
	r := checkPredictionLiquidity(in)
	if !r.Passed {
		t.Error("check should be disabled when MinLiquidity=0")
	}
}

func TestCheckPredictionSpread_Pass(t *testing.T) {
	in := passingInput()
	r := checkPredictionSpread(in)
	if !r.Passed {
		t.Errorf("expected pass, got: %s", r.Reason)
	}
}

func TestCheckPredictionSpread_Fail(t *testing.T) {
	in := passingInput()
	in.Spread = 0.20
	r := checkPredictionSpread(in)
	if r.Passed {
		t.Error("expected fail on wide spread")
	}
}

func TestCheckPredictionSpread_Disabled(t *testing.T) {
	in := passingInput()
	in.Cfg.Prediction.MaxSpread = 0
	in.Spread = 0.99
	r := checkPredictionSpread(in)
	if !r.Passed {
		t.Error("check should be disabled when MaxSpread=0")
	}
}

func TestCheckPredictionSpread_ExactBoundary(t *testing.T) {
	in := passingInput()
	in.Spread = 0.05 // exactly at boundary — should pass (<=)
	r := checkPredictionSpread(in)
	if !r.Passed {
		t.Error("spread at exact boundary should pass")
	}
}

func TestCheckPredictionTimeToResolution_Pass(t *testing.T) {
	in := passingInput()
	r := checkPredictionTimeToResolution(in)
	if !r.Passed {
		t.Errorf("expected pass, got: %s", r.Reason)
	}
}

func TestCheckPredictionTimeToResolution_Fail(t *testing.T) {
	in := passingInput()
	in.ResolutionDeadline = time.Now().Add(1 * time.Hour)
	r := checkPredictionTimeToResolution(in)
	if r.Passed {
		t.Error("expected fail on imminent resolution")
	}
}

func TestCheckPredictionTimeToResolution_ZeroDeadline(t *testing.T) {
	in := passingInput()
	in.ResolutionDeadline = time.Time{} // zero = unknown
	r := checkPredictionTimeToResolution(in)
	if !r.Passed {
		t.Error("unknown deadline should pass (cannot block on missing data)")
	}
}

func TestCheckPredictionTimeToResolution_Disabled(t *testing.T) {
	in := passingInput()
	in.Cfg.Prediction.MinTimeToResolutionHours = 0
	in.ResolutionDeadline = time.Now().Add(30 * time.Minute) // would fail if enabled
	r := checkPredictionTimeToResolution(in)
	if !r.Passed {
		t.Error("check should be disabled when MinTimeToResolutionHours=0")
	}
}

func TestCheckAllPrediction_BlockReasonNonEmpty(t *testing.T) {
	in := passingInput()
	in.Volume24h = 0 // will fail liquidity
	results, ok := CheckAllPrediction(in)
	if ok {
		t.Fatal("expected failure")
	}
	reason := BlockReason(results)
	if reason == "" {
		t.Error("BlockReason should be non-empty on failure")
	}
}
