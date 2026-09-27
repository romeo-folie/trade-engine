package prompts

import (
	"fmt"
	"strconv"
)

// RiskPolicy contains only the deterministic BUY limits that analysts need to
// structure a proposal. The confidence gate is deliberately excluded so it
// cannot anchor model scores just above the entry threshold.
type RiskPolicy struct {
	MaxTickerExposurePct float64
	MaxPerTicker         int
	MinRiskReward        float64
	// Configured distinguishes active zero-valued limits from an omitted policy.
	Configured bool
}

// Normalized preserves the historical prompt limits for direct callers that
// do not supply an engine risk policy. The engine always supplies active config.
func (p RiskPolicy) Normalized() RiskPolicy {
	if !p.Configured {
		if p.MaxTickerExposurePct <= 0 {
			p.MaxTickerExposurePct = 5
		}
		if p.MinRiskReward <= 0 {
			p.MinRiskReward = 1.2
		}
	}
	if p.MaxPerTicker < 0 {
		p.MaxPerTicker = 0
	}
	return p
}

func riskNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// RenderRiskPolicy distinguishes an incremental order recommendation from the
// portfolio's aggregate ticker cap. A position-count limit is not divided into
// a fabricated hard per-entry percentage: existing exposure determines the
// remaining room for a pyramid add.
func RenderRiskPolicy(policy RiskPolicy) string {
	policy = policy.Normalized()
	cap := riskNumber(policy.MaxTickerExposurePct)
	message := fmt.Sprintf(`BUY RISK POLICY:
- size_pct is the proposed incremental BUY order size as a percentage of portfolio equity; it must be between 0 and %s, and may need to be smaller when this ticker is already held.
- Combined long exposure in this ticker after the proposed BUY must not exceed %s%% of portfolio equity. Subtract existing same-ticker exposure before recommending a pyramid add; %s%% is not extra capacity for each entry.`, cap, cap, cap)
	if policy.MaxPerTicker > 0 {
		message += fmt.Sprintf("\n- At most %d open long positions in this ticker; this limits entry count, not the size of each entry.", policy.MaxPerTicker)
	}
	message += "\n- Deterministic risk checks remain authoritative if the portfolio context cannot establish exact remaining capacity."
	return message
}

// RenderRRSelfFilter keeps the proposal's self-filter aligned with the active
// deterministic minimum, including in the arbiter's custom prompt.
func RenderRRSelfFilter(policy RiskPolicy) string {
	minimumValue := policy.Normalized().MinRiskReward
	if minimumValue <= 0 {
		return "R:R SELF-FILTER (MANDATORY): Compute R:R as: reward = abs(take_profit - signal_price); risk = abs(signal_price - stop_loss); R:R = reward / risk. No minimum R:R floor is configured; do not reject a BUY solely for R:R. A defined nonzero-risk stop is still required."
	}
	minimum := riskNumber(minimumValue)
	return fmt.Sprintf("R:R SELF-FILTER (MANDATORY): Compute R:R as: reward = abs(take_profit - signal_price); risk = abs(signal_price - stop_loss); R:R = reward / risk. If R:R < %s, widen the take_profit toward the next structural target, or tighten the stop_loss to nearer structure — do not propose entry below %s. Only emit BUY if R:R >= %s.", minimum, minimum, minimum)
}
