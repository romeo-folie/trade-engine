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
	Action      string // BUY | SELL
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
	OpenByTicker map[string]int
	DailyPnL     float64   // realised P&L today (negative = loss)
	PeakValue    float64   // for drawdown calculation
	LastLossAt   time.Time // last time a losing trade closed

	// HoldsLong is true when there is an open long trade in Ticker.
	// LongQuantity is the total shares held long (sum across open trades).
	// Used by checkPositionOwnership to block SELL signals with no position
	// or where the proposed sell quantity exceeds what is held.
	HoldsLong    bool
	LongQuantity float64

	// LongAvgEntryPrice is the quantity-weighted average entry price across
	// all open long lots in Ticker. Consumed by checkPyramidPolicy (#324) to
	// determine whether a BUY-on-existing-long qualifies as a pyramid add.
	// Zero when HoldsLong is false or quantities are unknown.
	LongAvgEntryPrice float64

	// LongMaxStop is the highest (most-protective for a long) stop_loss across
	// all open long lots in Ticker. Consumed by checkPyramidPolicy (#324) to
	// reject pyramid attempts where the existing protective stop has already
	// drifted at or above the current signal price (e.g. after a gap down
	// before monitorPositions runs). Without this guard the new lot would be
	// inserted with a stop above market and immediately closed on the next
	// monitor pass. Zero when HoldsLong is false or no stops are recorded.
	LongMaxStop float64
}

// CheckResult is the outcome of a single risk check.
type CheckResult struct {
	Name   string
	Passed bool
	Reason string
}

// CheckAll runs every risk check in PRD §9.1 order.
// Returns the slice of all results and a bool indicating overall pass.
//
// Position ownership runs first (#319) so SELL signals on tickers we don't
// hold block out cleanly with the canonical NoPositionBlockReason message
// ("no open long position in <ticker> — cannot sell") rather than getting
// labelled with confidence/R:R complaints that don't match the real cause.
// For BUY signals checkPositionOwnership is a cheap no-op pass, so the
// reorder has no impact on BUY-side latency or behaviour.
func CheckAll(in CheckInput) ([]CheckResult, bool) {
	checks := []func(CheckInput) CheckResult{
		checkPositionOwnership,    // #337 — runs first; SELL on no-long blocks cleanly
		checkPyramidPolicy,        // #324 — pyramid eligibility on existing long
		checkConfidenceTiered,     // #339 — replaces checkConfidence
		checkRiskReward,
		checkPositionSize,
		checkMaxDollarRisk,        // #335
		checkMaxOpenPositions,
		checkMaxPerTicker,         // #312 (Epic A)
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

// checkConfidence is the static-floor confidence check retained for backwards
// compatibility and direct unit testing. Production CheckAll uses
// checkConfidenceTiered (#317) which falls back to this floor when no tiers
// match or none are configured.
func checkConfidence(in CheckInput) CheckResult {
	name := "confidence_threshold"
	if in.Confidence >= in.Cfg.ConfidenceThreshold {
		return pass(name)
	}
	return fail(name, fmt.Sprintf("confidence %.2f < threshold %.2f",
		in.Confidence, in.Cfg.ConfidenceThreshold))
}

// checkConfidenceTiered enforces a confidence floor that varies with R:R
// (#317). Tiers from Cfg.ConfidenceTiers are scanned in declaration order;
// the first tier whose RRMin is <= the trade's R:R applies. When no R:R can
// be computed (no stop/TP) or no tier matches, we fall back to the static
// ConfidenceThreshold floor — this preserves the pre-#317 behaviour for
// configs that haven't opted into tiers.
func checkConfidenceTiered(in CheckInput) CheckResult {
	name := "confidence_threshold"

	// When tiers aren't configured, behave exactly like the legacy static check.
	if len(in.Cfg.ConfidenceTiers) == 0 {
		return checkConfidence(in)
	}

	// Compute R:R if a stop and target are present. Without them we can't
	// place the signal in any tier, so fall back to the static floor.
	rr, hasRR := computeRR(in)
	if !hasRR {
		return checkConfidence(in)
	}

	for _, tier := range in.Cfg.ConfidenceTiers {
		if rr >= tier.RRMin {
			if in.Confidence >= tier.ConfMin {
				return pass(name)
			}
			return fail(name, fmt.Sprintf(
				"confidence %.2f < tier-required %.2f (R:R %.2f)",
				in.Confidence, tier.ConfMin, rr,
			))
		}
	}

	// R:R is below every configured tier floor. If it's also below the
	// configured MinRiskReward, defer the rejection to checkRiskReward so
	// the operator gets an accurate "R:R x.xx < minimum y.yy" reason
	// rather than a confidence reason that masks the real cause (#317
	// review). CheckAll runs confidence before R:R, so without this pass
	// here the confidence reason wins. When a misconfigured tier list
	// leaves a gap above MinRiskReward, fall back to the static floor so
	// signals still get gated.
	if rr < in.Cfg.MinRiskReward {
		return pass(name)
	}
	return checkConfidence(in)
}

// computeRR derives the trade's R:R ratio from CheckInput. Returns (0, false)
// when stop/TP/price are missing, when risk is zero, or when BUY geometry is
// inverted (stop at-or-above entry, target at-or-below entry).
//
// SELL is exit-only in this codebase (see checkPositionOwnership and the
// checkMaxDollarRisk SELL skip), and analyst SELL signals can ship inverted
// stop/target relative to a pure short setup. To avoid spuriously rejecting
// legitimate exits we keep the existing math.Abs semantics for SELL and only
// enforce geometry on BUY (the side that actually opens new risk).
func computeRR(in CheckInput) (float64, bool) {
	if in.StopLoss == 0 || in.TakeProfit == 0 || in.SignalPrice == 0 {
		return 0, false
	}
	if in.Action == "BUY" {
		if in.StopLoss >= in.SignalPrice || in.TakeProfit <= in.SignalPrice {
			return 0, false
		}
	}
	risk := math.Abs(in.SignalPrice - in.StopLoss)
	reward := math.Abs(in.TakeProfit - in.SignalPrice)
	if risk == 0 {
		return 0, false
	}
	return reward / risk, true
}

func checkRiskReward(in CheckInput) CheckResult {
	name := "risk_reward_ratio"
	if in.StopLoss == 0 || in.TakeProfit == 0 {
		// No stop/TP provided — skip R:R check.
		return pass(name)
	}
	// Validate BUY geometry before taking absolute values: an inverted long
	// setup (stop >= entry or target <= entry) would otherwise yield a
	// positive R:R via math.Abs and pass the gate, only to fire its stop
	// immediately at execution because the engine treats StopLoss as a long
	// stop that triggers on price <= stop. SELL is exit-only and skipped to
	// preserve the legacy math.Abs behaviour for analyst exit signals.
	if in.Action == "BUY" {
		if in.StopLoss >= in.SignalPrice || in.TakeProfit <= in.SignalPrice {
			return fail(name, fmt.Sprintf(
				"invalid SL/TP geometry for BUY (stop %.2f >= signal %.2f or take_profit %.2f <= signal %.2f)",
				in.StopLoss, in.SignalPrice, in.TakeProfit, in.SignalPrice,
			))
		}
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
	// SELLs close the HELD quantity (#436) — their executed size is never
	// derived from size_pct, so capping the analyst's proposal would only
	// block legitimate exits (same skip pattern as checkMaxDollarRisk).
	if in.Action != "BUY" {
		return pass(name)
	}
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
// This check applies only to opening trades (BUY); exits (SELL) should not
// be blocked by an entry-risk cap. SELL is exit-only in this codebase
// (checkPositionOwnership blocks SELL with no open long), and the analyst
// prompt requests stop_loss/take_profit on SELL signals too — running the
// entry-risk gate against those would block legitimate exits.
//
// Skipped when PortfolioValue == 0 (mirrors checkDailyLossLimit), when
// MaxDollarRiskPct <= 0 (treat as disabled to keep older configs safe),
// when StopLoss == 0 (no stop set, can't compute risk), or for non-BUY
// actions (entry-risk cap applies to opening trades only).
// Fails when StopLoss equals SignalPrice (zero risk distance, invalid setup;
// not necessarily caught upstream because checkRiskReward is skipped when
// TakeProfit is also zero).
//
// Quantity estimate uses Cash, not PortfolioValue, to match the actual
// engine sizing in executeSignal (`pState.Cash * SizePct/100 / SignalPrice`).
// Using PortfolioValue here would overestimate qty when cash is partially
// deployed and produce false blocks.
func checkMaxDollarRisk(in CheckInput) CheckResult {
	name := "max_dollar_risk"
	if in.Action != "BUY" {
		return pass(name)
	}
	if in.PortfolioValue == 0 || in.Cfg.MaxDollarRiskPct <= 0 {
		return pass(name)
	}
	if in.StopLoss == 0 {
		return pass(name)
	}
	if in.SignalPrice <= 0 {
		return pass(name)
	}
	const epsilon = 1e-9
	if math.Abs(in.SignalPrice-in.StopLoss) < epsilon {
		return fail(name, "stop-loss equals signal price (zero risk, invalid)")
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
		return fail(name, NoPositionBlockReason(in.Ticker))
	}
	// No oversell check: since #436 a SELL closes the HELD long quantity —
	// the executed size is derived from the open lots, never from the
	// signal's cash-based size_pct, so a size proposal can't over-sell. The
	// old cash-derived check here could wrongly block legitimate exits when
	// cash × size_pct exceeded the (smaller) held position.
	return pass(name)
}

// checkPyramidPolicy gates BUY signals that would land on a ticker with an
// existing open long (#324). Three outcomes:
//
//   - No existing long → pass; this is a fresh entry.
//   - Existing long but PyramidEnabled=false → fail with "pyramid_disabled".
//     Default rollout: operators must explicitly opt in to pyramiding.
//   - Existing long and PyramidEnabled=true → pass only if unrealised gain
//     on the average entry is at least PyramidMinGainPct. Below threshold
//     fails with "pyramid_min_gain_not_met" so we never double down on a
//     flat or losing position.
//
// SELL signals and BUYs on tickers with no open long are unaffected; the
// per-ticker cap (#312) still bounds the number of pyramid adds.
func checkPyramidPolicy(in CheckInput) CheckResult {
	name := "pyramid_policy"
	if in.Action != "BUY" {
		return pass(name)
	}
	if !in.HoldsLong {
		return pass(name)
	}
	if !in.Cfg.PyramidEnabled {
		return fail(name, fmt.Sprintf(
			"pyramid_disabled: %s already has an open long and risk.pyramid_enabled is false",
			in.Ticker,
		))
	}
	if in.LongAvgEntryPrice <= 0 || in.SignalPrice <= 0 {
		return fail(name, fmt.Sprintf(
			"cannot evaluate pyramid gain for %s (avg_entry=%.4f signal_price=%.4f)",
			in.Ticker, in.LongAvgEntryPrice, in.SignalPrice,
		))
	}
	gainPct := ((in.SignalPrice - in.LongAvgEntryPrice) / in.LongAvgEntryPrice) * 100
	if gainPct < in.Cfg.PyramidMinGainPct {
		return fail(name, fmt.Sprintf(
			"pyramid_min_gain_not_met: %s up %.2f%% < required %.2f%%",
			in.Ticker, gainPct, in.Cfg.PyramidMinGainPct,
		))
	}
	// The new lot's stop is tightened downstream to max(canonical existing
	// stop, weighted avg entry). When that canonical stop has drifted at or
	// above the current signal price the resulting long stop would sit above
	// market — the new trade closes immediately on the next monitor pass.
	// Reject the pyramid attempt instead of inserting a trade that's already
	// underwater. The pre-existing position is unaffected.
	if in.LongMaxStop > 0 && in.LongMaxStop >= in.SignalPrice {
		return fail(name, fmt.Sprintf(
			"pyramid_stop_above_price: %s existing stop %.4f >= signal price %.4f",
			in.Ticker, in.LongMaxStop, in.SignalPrice,
		))
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

// IsSellOnlyStrategy reports whether a strategy's primary action is SELL —
// i.e. an exit/close strategy that, by design, never recommends BUY. The
// engine uses this to short-circuit LLM calls (#319) when such a strategy is
// scheduled against a ticker with no open long: the analyst would only be
// able to recommend SELL, which the position-ownership gate would then block,
// so the LLM call is pure waste.
//
// Canonical list (matched case-sensitively against the lowercase strategy
// names used in config and the engine):
//
//   - sell_only
//   - exit_signal
//   - close_position
//
// The dual-direction strategies that ship today (trend_following,
// mean_reversion, momentum, news_sentiment) all emit BUY, SELL, HOLD, or
// WAIT depending on context, so they are not on the list and always reach
// the analyst. Add new exit-only strategy names here as they are introduced.
func IsSellOnlyStrategy(strategy string) bool {
	switch strategy {
	case "sell_only", "exit_signal", "close_position":
		return true
	}
	return false
}

// NoPositionBlockReason returns the canonical block reason recorded on
// signals when a SELL is rejected because the portfolio holds no open long in
// the ticker. Used by both checkPositionOwnership (post-LLM gate) and the
// pre-LLM SELL ownership short-circuit in engine.runAnalysis (#319), so the
// blocked-signal row and the SignalBlocked event payload carry identical
// reason text in either path. Centralised so dashboards and counterfactual
// logs can pattern-match on a single string.
func NoPositionBlockReason(ticker string) string {
	return fmt.Sprintf("no open long position in %s — cannot sell", ticker)
}

// ─── Counterfactual evaluation (Issue #325) ──────────────────────────────────

// LooseConfig overrides per the #325 spec. Used by EvaluateCounterfactual to
// re-run CheckAll against a relaxed gate set so the operator can see, over
// time, how often the strict gates would have admitted a signal under the
// looser policy.
//
// Only the calibratable knobs are loosened (R:R floor, confidence floor).
// Position-budget / cooldown / daily-loss / drawdown gates are not loosened
// because those are state machines, not calibration knobs.
const (
	LooseMinRiskReward       = 1.30
	LooseConfidenceThreshold = 0.55
)

// IsCalibratable reports whether `check` belongs to the family the
// counterfactual is meaningful for. Defaults to true for unknown names so
// future checks are observed by default — drop them into the non-calibratable
// list explicitly when the gate is a hard state machine.
//
// Implemented as a switch (not a map lookup) because the non-calibratable
// list is stable and code review caught the previous duplicate
// `CalibratableChecks` map drifting out of sync with this function — one
// source of truth here avoids the same bug recurring.
func IsCalibratable(check string) bool {
	if check == "" {
		return false
	}
	switch check {
	case "max_open_positions", "position_ownership", "daily_loss_limit",
		"max_drawdown", "cooldown_after_loss", "max_per_ticker",
		"pyramid_policy", "pyramid_disabled":
		return false
	}
	// Default to calibratable when in doubt — the spec explicitly says
	// "default to calibratable rather than skip" so a new check doesn't
	// silently disappear from observability.
	return true
}

// CounterfactualResult is the output of EvaluateCounterfactual.
type CounterfactualResult struct {
	// PassedLoose is true when CheckAll on the loose config returns pass.
	PassedLoose bool
	// FlippedCheck names the strict check that failed but the loose check
	// passed. Empty when PassedLoose is false (or when no flip occurred —
	// e.g. both runs failed on the same check, indicating the gate would
	// have failed regardless of threshold relaxation).
	FlippedCheck string
	// LooseResults is the full slice from the loose CheckAll, exposed for
	// debugging/tests; production callers only need PassedLoose +
	// FlippedCheck.
	LooseResults []CheckResult
}

// EvaluateCounterfactual re-runs CheckAll against a loose config derived from
// `in` and reports whether the loose policy would have admitted the signal.
// Strict-results is the slice produced by the canonical CheckAll the engine
// already ran — passed in so we can determine the flipped check without
// re-running it.
//
// The function performs no I/O and is safe to call inside the hot path.
// Designed to live alongside the existing CheckAll call without affecting
// which signals execute (this is observability-only).
func EvaluateCounterfactual(in CheckInput, strictResults []CheckResult) CounterfactualResult {
	loose := in
	loose.Cfg = in.Cfg // copy
	loose.Cfg.MinRiskReward = LooseMinRiskReward
	loose.Cfg.ConfidenceTiers = nil
	loose.Cfg.ConfidenceThreshold = LooseConfidenceThreshold

	looseResults, passedLoose := CheckAll(loose)
	out := CounterfactualResult{PassedLoose: passedLoose, LooseResults: looseResults}
	if !passedLoose {
		return out
	}
	// Find the strict failure that the loose run cleared. The strict slice
	// short-circuits on first failure so the failed check is the last
	// element; identify it by name and confirm the loose run admitted it.
	for _, r := range strictResults {
		if !r.Passed {
			out.FlippedCheck = r.Name
			break
		}
	}
	return out
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func pass(name string) CheckResult { return CheckResult{Name: name, Passed: true} }
func fail(name, reason string) CheckResult {
	return CheckResult{Name: name, Passed: false, Reason: reason}
}
