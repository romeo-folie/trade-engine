package prompts

import (
	"fmt"
	"sort"
	"strings"
)

// TimeframeRole identifies whether an analysis establishes the trade thesis or
// only checks timing for an already-established thesis. It is intentionally a
// prompt-safe contract shared by every provider and specialist role.
type TimeframeRole string

const (
	TimeframeRolePrimaryThesis     TimeframeRole = "primary_thesis"
	TimeframeRoleEntryConfirmation TimeframeRole = "entry_confirmation"
)

// PrimaryThesisContext is the immutable result of the primary-timeframe pass
// supplied to an optional entry-confirmation pass. The confirmation pass may
// assess entry timing, but must not replace this lens or thesis or create an
// independent exit decision.
type PrimaryThesisContext struct {
	PrimaryTimeframe     string
	Action               string
	Confidence           float64
	SelectedLens         string
	Regime               string
	RejectedLenses       map[string]string
	Reasoning            string
	StopLoss             float64
	TakeProfit           float64
	EntryTimingTimeframe string
}

func normalizeTimeframeRole(role TimeframeRole) TimeframeRole {
	if role == TimeframeRoleEntryConfirmation {
		return role
	}
	return TimeframeRolePrimaryThesis
}

func timeframeRoleInstruction(role TimeframeRole) string {
	if normalizeTimeframeRole(role) != TimeframeRoleEntryConfirmation {
		return ""
	}
	return `

TIMEFRAME ROLE — ENTRY CONFIRMATION:
- This pass checks entry timing only against the supplied primary thesis.
- For BUY, HOLD, and WAIT, retain the primary selected lens; do not substitute a different lens or construct a new thesis.
- Do not emit SELL from this timing-only pass. Use WAIT or HOLD when timing does not confirm the primary thesis.
`
}

// TimeframeRoleInstruction exposes the role-specific system constraint to
// custom prompt paths such as ensemble arbitration.
func TimeframeRoleInstruction(role TimeframeRole) string {
	return timeframeRoleInstruction(role)
}

// RenderTimeframeWorkflow exposes the structured workflow context to custom
// user-message paths that bypass the standard stock prompt builder.
func RenderTimeframeWorkflow(role TimeframeRole, context *PrimaryThesisContext) string {
	var b strings.Builder
	writePrimaryThesisContext(&b, role, context)
	return b.String()
}

// writePrimaryThesisContext renders the first pass as structured evidence. It
// deliberately does not make its BUY outcome an order: deterministic entry and
// risk controls remain authoritative after analysis.
func writePrimaryThesisContext(b *strings.Builder, role TimeframeRole, context *PrimaryThesisContext) {
	role = normalizeTimeframeRole(role)
	b.WriteString("### TIMEFRAME WORKFLOW\n")
	b.WriteString("Role: " + string(role) + "\n")
	if role == TimeframeRolePrimaryThesis {
		b.WriteString("Establish the trade thesis on this primary timeframe.\n\n")
		return
	}
	b.WriteString("Assess entry timing for the supplied thesis; do not replace its selected lens and do not emit an independent SELL.\n")
	if context == nil {
		b.WriteString("Primary thesis context: not available; do not infer a replacement thesis.\n\n")
		return
	}

	b.WriteString("\n### PRIMARY THESIS CONTEXT\n")
	b.WriteString(fmt.Sprintf("Primary timeframe: %s | entry timing timeframe: %s\n",
		valueOrUnknown(context.PrimaryTimeframe), valueOrUnknown(context.EntryTimingTimeframe)))
	b.WriteString(fmt.Sprintf("Action: %s | confidence: %.2f | selected lens: %s | regime: %s\n",
		valueOrUnknown(context.Action), context.Confidence, valueOrUnknown(context.SelectedLens), valueOrUnknown(context.Regime)))
	b.WriteString(fmt.Sprintf("Proposed stop / target: %s / %s\n",
		formatPromptPrice(context.StopLoss), formatPromptPrice(context.TakeProfit)))
	b.WriteString("Reasoning: " + valueOrUnknown(context.Reasoning) + "\n")
	if len(context.RejectedLenses) > 0 {
		keys := make([]string, 0, len(context.RejectedLenses))
		for lens := range context.RejectedLenses {
			keys = append(keys, lens)
		}
		sort.Strings(keys)
		for _, lens := range keys {
			b.WriteString(fmt.Sprintf("Rejected lens[%s]: %s\n", lens, valueOrUnknown(context.RejectedLenses[lens])))
		}
	}
	b.WriteString("\n")
}
