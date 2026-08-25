package analysis

import (
	"fmt"
	"strings"
)

// RenderTimeframeContexts renders supplemental timeframe evidence for custom
// and provider-built prompts alike. The supplied contexts are evidence for one
// decision, not independent analyst votes or confirmations.
func RenderTimeframeContexts(contexts []TimeframeContext) string {
	if len(contexts) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\n### SUPPLEMENTAL TIMEFRAME EVIDENCE — ONE UNIFIED DECISION\n")
	b.WriteString("Use every timeframe below as context for the single final recommendation. ")
	b.WriteString("Do not treat a supplemental timeframe as a separate vote, confirmation pass, or permission to override the final decision. ")
	b.WriteString("When a context is labelled entry_timing, use its latest close as the entry reference for stop, target, and R:R.\n")
	for _, context := range contexts {
		if strings.TrimSpace(context.Timeframe) == "" {
			continue
		}
		role := strings.TrimSpace(context.Role)
		if role == "" {
			role = "supporting"
		}
		b.WriteString(fmt.Sprintf("\n#### %s (%s evidence)\n", context.Timeframe, role))
		if len(context.RecentCandles) > 0 {
			b.WriteString(fmt.Sprintf("Latest close: %.2f\n", context.RecentCandles[0].Close))
		}
		ind := context.Indicators
		b.WriteString(fmt.Sprintf("Indicators: EMA20 %.2f | EMA50 %.2f | EMA200 %.2f | RSI %.2f | ADX %.2f | ATR %.2f | MACD %.4f | Volume ratio %.2f\n",
			ind.EMA20, ind.EMA50, ind.EMA200, ind.RSI, ind.ADX, ind.ATR, ind.MACDLine, ind.VolRatio))
		if len(context.RecentCandles) == 0 {
			b.WriteString("Recent OHLCV: unavailable\n")
			continue
		}
		b.WriteString("Recent OHLCV (most recent first):\n")
		for _, candle := range context.RecentCandles {
			b.WriteString(fmt.Sprintf("%s | O %.2f H %.2f L %.2f C %.2f V %d\n",
				candle.Timestamp.Format("2006-01-02 15:04"), candle.Open, candle.High, candle.Low, candle.Close, candle.Volume))
		}
	}
	return b.String()
}

func appendTimeframeContexts(systemPrompt, userMessage string, contexts []TimeframeContext) (string, string) {
	if len(contexts) == 0 {
		return systemPrompt, userMessage
	}
	systemPrompt += "\n\nMULTI-TIMEFRAME CONTRACT: make one final decision from all supplied timeframe evidence. A conflict is evidence to weigh, not a request for a second analysis pass."
	userMessage += RenderTimeframeContexts(contexts)
	return systemPrompt, userMessage
}

func multiTimeframeArbiterInstruction(contexts []TimeframeContext) string {
	if len(contexts) == 0 {
		return ""
	}
	return "\n\nMULTI-TIMEFRAME CONTRACT: resolve the analyst findings and all supplied timeframe evidence into one final action; the supplemental timeframe is not a separate veto or vote."
}
