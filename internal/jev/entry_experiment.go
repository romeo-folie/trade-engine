package jev

// This experiment is deliberately separate from Evaluator: the engine cannot
// enable it accidentally. The replay command compares it with the exact
// recorded request while keeping the policy thresholds unchanged.
import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
)

const EntryExperimentVersion = "jev-entry-evidence/v1"

type CandidatePlan struct {
	Side              string    `json:"side"`
	Timeframe         string    `json:"timeframe"`
	Entry             float64   `json:"entry"`
	Stop              float64   `json:"stop"`
	Target            float64   `json:"target"`
	RiskReward        float64   `json:"risk_reward"`
	Status            string    `json:"status"`
	Reason            string    `json:"reason,omitempty"`
	PriceTimestamp    time.Time `json:"price_timestamp"`
	CurrentMarkStatus string    `json:"current_mark_status"`
}

type EntryExperimentState struct {
	State
	Version                 string          `json:"experiment_version"`
	ExecutionTimeframe      string          `json:"execution_timeframe"`
	TimingEvidenceTimeframe string          `json:"timing_evidence_timeframe"`
	CandidatePlans          []CandidatePlan `json:"candidate_plans"`
}

// EntryExperiment adds evidence without fetching data, altering the execution
// timeframe, inventing support levels, or overriding an existing WAIT answer.
func EntryExperiment(req analysis.AnalysisRequest, cfg PolicyConfig, minRR float64, opts Options) (EntryExperimentState, map[string]Question, error) {
	if err := cfg.Validate(); err != nil {
		return EntryExperimentState{}, nil, err
	}
	if opts.Shorts {
		if err := cfg.ValidateShorts(); err != nil {
			return EntryExperimentState{}, nil, err
		}
	}
	if minRR < 0 || math.IsNaN(minRR) || math.IsInf(minRR, 0) {
		return EntryExperimentState{}, nil, fmt.Errorf("invalid risk/reward floor")
	}
	s := EntryExperimentState{State: BuildStateWith(req, opts), Version: EntryExperimentVersion, ExecutionTimeframe: req.Timeframe, TimingEvidenceTimeframe: req.Timeframe}
	// An explicit profile-owned role wins. Otherwise identify the shortest
	// available supporting timeframe as timing evidence, without relabeling it
	// as the execution timeframe or changing the policy's entry-price source.
	for _, c := range req.TimeframeContexts {
		if c.Role == "entry_timing" && len(c.RecentCandles) > 0 {
			s.ExecutionTimeframe, s.TimingEvidenceTimeframe = c.Timeframe, c.Timeframe
			break
		}
		if len(c.RecentCandles) > 0 && timeframeMinutes(c.Timeframe) < timeframeMinutes(s.TimingEvidenceTimeframe) {
			s.TimingEvidenceTimeframe = c.Timeframe
		}
	}
	qs := QuestionsWith(req, opts)
	if HoldsLong(req) || HoldsShort(req) || ExitOnly(req) {
		return s, qs, nil
	}
	entry, atr, r := entryPrice(req), req.Indicators.ATR, math.Max(cfg.TargetRMultiple, minRR)
	sides := []string{"long"}
	if opts.Shorts {
		sides = append(sides, "short")
	}
	for _, side := range sides {
		plan := CandidatePlan{Side: side, Timeframe: s.ExecutionTimeframe, Status: "unavailable", CurrentMarkStatus: "unknown"}
		if len(req.RecentCandles) > 0 {
			plan.PriceTimestamp = req.RecentCandles[0].Timestamp
		}
		for _, c := range req.TimeframeContexts {
			if c.Role == "entry_timing" && len(c.RecentCandles) > 0 {
				plan.PriceTimestamp = c.RecentCandles[0].Timestamp
				break
			}
		}
		if timing := req.Indicators.TimingSnapshot; timing != nil {
			plan.CurrentMarkStatus = timing.EvidenceStatus
		}
		if entry > 0 && atr > 0 && !math.IsInf(entry, 0) && !math.IsInf(atr, 0) {
			stop := round4(entry - cfg.StopATRMultiple*atr)
			if side == "short" {
				stop = ceil4(entry + cfg.StopATRMultiple*atr)
			}
			target := 0.0
			if side == "long" && stop > 0 && stop < entry {
				target = bracketTarget(entry, stop, r)
			}
			if side == "short" && stop > entry {
				target = shortBracketTarget(entry, stop, r)
			}
			if target > 0 && !math.IsInf(target, 0) && !math.IsInf(stop, 0) {
				plan.Entry, plan.Stop, plan.Target, plan.RiskReward, plan.Status = entry, stop, target, r, "mechanically_valid"
			}
		}
		if plan.Status == "unavailable" {
			plan.Reason = "entry price, ATR, or valid positive bracket unavailable in frozen inputs"
		}
		s.CandidatePlans = append(s.CandidatePlans, plan)
		timingKey, blockerKey := QTiming, "entry_blocker"
		if side == "short" {
			timingKey, blockerKey = QShortTiming, "short_entry_blocker"
		}
		q := qs[timingKey]
		q.Instructions = fmt.Sprintf("Assess a %s entry using candidate_plans, execution_timeframe and timing_evidence_timeframe. The ATR bracket is a mechanical candidate, not proof of a good entry. Use only frozen evidence. A suitable entry need not be perfect; choose wait only for an observed concrete blocker. If necessary evidence is missing, choose insufficient_evidence. Do not assume a catalyst, support level, or confirmation absent from the state.", side)
		qs[timingKey] = q
		qs[blockerKey] = Question{Type: TypeChoice, Instructions: "Identify the primary observed timing blocker for the " + side + " entry. This answer is recorded for diagnosis, not used to bypass risk gates.", Criteria: map[string]string{
			"none": "No observed timing blocker.", "extension": "Price is excessively extended for this entry.", "event": "An identified imminent event blocks entry.", "confirmation": "The supplied strategy evidence shows a required confirmation is not met.", "support_resistance": "A named price level in the state leaves an unsuitable entry.", "missing_data": "The state lacks necessary current market evidence or a valid candidate plan.",
		}}
	}
	return s, qs, nil
}

func timeframeMinutes(tf string) int {
	switch strings.ToLower(tf) {
	case "1m":
		return 1
	case "5m":
		return 5
	case "15m":
		return 15
	case "1h":
		return 60
	case "4h":
		return 240
	case "1d":
		return 1440
	}
	return int(^uint(0) >> 1)
}
