package notify

import (
	"context"
	"fmt"
	"strings"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// SignalApprover can approve or reject signals. Implemented by engine.Engine.
type SignalApprover interface {
	ApproveSignal(ctx context.Context, sigID string) error
	RejectSignal(ctx context.Context, sigID string) error
}

// NewEngineCallbackHandler creates a CallbackHandler that delegates to the engine
// for approve/reject actions and fetches analysis details from the store.
func NewEngineCallbackHandler(approver SignalApprover) CallbackHandler {
	return func(ctx context.Context, signalID, action string) (string, error) {
		switch action {
		case "approve":
			return handleApprove(ctx, approver, signalID)
		case "reject":
			return handleReject(ctx, approver, signalID)
		case "detail":
			return handleDetail(ctx, signalID)
		default:
			return "", fmt.Errorf("unknown action: %s", action)
		}
	}
}

func handleApprove(ctx context.Context, approver SignalApprover, signalID string) (string, error) {
	if err := approver.ApproveSignal(ctx, signalID); err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "expired") || strings.Contains(errStr, "stale") ||
			strings.Contains(errStr, "cannot approve") {
			return "⏰ Signal expired or already processed.", nil
		}
		return "", err
	}
	return "✅ Trade approved and executing.", nil
}

func handleReject(ctx context.Context, approver SignalApprover, signalID string) (string, error) {
	if err := approver.RejectSignal(ctx, signalID); err != nil {
		return "", err
	}
	return "❌ Signal rejected.", nil
}

func handleDetail(ctx context.Context, signalID string) (string, error) {
	sig, err := store.GetSignalByID(ctx, signalID)
	if err != nil {
		return "", fmt.Errorf("get signal: %w", err)
	}
	if sig == nil {
		return "Signal not found.", nil
	}

	var sb strings.Builder
	writeSignalDetailSummary(&sb, sig)

	// Fetch linked analysis reasoning.
	if len(sig.AnalysisIDs) > 0 {
		for _, aID := range sig.AnalysisIDs {
			a, err := store.GetAnalysisByID(ctx, aID)
			if err != nil || a == nil {
				continue
			}
			fmt.Fprintf(&sb, "*%s/%s*\n", a.ModelProvider, a.ModelName)
			fmt.Fprintf(&sb, "%s\n\n", truncate(a.Reasoning, 500))
		}
	}

	return sb.String(), nil
}

func writeSignalDetailSummary(sb *strings.Builder, sig *store.Signal) {
	fmt.Fprintf(sb, "📋 *Signal Detail*\n\n")
	fmt.Fprintf(sb, "Ticker: `%s` | %s\n", sig.Ticker, sig.Action)
	fmt.Fprintf(sb, "Confidence: %.0f%%\n", sig.Confidence*100)
	fmt.Fprintf(sb, "Price: $%.2f\n", sig.SignalPrice)
	fmt.Fprintf(sb, "SL: $%.2f | TP: $%.2f\n", sig.StopLoss, sig.TakeProfit)
	fmt.Fprintf(sb, "Status: %s\n", sig.Status)
	fmt.Fprintf(sb, "Strategy: %s%s\n\n",
		sig.Strategy, entrySetupNotification(sig.EntrySetup, sig.EntrySetupEvidence))
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}
