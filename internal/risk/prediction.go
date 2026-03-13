package risk

import (
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// PredictionCheckInput carries prediction-market-specific data for risk checks.
// Portfolio state fields are shared with CheckInput and should be populated
// from the same portfolio snapshot.
type PredictionCheckInput struct {
	// Base risk config (confidence, position size, open positions, etc.).
	Cfg config.RiskConfig

	// Signal fields
	Action     string  // BUY | SELL
	Confidence float64 // 0.0–1.0
	SizePct    float64 // recommended position %

	// Portfolio state
	PortfolioValue float64
	Cash           float64
	OpenPositions  int
	DailyPnL       float64
	PeakValue      float64
	LastLossAt     time.Time

	// Prediction-specific market data
	Spread             float64   // bid-ask spread as fraction of mid-price (0.05 = 5%)
	Volume24h          float64   // 24-hour trading volume (USD)
	ResolutionDeadline time.Time // when the contract resolves (zero = unknown)
}

// CheckAllPrediction runs baseline risk checks (reusing stock checks where
// applicable) followed by prediction-specific checks.
//
// Baseline checks applied:
//   - confidence_threshold (using Prediction.ConfidenceThreshold if non-zero)
//   - position_size_limit
//   - max_open_positions
//   - daily_loss_limit
//   - max_drawdown
//   - cooldown_after_loss
//
// Prediction-specific checks:
//   - liquidity_gate
//   - spread_gate
//   - time_to_resolution
func CheckAllPrediction(in PredictionCheckInput) ([]CheckResult, bool) {
	// Build a CheckInput for baseline checks, overriding confidence threshold
	// with the prediction-specific value when configured.
	baseCfg := in.Cfg
	if in.Cfg.Prediction.ConfidenceThreshold > 0 {
		baseCfg.ConfidenceThreshold = in.Cfg.Prediction.ConfidenceThreshold
	}

	baseIn := CheckInput{
		Cfg:            baseCfg,
		Action:         in.Action,
		Confidence:     in.Confidence,
		SizePct:        in.SizePct,
		PortfolioValue: in.PortfolioValue,
		Cash:           in.Cash,
		OpenPositions:  in.OpenPositions,
		DailyPnL:       in.DailyPnL,
		PeakValue:      in.PeakValue,
		LastLossAt:     in.LastLossAt,
	}

	baseChecks := []func(CheckInput) CheckResult{
		checkConfidence,
		checkPositionSize,
		checkMaxOpenPositions,
		checkDailyLossLimit,
		checkMaxDrawdown,
		checkCooldownAfterLoss,
	}

	results := make([]CheckResult, 0, len(baseChecks)+3)
	for _, fn := range baseChecks {
		r := fn(baseIn)
		results = append(results, r)
		if !r.Passed {
			return results, false
		}
	}

	// Prediction-specific checks.
	predChecks := []func(PredictionCheckInput) CheckResult{
		checkPredictionLiquidity,
		checkPredictionSpread,
		checkPredictionTimeToResolution,
	}
	for _, fn := range predChecks {
		r := fn(in)
		results = append(results, r)
		if !r.Passed {
			return results, false
		}
	}

	return results, true
}

// ── Prediction-specific checks ────────────────────────────────────────────────

func checkPredictionLiquidity(in PredictionCheckInput) CheckResult {
	name := "prediction_liquidity"
	minLiq := in.Cfg.Prediction.MinLiquidity
	if minLiq <= 0 {
		return pass(name) // check disabled
	}
	if in.Volume24h >= minLiq {
		return pass(name)
	}
	return fail(name, fmt.Sprintf("24h volume $%.0f < minimum $%.0f", in.Volume24h, minLiq))
}

func checkPredictionSpread(in PredictionCheckInput) CheckResult {
	name := "prediction_spread"
	maxSpread := in.Cfg.Prediction.MaxSpread
	if maxSpread <= 0 {
		return pass(name) // check disabled
	}
	if in.Spread <= maxSpread {
		return pass(name)
	}
	return fail(name, fmt.Sprintf("spread %.2f%% > max %.2f%%",
		in.Spread*100, maxSpread*100))
}

func checkPredictionTimeToResolution(in PredictionCheckInput) CheckResult {
	name := "prediction_time_to_resolution"
	minHours := in.Cfg.Prediction.MinTimeToResolutionHours
	if minHours <= 0 || in.ResolutionDeadline.IsZero() {
		return pass(name) // check disabled or deadline unknown
	}
	remaining := time.Until(in.ResolutionDeadline).Hours()
	if remaining >= minHours {
		return pass(name)
	}
	return fail(name, fmt.Sprintf("%.1fh to resolution < minimum %.1fh",
		remaining, minHours))
}
