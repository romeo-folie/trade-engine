package scanner

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// SwingCandidate is the enriched candidate passed to the LLM for scoring.
type SwingCandidate struct {
	Ticker     string
	Price      float64
	ChangePct  float64 // % price change today (from movers); 0 if from most-actives only
	Volume     int64   // today's volume
	TradeCount int64   // number of trades today
	Sources    []string // "mover" | "active" — which screeners surfaced this ticker
	News       []SwingNewsItem
}

// SwingNewsItem is a lightweight news record for prompt injection.
type SwingNewsItem struct {
	Headline    string
	Sentiment   string // "POSITIVE" | "NEUTRAL" | "NEGATIVE"
	PublishedAt time.Time
}

// SwingScoreResult is the structured output returned by the LLM scorer.
type SwingScoreResult struct {
	Action     string   // "add" | "skip"
	Conviction float64  // 0.0–1.0
	SetupType  string   // "momentum_breakout" | "news_catalyst" | "volume_surge" | "oversold_bounce" | "other"
	Strategy   string   // "trend_following" | "momentum" | "mean_reversion" | "news_sentiment"
	Timeframes []string // e.g. ["1D", "4H"]
	Reasoning  string
	Risks      []string
}

// BuildSwingPrompt constructs the system and user prompts for swing-trade candidate scoring.
// minConviction is injected from config so the prompt threshold always matches runtime behaviour.
func BuildSwingPrompt(candidate SwingCandidate, minConviction float64) (systemPrompt, userMessage string) {
	// Clamp to valid range; fall back to a sensible default if misconfigured.
	if minConviction <= 0 || minConviction > 1 || math.IsNaN(minConviction) || math.IsInf(minConviction, 0) {
		minConviction = 0.55
	}
	convictionStr := strconv.FormatFloat(minConviction, 'f', -1, 64)

	systemPrompt = fmt.Sprintf(`You are an expert swing trader and market analyst. Your role is to evaluate intraday market screener candidates and determine whether they are worth adding to a swing-trade watchlist for further analysis.

You MUST respond with ONLY a valid JSON object matching this exact schema:
{
  "action":     "add" | "skip",
  "conviction": <float 0.0-1.0>,
  "setup_type": "momentum_breakout" | "news_catalyst" | "volume_surge" | "oversold_bounce" | "other",
  "strategy":   "trend_following" | "momentum" | "mean_reversion" | "news_sentiment",
  "timeframes": ["1D"] | ["4H", "1D"] | ["1H", "4H"],
  "reasoning":  "<max 200 chars>",
  "risks":      ["<risk1>", "<risk2>"]
}

Rules:
- action "add" when conviction >= %s; "skip" otherwise
- "add" signals: strong news catalyst, unusual volume with price confirmation, momentum continuation above key levels, clear oversold setup with reversal signal
- "skip" signals: no identifiable catalyst, purely noise-driven movement, already extended past reasonable entry, penny stock / very low float risk
- strategy must match the setup:
    - momentum_breakout → "momentum" or "trend_following"
    - news_catalyst → "news_sentiment" or "momentum"
    - volume_surge without directional news → "momentum"
    - oversold_bounce → "mean_reversion"
- timeframes: swing trades generally use "1D" as primary; add "4H" for momentum setups or when entry timing matters
- This is a discovery watchlist — add anything with a credible setup worth monitoring; the trading engine applies its own signal gates before acting.`, convictionStr)

	var sb strings.Builder
	direction := "up"
	if candidate.ChangePct < 0 {
		direction = "down"
	}

	sb.WriteString("## CANDIDATE\n")
	sb.WriteString(fmt.Sprintf("Ticker:       %s\n", candidate.Ticker))
	sb.WriteString(fmt.Sprintf("Price:        %.2f\n", candidate.Price))
	if candidate.ChangePct != 0 {
		sb.WriteString(fmt.Sprintf("Change:       %+.2f%% (%s)\n", candidate.ChangePct, direction))
	}
	if candidate.Volume > 0 {
		sb.WriteString(fmt.Sprintf("Volume:       %d\n", candidate.Volume))
	}
	if candidate.TradeCount > 0 {
		sb.WriteString(fmt.Sprintf("Trade count:  %d\n", candidate.TradeCount))
	}
	sb.WriteString(fmt.Sprintf("Surfaced by:  %s\n", strings.Join(candidate.Sources, ", ")))
	sb.WriteString("\n")

	if len(candidate.News) > 0 {
		sb.WriteString("## RECENT NEWS\n")
		limit := 5
		if len(candidate.News) < limit {
			limit = len(candidate.News)
		}
		for _, n := range candidate.News[:limit] {
			age := time.Since(n.PublishedAt)
			sb.WriteString(fmt.Sprintf("  [%.0fh ago] [%s] %s\n", age.Hours(), n.Sentiment, n.Headline))
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("## RECENT NEWS\nNo relevant news found in the past 24 hours.\n\n")
	}

	sb.WriteString("## TASK\n")
	sb.WriteString(fmt.Sprintf(
		"Evaluate %s as a swing-trade candidate. Determine if it belongs on the watchlist for deeper analysis, and if so, identify the best-fit strategy and timeframes. Return ONLY the JSON object.",
		candidate.Ticker,
	))

	return systemPrompt, sb.String()
}
