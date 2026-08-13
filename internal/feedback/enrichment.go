package feedback

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// titleCase uppercases the first byte of s.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// GetPerformanceContext returns a formatted text block summarising recent
// instrument and model performance for use in LLM prompts.
// Returns an empty string when no data is available.
func GetPerformanceContext(ctx context.Context, ticker string) (string, error) {
	if !store.IsConnected() {
		return "", fmt.Errorf("feedback: database not connected")
	}
	// Instrument-level insights for this ticker (last 30 days, up to 5 rows).
	instrInsights, err := store.GetFeedbackInsights(ctx, "instrument", ticker, "30d", 5)
	if err != nil {
		return "", fmt.Errorf("feedback: GetFeedbackInsights instrument %s: %w", ticker, err)
	}

	// All model-level insights for the latest date (30d window).
	// Search backwards up to 7 days to find the most recent date with data.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var modelInsights []store.FeedbackInsight
	for daysBack := 0; daysBack < 7; daysBack++ {
		insightDate := today.AddDate(0, 0, -daysBack)
		modelInsights, err = store.GetLatestFeedbackInsights(ctx, "model", "30d", insightDate)
		if err != nil {
			return "", fmt.Errorf("feedback: model insights: %w", err)
		}
		if len(modelInsights) > 0 {
			break
		}
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
			parts = append(parts, fmt.Sprintf("avg %s%.1f%%", sign, avgPnl))
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
