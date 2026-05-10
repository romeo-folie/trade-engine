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
	Action      string  // BUY | SELL
	Ticker      string
	Confidence  float64 // 0.0–1.0
	SizePct     float64 // recommended position %
	StopLoss    float64 // price
	TakeProfit  float64 // price
	SignalPrice float64

	// Portfolio state
	PortfolioValue float64
	Cash           float64
	OpenPositions  int
	// OpenByTicker holds the number of open positions in each ticker, counted
	// in the same direction as the incoming signal (longs for BUY, shorts for
	// SELL). Consumed by checkMaxPerTicker (#312). May be nil; checks treat a
	// missing key as 0.
	OpenByTicker   map[string]int
	DailyPnL       float64   // realised P&L today (negative = loss)
	PeakValue      float64   // for drawdown calculation
	LastLossAt     time.Time // last time a losing trade closed

	// HoldsLong is true when there is an open long trade in Ticker.
	// LongQuantity is the total shares held long (sum across open trades).
	// Used by checkPositionOwnership to block SELL signals with no position
	// or where the proposed sell quantity exceeds what is held.
	HoldsLong    bool
	LongQuantity float64
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
		checkPositionOwnership,
		checkRiskReward,
		checkPositionSize,
		checkMaxDollarRisk,
		checkMaxOpenPositions,
		checkMaxPerTicker,
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

// checkMaxDollarRisk enforces a hard cap on per-trade dollar at risk
// (qty × |entry − stop|) as a percent of portfolio equity (#320). This is
// independent of checkPositionSize, which caps notional exposure: a small
// notional with a far stop can still risk more than acceptable, and a large
// notional with a tight stop can risk less. Both gates protect against
// different failure modes.
//
// Skipped when PortfolioValue == 0 (mirrors checkDailyLossLimit), when
// MaxDollarRiskPct <= 0 (treat as disabled to keep older configs safe),
// or when StopLoss == 0 (mirrors checkRiskReward — no stop, nothing to
// measure). Zero stop-distance with a non-zero stop is already caught
// upstream by checkRiskReward.
//
// Quantity estimate uses Cash, not PortfolioValue, to match the actual
// engine sizing in executeSignal (`pState.Cash * SizePct/100 / SignalPrice`).
// Using PortfolioValue here would overestimate qty when cash is partially
// deployed and produce false blocks.
func checkMaxDollarRisk(in CheckInput) CheckResult {
	name := "max_dollar_risk"
	if in.PortfolioValue == 0 || in.Cfg.MaxDollarRiskPct <= 0 {
		return pass(name)
	}
	if in.StopLoss == 0 {
		return pass(name)
	}
	if in.SignalPrice <= 0 {
		return pass(name)
	}
	qty := (in.SizePct / 100) * in.Cash / in.SignalPrice
	dollarAtRisk := qty * math.Abs(in.SignalPrice-in.StopLoss)
	riskPct := dollarAtRisk / in.PortfolioValue * 100
	if riskPct <= in.Cfg.MaxDollarRiskPct {
		return pass(name)
	}
	return fail(name, fmt.Sprintf("$-at-risk %.2f%% > limit %.2f%%",
		riskPct, in.Cfg.MaxDollarRiskPct))
}

func checkMaxOpenPositions(in CheckInput) CheckResult {
	name := "max_open_positions"
	if in.OpenPositions < in.Cfg.MaxOpenPositions {
		return pass(name)
	}
	return fail(name, fmt.Sprintf("open positions %d >= limit %d",
		in.OpenPositions, in.Cfg.MaxOpenPositions))
}

// checkMaxPerTicker caps the number of open positions per ticker in the
// signal's direction (#312). Skipped when Cfg.MaxPerTicker <= 0; the
// default in Defaults() is 0 so older configs without the new key upgrade
// safely without unexpected blocks. Opt in by setting a positive value
// (risk.max_per_ticker) in your config — config.example.yaml recommends 2.
func checkMaxPerTicker(in CheckInput) CheckResult {
	name := "max_per_ticker"
	if in.Cfg.MaxPerTicker <= 0 {
		return pass(name)
	}
	count := in.OpenByTicker[in.Ticker]
	if count < in.Cfg.MaxPerTicker {
		return pass(name)
	}
	return fail(name, fmt.Sprintf("open positions in %s reach per-ticker limit %d",
		in.Ticker, in.Cfg.MaxPerTicker))
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

func checkPositionOwnership(in CheckInput) CheckResult {
	name := "position_ownership"
	if in.Action != "SELL" {
		return pass(name)
	}
	if !in.HoldsLong {
		return fail(name, fmt.Sprintf("no open long position in %s — cannot sell", in.Ticker))
	}
	// Verify the proposed sell quantity does not exceed shares held.
	// Overselling would result in a net short even with a position check.
	if in.SignalPrice > 0 && in.LongQuantity > 0 {
		proposedQty := (in.Cash * in.SizePct / 100) / in.SignalPrice
		if proposedQty > in.LongQuantity {
			return fail(name, fmt.Sprintf(
				"sell qty %.2f exceeds held long %.2f in %s",
				proposedQty, in.LongQuantity, in.Ticker,
			))
		}
	}
	return pass(name)
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
