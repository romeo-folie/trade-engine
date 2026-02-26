// Package risk implements all position risk checks defined in PRD §9.1.
// Each check is independent; CheckAll runs them in PRD-specified order and
// short-circuits on the first failure.
package risk

import (
	"fmt"
	"math"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

// CheckInput carries all the data a risk check needs.
type CheckInput struct {
	Cfg config.RiskConfig

	// Signal fields
	Action     string  // BUY | SELL
	Confidence float64 // 0.0–1.0
	SizePct    float64 // recommended position %
	StopLoss   float64 // price
	TakeProfit float64 // price
	SignalPrice float64

	// Portfolio state
	PortfolioValue  float64
	Cash            float64
	OpenPositions   int
	DailyPnL        float64 // realised P&L today (negative = loss)
	PeakValue       float64 // for drawdown calculation
	LastLossAt      time.Time // last time a losing trade closed
}

// CheckResult is the outcome of a single risk check.
type CheckResult struct {
	Name   string
	Passed bool
	Reason string
}

// CheckAll runs every risk check in PRD §9.1 order.
// Returns the slice of all results and a bool indicating overall pass.
func CheckAll(in CheckInput) ([]CheckResult, bool) {
	checks := []func(CheckInput) CheckResult{
		checkConfidence,
		checkRiskReward,
		checkPositionSize,
		checkMaxOpenPositions,
		checkDailyLossLimit,
		checkMaxDrawdown,
		checkCooldownAfterLoss,
	}

	results := make([]CheckResult, 0, len(checks))
	for _, fn := range checks {
		r := fn(in)
		results = append(results, r)
		if !r.Passed {
			return results, false
		}
	}
	return results, true
}

// BlockReason returns the reason string from the first failed check, or "".
func BlockReason(results []CheckResult) string {
	for _, r := range results {
		if !r.Passed {
			return r.Reason
		}
	}
	return ""
}

// ─── Individual checks ────────────────────────────────────────────────────────

func checkConfidence(in CheckInput) CheckResult {
	name := "confidence_threshold"
	if in.Confidence >= in.Cfg.ConfidenceThreshold {
		return pass(name)
	}
	return fail(name, fmt.Sprintf("confidence %.2f < threshold %.2f",
		in.Confidence, in.Cfg.ConfidenceThreshold))
}

func checkRiskReward(in CheckInput) CheckResult {
	name := "risk_reward_ratio"
	if in.StopLoss == 0 || in.TakeProfit == 0 {
		// No stop/TP provided — skip R:R check.
		return pass(name)
	}
	risk := math.Abs(in.SignalPrice - in.StopLoss)
	reward := math.Abs(in.TakeProfit - in.SignalPrice)
	if risk == 0 {
		return fail(name, "stop-loss equals signal price (zero risk, invalid)")
	}
	rr := reward / risk
	if rr >= in.Cfg.MinRiskReward {
		return pass(name)
	}
	return fail(name, fmt.Sprintf("R:R %.2f < minimum %.2f", rr, in.Cfg.MinRiskReward))
}

func checkPositionSize(in CheckInput) CheckResult {
	name := "position_size_limit"
	if in.SizePct <= in.Cfg.MaxPositionPct {
		return pass(name)
	}
	return fail(name, fmt.Sprintf("size_pct %.1f%% > max %.1f%%",
		in.SizePct, in.Cfg.MaxPositionPct))
}

func checkMaxOpenPositions(in CheckInput) CheckResult {
	name := "max_open_positions"
	if in.OpenPositions < in.Cfg.MaxOpenPositions {
		return pass(name)
	}
	return fail(name, fmt.Sprintf("open positions %d >= limit %d",
		in.OpenPositions, in.Cfg.MaxOpenPositions))
}

func checkDailyLossLimit(in CheckInput) CheckResult {
	name := "daily_loss_limit"
	if in.PortfolioValue == 0 {
		return pass(name) // can't evaluate without portfolio value
	}
	lossPct := (-in.DailyPnL / in.PortfolioValue) * 100
	if lossPct < in.Cfg.DailyLossLimitPct {
		return pass(name)
	}
	return fail(name, fmt.Sprintf("daily loss %.2f%% >= limit %.2f%%",
		lossPct, in.Cfg.DailyLossLimitPct))
}

func checkMaxDrawdown(in CheckInput) CheckResult {
	name := "max_drawdown"
	if in.PeakValue == 0 || in.PortfolioValue == 0 {
		return pass(name)
	}
	drawdownPct := ((in.PeakValue - in.PortfolioValue) / in.PeakValue) * 100
	if drawdownPct < in.Cfg.MaxDrawdownPct {
		return pass(name)
	}
	return fail(name, fmt.Sprintf("drawdown %.2f%% >= limit %.2f%% — circuit breaker active",
		drawdownPct, in.Cfg.MaxDrawdownPct))
}

func checkCooldownAfterLoss(in CheckInput) CheckResult {
	name := "cooldown_after_loss"
	if in.LastLossAt.IsZero() || in.Cfg.CooldownAfterLoss == 0 {
		return pass(name)
	}
	cooldownEnd := in.LastLossAt.Add(time.Duration(in.Cfg.CooldownAfterLoss) * time.Minute)
	if time.Now().After(cooldownEnd) {
		return pass(name)
	}
	remaining := time.Until(cooldownEnd).Round(time.Second)
	return fail(name, fmt.Sprintf("cooldown active — %s remaining after last loss", remaining))
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func pass(name string) CheckResult { return CheckResult{Name: name, Passed: true} }
func fail(name, reason string) CheckResult {
	return CheckResult{Name: name, Passed: false, Reason: reason}
}
