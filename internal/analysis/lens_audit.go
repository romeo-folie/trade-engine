package analysis

import "strings"

// LensSelectionStatus makes the difference between omitted output, an
// intentional no-fit answer, and ensemble disagreement queryable without
// parsing raw model JSON.
type LensSelectionStatus string

const (
	LensSelectionAbsent      LensSelectionStatus = "absent"
	LensSelectionNone        LensSelectionStatus = "none"
	LensSelectionSelected    LensSelectionStatus = "selected"
	LensSelectionInvalid     LensSelectionStatus = "invalid"
	LensSelectionNoConsensus LensSelectionStatus = "no_consensus"
)

// LensSelectionAudit is the durable, validated summary of a final response.
// SelectedLens preserves an invalid model value for investigation while an
// actionable response is suppressed before it can affect downstream consumers.
type LensSelectionAudit struct {
	SelectedLens    string
	RejectedLenses  map[string]string
	Regime          string
	SelectionStatus LensSelectionStatus
}

// ValidateFinalLensSelection validates an aggregated outcome against the
// scanner-owned execution profile when present. Legacy rows deliberately keep
// their existing strategy semantics; profile-bound rows may only select a lens
// the scanner explicitly offered. Raw model output remains in ModelCallMeta.
func ValidateFinalLensSelection(
	response *AnalysisResponse,
	req AnalysisRequest,
	aggregated bool,
) LensSelectionAudit {
	// The profile is a strict authority only after it validates. Invalid or
	// absent profiles retain legacy compatibility rather than turning an
	// operational row into a false-negative selection.
	var allowed map[string]struct{}
	if req.ExecutionProfile != nil {
		if profile, err := req.ExecutionProfile.Canonicalize(); err == nil {
			allowed = make(map[string]struct{}, len(profile.AdmissibleLenses))
			for _, lens := range profile.AdmissibleLenses {
				allowed[lens] = struct{}{}
			}
		}
	}

	selected := strings.ToLower(strings.TrimSpace(response.SelectedStrategy))
	audit := LensSelectionAudit{
		SelectedLens:    selected,
		RejectedLenses:  cloneRejectedLenses(response.RejectedStrategies),
		Regime:          strings.ToLower(strings.TrimSpace(response.Regime)),
		SelectionStatus: LensSelectionAbsent,
	}
	if selected == "" {
		if aggregated {
			audit.SelectionStatus = LensSelectionNoConsensus
		}
		suppressProfileBoundAction(response, allowed)
		return audit
	}
	if selected == "none" {
		audit.SelectionStatus = LensSelectionNone
		suppressProfileBoundAction(response, allowed)
		return audit
	}

	if allowed != nil {
		if _, ok := allowed[selected]; !ok {
			response.SelectedStrategy = ""
			// A profile-bound actionable response was reasoned under a lens the
			// scanner explicitly prohibited. Clearing only SelectedStrategy would let
			// downstream entry gates fall back to the legacy configured strategy
			// and potentially execute that invalid recommendation. Preserve the
			// response for audit, but make it non-actionable.
			suppressProfileBoundAction(response, allowed)
			audit.SelectionStatus = LensSelectionInvalid
			return audit
		}
		if response.Action == ActionBuy && !hasCompleteRejectionCoverage(response.RejectedStrategies, selected, allowed) {
			// A profile offers a bounded decision set, not a menu from which the
			// model may silently omit inconvenient alternatives. A BUY is valid
			// only when every non-selected lens was explicitly rejected and no
			// other lens was introduced. Preserve the original selection and map
			// in the audit record, but do not let this incomplete decision reach
			// downstream entry handling.
			response.SelectedStrategy = ""
			suppressProfileBoundAction(response, allowed)
			audit.SelectionStatus = LensSelectionInvalid
			return audit
		}
	}
	audit.SelectionStatus = LensSelectionSelected
	return audit
}

// suppressProfileBoundAction prevents a scanner-profiled BUY without a valid
// selected lens from reaching a legacy configured-strategy fallback. A lens is
// the authorisation for a new entry on those rows; "none" or no consensus is
// explicitly not such an authorisation. SELL decisions remain intact: exits
// must never depend on a scanner execution lens selection.
func suppressProfileBoundAction(response *AnalysisResponse, allowed map[string]struct{}) {
	if allowed == nil {
		return
	}
	if response.Action == ActionBuy {
		response.Action = ActionHold
		// A HOLD cannot carry entry sizing or bracket prices. This function runs
		// after JSON extraction, so it must re-establish the same invariant the
		// parser applies to model-originated HOLD responses.
		response.SizePct = 0
		response.StopLoss = 0
		response.TakeProfit = 0
	}
}

// hasCompleteRejectionCoverage verifies the rejection side of a selected
// profile-bound BUY. It intentionally canonicalizes the model's keys here as
// a defence for callers that construct AnalysisResponse directly rather than
// through extractAnalysisJSON, which normally does that normalization.
func hasCompleteRejectionCoverage(rejected map[string]string, selected string, allowed map[string]struct{}) bool {
	if len(rejected) != len(allowed)-1 {
		return false
	}

	seen := make(map[string]struct{}, len(rejected))
	for lens := range rejected {
		lens = strings.ToLower(strings.TrimSpace(lens))
		if lens == "" || lens == selected {
			return false
		}
		if _, ok := allowed[lens]; !ok {
			return false
		}
		if _, duplicate := seen[lens]; duplicate {
			return false
		}
		seen[lens] = struct{}{}
	}

	for lens := range allowed {
		if lens == selected {
			continue
		}
		if _, rejected := seen[lens]; !rejected {
			return false
		}
	}
	return true
}

func cloneRejectedLenses(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for lens, reason := range in {
		lens = strings.ToLower(strings.TrimSpace(lens))
		if lens == "" {
			continue
		}
		out[lens] = strings.TrimSpace(reason)
	}
	return out
}
