package engine

import (
	"fmt"
	"math"
	"strings"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

const profileConfirmationWaitMinConfidence = 0.70

// timeframeWorkflow gives each analysis timeframe a stable role. Profile-owned
// workflows use the scanner contract's thesis timeframe first and reserve an
// optional distinct timeframe for entry confirmation. Legacy rows retain their
// current ordered-timeframe behaviour until they receive a valid profile.
//
// This is deliberately a pure planning type. The execution of a second pass
// and combination of its result are introduced separately, so defining the
// contract cannot change live analysis scheduling by itself.
type timeframeWorkflow struct {
	PrimaryTimeframe     data.Timeframe
	EntryTimingTimeframe data.Timeframe
	FetchTimeframes      []string
	ProfileOwned         bool
}

// resolveTimeframeWorkflow prefers a valid execution profile over the legacy
// watchlist timeframe list. Invalid or absent profiles deliberately fall back
// to the exact legacy normalization and primary-selection rules, preserving
// existing rows while profile migrations are rolled out.
func resolveTimeframeWorkflow(legacyTimeframes []string, profile *tradeprofile.ExecutionProfile) timeframeWorkflow {
	if profile != nil {
		if canonical, err := profile.Canonicalize(); err == nil {
			primary := profileTimeframe(canonical.PrimaryTimeframe)
			fetchTimeframes := []string{string(primary)}
			entryTiming := profileTimeframe(canonical.EntryTimingTimeframe)
			if entryTiming != "" {
				fetchTimeframes = append(fetchTimeframes, string(entryTiming))
			}
			return timeframeWorkflow{
				PrimaryTimeframe:     primary,
				EntryTimingTimeframe: entryTiming,
				FetchTimeframes:      fetchTimeframes,
				ProfileOwned:         true,
			}
		}
	}

	requested := stockTimeframesForEntry(legacyTimeframes, false)
	return timeframeWorkflow{
		PrimaryTimeframe: primaryStockTimeframe(requested),
		FetchTimeframes:  requested,
	}
}

func profileTimeframe(value string) data.Timeframe {
	return data.Timeframe(strings.ToLower(strings.TrimSpace(value)))
}

func profileMomentumPlay(timeframe, source string, profile *tradeprofile.ExecutionProfile) bool {
	if profile != nil {
		if canonical, err := profile.Canonicalize(); err == nil {
			return canonical.PrimaryTimeframe == "4H" || canonical.PrimaryTimeframe == "1H"
		}
	}
	return source == "scanner"
}

func populatePrimaryWorkflowAudit(workflow *store.TimeframeWorkflowAudit, resp analysis.AnalysisResponse, audit analysis.LensSelectionAudit) {
	workflow.Primary.Action = string(resp.Action)
	workflow.Primary.Confidence = resp.Confidence
	workflow.Primary.SelectedLens = audit.SelectedLens
}

func signalEvidenceAfterConfirmation(primary []data.Candle, primaryTimeframe string, confirmation []data.Candle, confirmationTimeframe, confirmationStatus string, finalAction analysis.Action) ([]data.Candle, string) {
	if confirmationStatus == "confirmed" && finalAction == analysis.ActionBuy && len(confirmation) > 0 {
		return confirmation, confirmationTimeframe
	}
	return primary, primaryTimeframe
}

func profileConfirmationEligible(resp analysis.AnalysisResponse, audit analysis.LensSelectionAudit, admissible []string) bool {
	if audit.SelectionStatus != analysis.LensSelectionSelected || audit.SelectedLens == "" {
		return false
	}
	return resp.Action == analysis.ActionBuy ||
		(resp.Action == analysis.ActionWait && resp.Confidence >= profileConfirmationWaitMinConfidence && completeWaitRejections(resp, audit.SelectedLens, admissible))
}

// profileConfirmationAllowed prevents a later timing pass from recreating a
// BUY that the specialist authority policy already downgraded on the primary
// thesis. The primary result and its applied audit remain authoritative.
func profileConfirmationAllowed(resp analysis.AnalysisResponse, audit analysis.LensSelectionAudit, admissible []string, policy *analysis.DecisionPolicyAudit) bool {
	return decisionPolicyAllowsReArbitration(policy) && profileConfirmationEligible(resp, audit, admissible)
}

func completeWaitRejections(resp analysis.AnalysisResponse, selected string, admissible []string) bool {
	if len(resp.RejectedStrategies) != len(admissible)-1 {
		return false
	}
	reasons := make(map[string]string, len(resp.RejectedStrategies))
	for lens, reason := range resp.RejectedStrategies {
		lens = strings.ToLower(strings.TrimSpace(lens))
		if _, exists := reasons[lens]; exists {
			return false
		}
		reasons[lens] = reason
	}
	for _, lens := range admissible {
		if lens == selected {
			continue
		}
		if strings.TrimSpace(reasons[lens]) == "" {
			return false
		}
	}
	return true
}

func mergeProfileConfirmation(primary, confirmation analysis.AnalysisResponse, primaryAudit, confirmationAudit analysis.LensSelectionAudit, admissible []string, available bool) analysis.AnalysisResponse {
	if !profileConfirmationEligible(primary, primaryAudit, admissible) {
		return primary
	}
	confirmed := profileConfirmationConfirmed(confirmation, primaryAudit, confirmationAudit, available)
	if !confirmed {
		primary.Action = analysis.ActionWait
		primary.SizePct, primary.StopLoss, primary.TakeProfit = 0, 0, 0
		primary.Reasoning = fmt.Sprintf("[PRIMARY %s]\n%s\n\n[ENTRY CONFIRMATION]\nTiming was not confirmed; retry on the next analysis cycle.", primaryAudit.Regime, primary.Reasoning)
		return primary
	}
	primary.Action = analysis.ActionBuy
	primary.Confidence = math.Min(primary.Confidence, confirmation.Confidence)
	if primary.SizePct > 0 {
		primary.SizePct = math.Min(primary.SizePct, confirmation.SizePct)
	} else {
		primary.SizePct = confirmation.SizePct
	}
	primary.StopLoss = confirmation.StopLoss
	primary.TakeProfit = confirmation.TakeProfit
	primary.Reasoning = fmt.Sprintf("[PRIMARY]\n%s\n\n[ENTRY CONFIRMATION]\n%s", primary.Reasoning, confirmation.Reasoning)
	return primary
}

// decisionPolicyAfterConfirmation keeps the policy audit belonging to the
// decision that determined the final entry outcome. A profile confirmation can
// independently downgrade an otherwise valid primary BUY, so its deterministic
// authority audit is more relevant than the primary pass's audit.
func decisionPolicyAfterConfirmation(primary, confirmation *analysis.DecisionPolicyAudit) *analysis.DecisionPolicyAudit {
	if confirmation != nil {
		return confirmation
	}
	return primary
}

// decisionPolicyAllowsReArbitration prevents a deterministic specialist BUY
// authority downgrade from being replaced by a later arbiter pass that still
// sees the risk reviewer's original unsupported BUY.
func decisionPolicyAllowsReArbitration(policy *analysis.DecisionPolicyAudit) bool {
	return policy == nil || !policy.Applied
}

func profileConfirmationConfirmed(confirmation analysis.AnalysisResponse, primaryAudit, confirmationAudit analysis.LensSelectionAudit, available bool) bool {
	return available && confirmation.Action == analysis.ActionBuy &&
		confirmationAudit.SelectionStatus == analysis.LensSelectionSelected &&
		confirmationAudit.SelectedLens == primaryAudit.SelectedLens &&
		confirmation.SizePct > 0 && confirmation.StopLoss > 0 && confirmation.TakeProfit > 0
}
