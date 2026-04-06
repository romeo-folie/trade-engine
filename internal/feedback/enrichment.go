package feedback

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// GetPerformanceContext returns a formatted text block summarising recent
// instrument and model performance for use in LLM prompts.
// Returns an empty string when no data is available.
func GetPerformanceContext(ctx context.Context, ticker string) (string, error) {
	// Instrument-level insights for this ticker (last 30 days, up to 5 rows).
	instrInsights, err := store.GetFeedbackInsights(ctx, "instrument", ticker, "30d", 5)
	if err != nil {
		return "", fmt.Errorf("feedback: GetFeedbackInsights instrument %s: %w", ticker, err)
	}

	// All model-level insights for the latest date (30d window).
	today := time.Now().UTC().Truncate(24 * time.Hour)
	modelInsights, err := store.GetLatestFeedbackInsights(ctx, "model", "30d", today)
	if err != nil {
		return "", fmt.Errorf("feedback: GetLatestFeedbackInsights model: %w", err)
	}

	if len(instrInsights) == 0 && len(modelInsights) == 0 {
		return "", nil
	}

	var sb strings.Builder

	// ── Instrument block ────────────────────────────────────────────────────
	// Collect the most-recent metrics for this ticker from instrInsights.
	instrMetrics := make(map[string]float64)
	instrTradeCount := 0
	for _, ins := range instrInsights {
		if _, seen := instrMetrics[ins.Metric]; !seen {
			instrMetrics[ins.Metric] = ins.Value
			if ins.Metric == "trade_count" {
				instrTradeCount = ins.TradeCount
			}
		}
	}
	if len(instrMetrics) > 0 {
		winRate, hasWR := instrMetrics["win_rate"]
		avgPnl, hasPnl := instrMetrics["avg_pnl_pct"]
		tc := instrTradeCount

		var parts []string
		if tc > 0 {
			parts = append(parts, fmt.Sprintf("%d trades in last 30 days", tc))
		}
		if hasWR {
			parts = append(parts, fmt.Sprintf("%.0f%% win rate", winRate*100))
		}
		if hasPnl {
			sign := "+"
			if avgPnl < 0 {
				sign = ""
			}
			parts = append(parts, fmt.Sprintf("avg %s%.1f%%", sign, avgPnl*100))
		}
		if len(parts) > 0 {
			fmt.Fprintf(&sb, "- %s: %s\n", ticker, strings.Join(parts, ", "))
		}
	}

	// ── Model block ──────────────────────────────────────────────────────────
	// Group model insights: scopeKey → metric → value.
	type modelStat struct {
		winRate    float64
		tradeCount int
		hasWR      bool
	}
	modelMap := make(map[string]*modelStat)
	for _, ins := range modelInsights {
		if modelMap[ins.ScopeKey] == nil {
			modelMap[ins.ScopeKey] = &modelStat{}
		}
		ms := modelMap[ins.ScopeKey]
		switch ins.Metric {
		case "win_rate":
			ms.winRate = ins.Value
			ms.hasWR = true
		case "trade_count":
			ms.tradeCount = ins.TradeCount
		}
	}
	for modelKey, ms := range modelMap {
		if !ms.hasWR {
			continue
		}
		if ms.tradeCount > 0 {
			fmt.Fprintf(&sb, "- %s: %.0f%% win rate on %d trades\n",
				modelKey, ms.winRate*100, ms.tradeCount)
		} else {
			fmt.Fprintf(&sb, "- %s: %.0f%% win rate\n", modelKey, ms.winRate*100)
		}
	}

	return sb.String(), nil
}

// GetORBPerformanceContext returns a formatted text block summarising recent
// ORB session performance for use in LLM prompts.
// Returns an empty string when no data is available.
func GetORBPerformanceContext(ctx context.Context, ticker, session string) (string, error) {
	scopeKey := ticker
	if session != "" {
		scopeKey = ticker + ":" + session
	}

	insights, err := store.GetFeedbackInsights(ctx, "orb_session", scopeKey, "30d", 5)
	if err != nil {
		return "", fmt.Errorf("feedback: GetFeedbackInsights orb_session %s: %w", scopeKey, err)
	}
	if len(insights) == 0 {
		return "", nil
	}

	// Extract the most-recent value for each metric.
	metrics := make(map[string]float64)
	tradeCount := 0
	for _, ins := range insights {
		if _, seen := metrics[ins.Metric]; !seen {
			metrics[ins.Metric] = ins.Value
			if ins.Metric == "trade_count" {
				tradeCount = ins.TradeCount
			}
		}
	}

	winRate, hasWR := metrics["win_rate"]
	avgPnl, hasPnl := metrics["avg_pnl_pct"]

	if !hasWR && !hasPnl {
		return "", nil
	}

	// Build the label: "EUR/USD London ORB" or just "EUR/USD ORB".
	label := ticker
	if session != "" {
		// Capitalise the session name for display.
		label = ticker + " " + strings.Title(session) //nolint:staticcheck
	}
	label += " ORB"

	var sb strings.Builder
	if hasWR && tradeCount > 0 {
		wins := int(float64(tradeCount) * winRate)
		losses := tradeCount - wins
		fmt.Fprintf(&sb, "%s: %dW / %dL (%.0f%%)", label, wins, losses, winRate*100)
	} else if hasWR {
		fmt.Fprintf(&sb, "%s: %.0f%% win rate", label, winRate*100)
	} else {
		fmt.Fprintf(&sb, "%s:", label)
	}

	if hasPnl {
		sign := "+"
		if avgPnl < 0 {
			sign = ""
		}
		fmt.Fprintf(&sb, ", avg PnL: %s%.1f%%", sign, avgPnl*100)
	}
	sb.WriteString("\n")

	return sb.String(), nil
}
