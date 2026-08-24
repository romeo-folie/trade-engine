// Package antichase evaluates deterministic entry-timing risks for new stock
// BUYs. It deliberately has no dependency on the engine, providers, or store so
// the exact same policy can run when an analysis is produced and when a broker
// quote is about to be submitted.
package antichase

import (
	"fmt"
	"math"
	"time"
)

const (
	PolicyVersion            = 1
	MinimumCompletedSessions = 21
	TrueHighSessions         = 253
)

// Policy contains the operator-tunable portions of the V1 anti-chase policy.
// Session horizons remain fixed in code so audit records have stable semantics.
type Policy struct {
	Enabled bool `json:"enabled"`

	StaleCatalystRunupEnabled bool    `json:"stale_catalyst_runup_enabled"`
	StaleAfterHours           float64 `json:"stale_after_hours"`
	MinReturn3SessionPct      float64 `json:"min_return_3_session_pct"`
	WeakVolumeRatioMax        float64 `json:"weak_volume_ratio_max"`
	StaleEMAExtensionATR      float64 `json:"stale_ema_extension_atr"`

	PriorHighExtensionEnabled bool    `json:"prior_high_extension_enabled"`
	NearPriorHighPct          float64 `json:"near_prior_high_pct"`
	HighEMAExtensionATR       float64 `json:"high_ema_extension_atr"`

	MomentumExhaustionEnabled bool    `json:"momentum_exhaustion_enabled"`
	RSIMin                    float64 `json:"rsi_min"`
	StochKMin                 float64 `json:"stoch_k_min"`
	ExhaustionReturn3Pct      float64 `json:"exhaustion_return_3_session_pct"`
}

// Snapshot is the point-in-time evidence supplied to prompts and policy
// evaluation. All price-derived fields are computed from completed daily bars;
// ReferencePrice may be a current mark or the final broker ask.
type Snapshot struct {
	SchemaVersion   int       `json:"schema_version"`
	Ticker          string    `json:"ticker"`
	ObservedAt      time.Time `json:"observed_at"`
	ReferencePrice  float64   `json:"reference_price"`
	ReferenceSource string    `json:"reference_source"`

	EvidenceStatus    string `json:"evidence_status"` // available | unavailable
	EvidenceReason    string `json:"evidence_reason,omitempty"`
	CompletedSessions int    `json:"completed_sessions"`
	HistoryScope      string `json:"history_scope"` // true_52w | limited_history

	Return3SessionPct float64 `json:"return_3_session_pct"`
	EMA20             float64 `json:"ema20"`
	ATR14             float64 `json:"atr14"`
	EMAExtensionATR   float64 `json:"ema_extension_atr"`
	RSI14             float64 `json:"rsi14"`
	StochK            float64 `json:"stoch_k"`
	BBUpper           float64 `json:"bb_upper"`
	VolumeRatio       float64 `json:"volume_ratio"`

	PriorHigh52Week    float64 `json:"prior_high_52_week,omitempty"`
	PriorHighProximity float64 `json:"prior_high_proximity,omitempty"`

	CatalystAt       *time.Time `json:"catalyst_at,omitempty"`
	CatalystSource   string     `json:"catalyst_source,omitempty"`
	CatalystAgeHours float64    `json:"catalyst_age_hours,omitempty"`
}

// RuleDecision records every combination independently so operators can see
// why a BUY passed as well as why it was stopped.
type RuleDecision struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	Matched bool   `json:"matched"`
	Reason  string `json:"reason"`
}

// Decision is safe to persist verbatim with an analysis or signal gate audit.
type Decision struct {
	PolicyVersion int            `json:"policy_version"`
	Status        string         `json:"status"` // passed | blocked | unavailable_block | disabled | not_applicable
	Reason        string         `json:"reason,omitempty"`
	Rules         []RuleDecision `json:"rules"`
}

func DefaultPolicy() Policy {
	return Policy{
		Enabled: true,

		StaleCatalystRunupEnabled: true,
		StaleAfterHours:           24,
		MinReturn3SessionPct:      8,
		WeakVolumeRatioMax:        .75,
		StaleEMAExtensionATR:      1.75,

		PriorHighExtensionEnabled: true,
		NearPriorHighPct:          5,
		HighEMAExtensionATR:       2,

		MomentumExhaustionEnabled: true,
		RSIMin:                    70,
		StochKMin:                 80,
		ExhaustionReturn3Pct:      5,
	}
}

// Evaluate applies V1 to a prospective BUY. Callers should skip this function
// for SELL/HOLD/WAIT; Evaluate intentionally does not need a strategy/lens.
func Evaluate(snapshot Snapshot, policy Policy) Decision {
	decision := Decision{PolicyVersion: PolicyVersion}
	if !policy.Enabled {
		decision.Status = "disabled"
		return decision
	}
	if snapshot.EvidenceStatus != "available" || !hasMinimumEvidence(snapshot) {
		decision.Status = "unavailable_block"
		if snapshot.EvidenceReason != "" {
			decision.Reason = snapshot.EvidenceReason
		} else {
			decision.Reason = fmt.Sprintf("anti-chase: at least %d valid completed daily sessions and derived indicators are required", MinimumCompletedSessions)
		}
		return decision
	}

	weakVolume := snapshot.VolumeRatio <= policy.WeakVolumeRatioMax
	staleCatalyst := snapshot.CatalystAt != nil && snapshot.CatalystAgeHours > policy.StaleAfterHours
	staleExtension := snapshot.EMAExtensionATR >= policy.StaleEMAExtensionATR
	rule1 := RuleDecision{
		ID: "stale_catalyst_runup", Enabled: policy.StaleCatalystRunupEnabled,
		Matched: policy.StaleCatalystRunupEnabled && staleCatalyst &&
			snapshot.Return3SessionPct >= policy.MinReturn3SessionPct && (weakVolume || staleExtension),
		Reason: fmt.Sprintf("catalyst_age=%.1fh stale=%t; return_3_session=%.2f%%; volume_ratio=%.2fx weak=%t; ema_extension=%.2f ATR",
			snapshot.CatalystAgeHours, staleCatalyst, snapshot.Return3SessionPct, snapshot.VolumeRatio, weakVolume, snapshot.EMAExtensionATR),
	}

	hasTrueHigh := snapshot.HistoryScope == "true_52w" && snapshot.PriorHigh52Week > 0
	nearPriorHigh := hasTrueHigh && snapshot.PriorHighProximity >= 1-policy.NearPriorHighPct/100
	rule2 := RuleDecision{
		ID: "prior_high_extension", Enabled: policy.PriorHighExtensionEnabled,
		Matched: policy.PriorHighExtensionEnabled && nearPriorHigh &&
			snapshot.EMAExtensionATR >= policy.HighEMAExtensionATR && weakVolume,
		Reason: fmt.Sprintf("history_scope=%s; prior_high_proximity=%.3f near=%t; ema_extension=%.2f ATR; volume_ratio=%.2fx weak=%t",
			snapshot.HistoryScope, snapshot.PriorHighProximity, nearPriorHigh, snapshot.EMAExtensionATR, snapshot.VolumeRatio, weakVolume),
	}

	exhausted := snapshot.RSI14 >= policy.RSIMin || snapshot.StochK >= policy.StochKMin || snapshot.ReferencePrice > snapshot.BBUpper
	rule3 := RuleDecision{
		ID: "momentum_exhaustion", Enabled: policy.MomentumExhaustionEnabled,
		Matched: policy.MomentumExhaustionEnabled && exhausted &&
			snapshot.Return3SessionPct >= policy.ExhaustionReturn3Pct && weakVolume,
		Reason: fmt.Sprintf("rsi=%.2f; stoch_k=%.2f; above_bb_upper=%t; return_3_session=%.2f%%; volume_ratio=%.2fx weak=%t",
			snapshot.RSI14, snapshot.StochK, snapshot.ReferencePrice > snapshot.BBUpper, snapshot.Return3SessionPct, snapshot.VolumeRatio, weakVolume),
	}
	decision.Rules = []RuleDecision{rule1, rule2, rule3}
	for _, rule := range decision.Rules {
		if rule.Matched {
			decision.Status = "blocked"
			decision.Reason = "anti-chase: " + rule.ID + " combination matched"
			return decision
		}
	}
	decision.Status = "passed"
	return decision
}

func hasMinimumEvidence(s Snapshot) bool {
	return s.CompletedSessions >= MinimumCompletedSessions && positiveFinite(s.ReferencePrice) &&
		positiveFinite(s.EMA20) && positiveFinite(s.ATR14) && positiveFinite(s.VolumeRatio) &&
		finite(s.Return3SessionPct) && finite(s.RSI14) && finite(s.StochK) && positiveFinite(s.BBUpper)
}

func positiveFinite(v float64) bool { return v > 0 && finite(v) }
func finite(v float64) bool         { return !math.IsNaN(v) && !math.IsInf(v, 0) }
