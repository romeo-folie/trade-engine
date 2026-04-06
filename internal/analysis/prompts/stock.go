// Package prompts builds structured LLM prompts for stock analysis.
// PRD §8.1 — prompt structure.
package prompts

import (
	"fmt"
	"strings"
	"time"
)

// ─── Input types (decoupled from data/store packages) ────────────────────────

// Candle is a single OHLCV bar.
type Candle struct {
	Timestamp              time.Time
	Open, High, Low, Close float64
	Volume                 int64
	VWAP                   float64
}

// Indicators holds all computed technical indicator values.
type Indicators struct {
	EMA20, EMA50, EMA200                float64
	SMA20, SMA50, SMA200                float64
	RSI                                 float64
	MACDLine, MACDSignal, MACDHistogram float64
	BBUpper, BBMiddle, BBLower          float64
	ADX, ATR, OBV                       float64
	StochK, StochD                      float64
	VWAP, VolSMA20, VolRatio            float64
}

// NewsItem is a compressed news entry.
type NewsItem struct {
	Headline    string
	Source      string
	PublishedAt time.Time
	Sentiment   string
}

// Portfolio carries current portfolio state.
type Portfolio struct {
	CashAvailable float64
	OpenPositions []Position
}

// Position is a condensed open position.
type Position struct {
	Ticker, Sentiment                                  string
	Quantity, AvgCostBasis, CurrentValue, UnrealizedPL float64
}

// SimilarPastSetup represents a past analysis included in the "Similar Past
// Analyses" prompt section.  Nil TradeOutcomePct means no closed trade is linked.
type SimilarPastSetup struct {
	Ticker          string
	Action          string
	Confidence      float64
	Reasoning       string
	CreatedAt       string
	TradeOutcomePct *float64
}

// ─── Prompt builder ───────────────────────────────────────────────────────────

// BuildStockPrompt returns (systemPrompt, userMessage) for a stock analysis request.
// PRD §8.1 sections in order:
//  1. System prompt (role, output schema, rules)
//  2. Market data (recent OHLCV)
//  3. Technical indicators
//  4. Portfolio context
//  5. Recent news
//  6. Knowledge graph (placeholder)
//  7. Similar past analyses (placeholder)
//  8. Performance context (feedback pipeline)
func BuildStockPrompt(
	ticker, strategy, timeframe string,
	candles []Candle,
	ind Indicators,
	news []NewsItem,
	portfolio Portfolio,
	knowledgeContext string,
	similarSetups []SimilarPastSetup,
	performanceContext string,
) (systemPrompt, userMessage string) {
	systemPrompt = buildSystem(strategy)
	userMessage = buildUser(ticker, timeframe, candles, ind, news, portfolio, knowledgeContext, similarSetups, performanceContext)
	return
}

func buildSystem(strategy string) string {
	return fmt.Sprintf(`You are a disciplined quantitative trading analyst running the "%s" strategy.

Your task is to analyse the provided market data and return a trade recommendation.

OUTPUT FORMAT — respond with ONLY a valid JSON object and no other text:
{
  "action":      "BUY" | "SELL" | "HOLD",
  "confidence":  <float 0.0–1.0>,
  "size_pct":    <float, recommended position size as %% of portfolio>,
  "stop_loss":   <float price, or null if HOLD>,
  "take_profit": <float price, or null if HOLD>,
  "reasoning":   "<concise explanation referencing specific data points>"
}

RULES:
- action must be exactly "BUY", "SELL", or "HOLD" (uppercase).
- confidence must be between 0.0 and 1.0.
- size_pct must be between 0.0 and 5.0 (max 5%% of portfolio per position).
- Do not output markdown, explanations, or any text outside the JSON object.
- If data is insufficient, return HOLD with low confidence.
- Never recommend size_pct > 0 for a HOLD action.`, strategy)
}

func buildUser(
	ticker, timeframe string,
	candles []Candle,
	ind Indicators,
	news []NewsItem,
	portfolio Portfolio,
	knowledgeContext string,
	similarSetups []SimilarPastSetup,
	performanceContext string,
) string {
	var b strings.Builder

	// Market data section
	b.WriteString(fmt.Sprintf("## TICKER: %s | TIMEFRAME: %s\n\n", ticker, timeframe))
	b.WriteString("### RECENT OHLCV (most recent first)\n")
	b.WriteString("Timestamp            | Open    | High    | Low     | Close   | Volume      | VWAP\n")
	b.WriteString("---------------------|---------|---------|---------|---------|-------------|--------\n")
	limit := len(candles)
	if limit > 10 {
		limit = 10
	}
	for _, c := range candles[:limit] {
		b.WriteString(fmt.Sprintf("%-20s | %7.2f | %7.2f | %7.2f | %7.2f | %11d | %7.2f\n",
			c.Timestamp.UTC().Format("2006-01-02 15:04"),
			c.Open, c.High, c.Low, c.Close, c.Volume, c.VWAP))
	}

	// Indicators section
	b.WriteString("\n### TECHNICAL INDICATORS\n")
	b.WriteString(fmt.Sprintf("EMA(20/50/200):        %.2f / %.2f / %.2f\n", ind.EMA20, ind.EMA50, ind.EMA200))
	b.WriteString(fmt.Sprintf("SMA(20/50/200):        %.2f / %.2f / %.2f\n", ind.SMA20, ind.SMA50, ind.SMA200))
	b.WriteString(fmt.Sprintf("RSI(14):               %.2f\n", ind.RSI))
	b.WriteString(fmt.Sprintf("MACD line/signal/hist: %.4f / %.4f / %.4f\n", ind.MACDLine, ind.MACDSignal, ind.MACDHistogram))
	b.WriteString(fmt.Sprintf("Bollinger Bands:       upper=%.2f  mid=%.2f  lower=%.2f\n", ind.BBUpper, ind.BBMiddle, ind.BBLower))
	b.WriteString(fmt.Sprintf("ADX(14):               %.2f\n", ind.ADX))
	b.WriteString(fmt.Sprintf("ATR(14):               %.4f\n", ind.ATR))
	b.WriteString(fmt.Sprintf("OBV:                   %.0f\n", ind.OBV))
	b.WriteString(fmt.Sprintf("Stoch K/D:             %.2f / %.2f\n", ind.StochK, ind.StochD))
	b.WriteString(fmt.Sprintf("VWAP:                  %.2f\n", ind.VWAP))
	b.WriteString(fmt.Sprintf("Volume SMA20 / ratio:  %.0f / %.2fx\n", ind.VolSMA20, ind.VolRatio))

	// Portfolio context section
	b.WriteString("\n### PORTFOLIO CONTEXT\n")
	if portfolio.CashAvailable > 0 {
		b.WriteString(fmt.Sprintf("Cash available: $%.2f\n", portfolio.CashAvailable))
	} else {
		b.WriteString("Cash available: (not yet available — Phase 4)\n")
	}
	if len(portfolio.OpenPositions) > 0 {
		b.WriteString("Open positions:\n")
		for _, p := range portfolio.OpenPositions {
			b.WriteString(fmt.Sprintf("  %s: qty=%.4f  avg_cost=%.2f  value=%.2f  unrealised_pl=%.2f\n",
				p.Ticker, p.Quantity, p.AvgCostBasis, p.CurrentValue, p.UnrealizedPL))
		}
	} else {
		b.WriteString("Open positions: none\n")
	}

	// News section
	b.WriteString("\n### RECENT NEWS (last 48h)\n")
	if len(news) == 0 {
		b.WriteString("No recent news available.\n")
	} else {
		for _, n := range news {
			age := time.Since(n.PublishedAt)
			b.WriteString(fmt.Sprintf("[%s | %s | %.0fh ago] %s\n",
				n.Sentiment, n.Source, age.Hours(), n.Headline))
		}
	}

	// Knowledge graph context (populated by Phase 10 knowledge.BuildContext).
	b.WriteString("\n### KNOWLEDGE GRAPH\n")
	if knowledgeContext != "" {
		b.WriteString(knowledgeContext)
	} else {
		b.WriteString("No relationship data available.\n")
	}

	// Similar past setups (populated by Phase 11 semantic search).
	b.WriteString("\n### SIMILAR PAST ANALYSES\n")
	if len(similarSetups) > 0 {
		for _, s := range similarSetups {
			outcome := "outcome unknown"
			if s.TradeOutcomePct != nil {
				if *s.TradeOutcomePct >= 0 {
					outcome = fmt.Sprintf("trade resulted in +%.2f%%", *s.TradeOutcomePct)
				} else {
					outcome = fmt.Sprintf("trade resulted in %.2f%%", *s.TradeOutcomePct)
				}
			}
			b.WriteString(fmt.Sprintf("- %s [%s] — %s at confidence %.0f%%. %s. Reasoning: %s\n",
				s.CreatedAt, s.Ticker, s.Action, s.Confidence*100, outcome, truncateStr(s.Reasoning, 200)))
		}
	} else {
		b.WriteString("No similar past setups found.\n")
	}

	// Performance context section (populated by feedback pipeline Phase 5).
	b.WriteString("\n### PERFORMANCE CONTEXT\n")
	if performanceContext != "" {
		b.WriteString(performanceContext)
		b.WriteString("\n")
	} else {
		b.WriteString("No performance data available.\n")
	}

	b.WriteString("\nRespond with ONLY the JSON object.")
	return b.String()
}

// truncateStr truncates s to at most n runes, appending "…" if truncated.
func truncateStr(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
