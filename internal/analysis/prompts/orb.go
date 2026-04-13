package prompts

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// ORBCandidate mirrors data.GapCandidate for prompt construction,
// keeping the prompts package decoupled from the data package.
type ORBCandidate struct {
	Ticker        string
	GapPct        float64
	PreMarketVol  int64
	PreMarketLast float64
	PriorClose    float64
}

// ORBNewsItem is a lightweight news record for prompt injection.
type ORBNewsItem struct {
	Headline    string
	Sentiment   string
	PublishedAt time.Time
}

// BuildORBPrompt constructs the system and user prompts for ORB catalyst scoring.
func BuildORBPrompt(candidate ORBCandidate, news []ORBNewsItem, orbPerformanceContext string) (systemPrompt, userMessage string) {
	systemPrompt = `You are an expert pre-market stock analyst specializing in gap-and-go momentum plays.

Your task: analyze a pre-market gap candidate and determine whether the catalyst is substantive enough to support an Opening Range Breakout trade.

You MUST respond with ONLY a valid JSON object matching this exact schema:
{
  "action":           "trade" | "skip",
  "confidence":       <float 0.0-1.0>,
  "catalyst_type":    "earnings" | "upgrade" | "filing" | "news" | "unknown",
  "catalyst_quality": "substantive" | "speculative" | "none",
  "reasoning":        "<max 200 chars>",
  "risks":            ["<risk1>", "<risk2>"]
}

Rules:
- action "trade" only when confidence >= 0.65 AND catalyst_quality != "none"
- Strong BUY signals: earnings beat with raised guidance, major analyst upgrade, FDA approval, acquisition announcement
- SKIP signals: no clear catalyst, low-float pump pattern, very thin pre-market volume, purely technical move
- Be conservative — most gap candidates should be skipped

USING HISTORICAL ORB PERFORMANCE (apply only when trade_count >= 5):
- Win rate < 45%: your confidence ceiling is 0.65 for this ticker/session.
- Win rate >= 65%: full confidence range (0.0–1.0) is available.
- Win rate 45%–65%: no constraint — use your normal judgment.
- Negative avg PnL: bias toward "skip" unless the catalyst is clearly substantive.`

	var sb strings.Builder
	direction := "up"
	if candidate.GapPct < 0 {
		direction = "down"
	}
	sb.WriteString("## CANDIDATE\n")
	sb.WriteString(fmt.Sprintf("Ticker:         %s\n", candidate.Ticker))
	sb.WriteString(fmt.Sprintf("Gap:            %+.2f%% (%s)\n", candidate.GapPct, direction))
	sb.WriteString(fmt.Sprintf("Pre-market vol: %d\n", candidate.PreMarketVol))
	sb.WriteString(fmt.Sprintf("Pre-market px:  %.2f\n", candidate.PreMarketLast))
	sb.WriteString(fmt.Sprintf("Prior close:    %.2f\n", candidate.PriorClose))
	sb.WriteString("\n")

	if len(news) > 0 {
		sb.WriteString("## RECENT NEWS\n")
		limit := 8
		if len(news) < limit {
			limit = len(news)
		}
		for _, n := range news[:limit] {
			age := time.Since(n.PublishedAt)
			sb.WriteString(fmt.Sprintf("  [%.0fh ago] [%s] %s\n", age.Hours(), n.Sentiment, n.Headline))
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("## RECENT NEWS\nNo relevant news found in the past 24 hours.\n\n")
	}

	if orbPerformanceContext != "" {
		sb.WriteString("## HISTORICAL ORB PERFORMANCE\n")
		sb.WriteString(orbPerformanceContext)
		sb.WriteString("\n")
	}

	sb.WriteString("## TASK\n")
	sb.WriteString(fmt.Sprintf(
		"%s is gapping %s %.2f%% pre-market. Assess the catalyst quality and determine if this is a viable ORB candidate. Return ONLY the JSON object.",
		candidate.Ticker, direction, math.Abs(candidate.GapPct),
	))

	return systemPrompt, sb.String()
}
