package pullback

import "time"

// PromptContext is the prompt-safe projection of a deterministic daily
// pullback evaluation. It deliberately contains facts and evaluator outcomes,
// never an LLM-generated conclusion. Execution remains governed by Result and
// the entry gate; this context only gives an analyst the facts needed to
// assess whether pullback_strength is the fitting lens.
type PromptContext struct {
	Qualified   bool
	FailedCheck string
	Reason      string

	SignalSession time.Time
	SignalPrice   float64
	SessionOpen   float64
	SessionLow    float64

	Return1M float64
	Return3M float64
	Return6M float64
	EMA9     float64
	EMA21    float64

	AVWAPHigh            float64
	AVWAPHighAnchor      time.Time
	AVWAPLow             float64
	AVWAPLowAnchor       time.Time
	AVWAPDefinition      string
	HorizontalSupport    float64
	SupportTolerancePct  float64
	StackedAVWAP         float64
	StackedCompanion     float64
	StackedCompanionKind string
	Support              float64

	ConsolidationSessions      int
	ConsolidationStart         time.Time
	ConsolidationEnd           time.Time
	ConsolidationHigherLow     bool
	ConsolidationRangeNarrower bool
	EarlyRangePct              float64
	RecentRangePct             float64
	FlushedIntoSupport         bool
	PositiveReclaim            bool

	StopLoss              float64
	StopWidthPct          float64
	MaxStopWidthPct       float64
	ExecutionInvalidation string
}

// PromptContext returns the stable, model-safe view of a completed evaluation.
// Keeping this conversion beside Result prevents prompt code from reimplementing
// AVWAP, support, or qualification logic.
func (r Result) PromptContext() PromptContext {
	return PromptContext{
		Qualified: r.Eligible, FailedCheck: r.FailedCheck, Reason: r.Reason,
		SignalSession: r.SignalSession, SignalPrice: r.SignalPrice,
		SessionOpen: r.SessionOpen, SessionLow: r.SessionLow,
		Return1M: r.Return1M, Return3M: r.Return3M, Return6M: r.Return6M,
		EMA9: r.EMA9, EMA21: r.EMA21,
		AVWAPHigh: r.AVWAPHigh, AVWAPHighAnchor: r.AVWAPHighAnchor,
		AVWAPLow: r.AVWAPLow, AVWAPLowAnchor: r.AVWAPLowAnchor,
		AVWAPDefinition:   r.AVWAPDefinition,
		HorizontalSupport: r.HorizontalSupport, SupportTolerancePct: r.SupportTolerancePct,
		StackedAVWAP: r.StackedAVWAP, StackedCompanion: r.StackedCompanion,
		StackedCompanionKind: r.StackedCompanionKind, Support: r.Support,
		ConsolidationSessions: r.ConsolidationSessions,
		ConsolidationStart:    r.ConsolidationStart, ConsolidationEnd: r.ConsolidationEnd,
		ConsolidationHigherLow:     r.ConsolidationHigherLow,
		ConsolidationRangeNarrower: r.ConsolidationRangeNarrower,
		EarlyRangePct:              r.EarlyRangePct, RecentRangePct: r.RecentRangePct,
		FlushedIntoSupport: r.FlushedIntoSupport, PositiveReclaim: r.PositiveReclaim,
		StopLoss: r.StopLoss, StopWidthPct: r.StopWidthPct,
		MaxStopWidthPct: r.MaxStopWidthPct, ExecutionInvalidation: r.ExecutionInvalidation,
	}
}
