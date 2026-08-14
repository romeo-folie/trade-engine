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
		return audit
	}
	if selected == "none" {
		audit.SelectionStatus = LensSelectionNone
		return audit
	}

	// The profile is the strict authority only after it validates. Invalid or
	// absent profiles must retain legacy compatibility rather than turning an
	// operational row into a false-negative selection.
	if req.ExecutionProfile != nil {
		if profile, err := req.ExecutionProfile.Canonicalize(); err == nil {
			allowed := make(map[string]struct{}, len(profile.AdmissibleLenses))
			for _, lens := range profile.AdmissibleLenses {
				allowed[lens] = struct{}{}
			}
			if _, ok := allowed[selected]; !ok {
				response.SelectedStrategy = ""
				// A profile-bound actionable response was reasoned under a lens the
				// scanner explicitly prohibited. Clearing only SelectedStrategy would let
				// downstream entry gates fall back to the legacy configured strategy
				// and potentially execute that invalid recommendation. Preserve the
				// response for audit, but make it non-actionable.
				if response.Action != ActionHold && response.Action != ActionWait {
					response.Action = ActionHold
				}
				audit.SelectionStatus = LensSelectionInvalid
				return audit
			}
		}
	}
	audit.SelectionStatus = LensSelectionSelected
	return audit
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
