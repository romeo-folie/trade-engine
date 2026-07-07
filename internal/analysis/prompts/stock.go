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

// ─── Shared calibration blocks ────────────────────────────────────────────────

// ConfidenceAnchors gives the confidence scale semantic meaning. Live outcome
// data (89 closed trades, Mar–Jul 2026) showed win rate rises monotonically
// with confidence (24% below 0.60 → 60% at 0.75–0.85) — the models' relative
// calibration is real — but with no anchors defined, scores clustered in a
// 0.55–0.68 hedge band and nothing ever scored ≥0.85. Anchors + an explicit
// anti-hedging rule push models to commit rather than default to the middle.
//
// Deliberately does NOT mention the engine's entry threshold: teaching the
// gate would inflate every score to just above it and destroy the calibration
// signal the gate depends on.
const ConfidenceAnchors = `CONFIDENCE CALIBRATION:
- 0.50 = coin flip — no discernible edge. Pair with HOLD/WAIT.
- 0.60 = weak edge — one supporting factor; others neutral or mixed.
- 0.70 = clear edge — multiple independent factors align (e.g. trend + volume + catalyst).
- 0.80+ = exceptional — rare; reserve for overwhelming multi-factor confluence.
- Commit to a view. Do not hedge into the 0.60–0.65 band by default: if the setup is real, score it accordingly; if it is not, score it below 0.60.`

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
	momentumPlay bool,
) (systemPrompt, userMessage string) {
	if momentumPlay {
		systemPrompt = buildMomentumSystem(strategy)
	} else {
		systemPrompt = buildSystem(strategy)
	}
	userMessage = buildUser(ticker, timeframe, candles, ind, news, portfolio, knowledgeContext, similarSetups, performanceContext)
	return
}

// buildMomentumSystem returns a system prompt for scanner-sourced tickers.
// It reframes the question from "swing-trade evaluation" to "is this momentum
// entry still valid?" — appropriate for intraday movers and news catalysts.
func buildMomentumSystem(strategy string) string {
	return fmt.Sprintf(`You are a short-term momentum trader evaluating a live entry opportunity.

This ticker was flagged by an intraday screener as a "%s" play (momentum move, news catalyst, or volume surge). Your job is to determine whether the entry is STILL valid given the current market data — the stock has already moved, so assess entry quality, not the original catalyst.

OUTPUT FORMAT — respond with ONLY a valid JSON object and no other text:
{
  "action":      "BUY" | "SELL" | "HOLD" | "WAIT",
  "confidence":  <float 0.0–1.0>,
  "size_pct":    <float, recommended position size as %% of portfolio>,
  "stop_loss":   <float price, or 0 if HOLD/WAIT>,
  "take_profit": <float price, or 0 if HOLD/WAIT>,
  "reasoning":   "<concise explanation referencing specific data points>"
}

RULES:
- action must be exactly "BUY", "SELL", "HOLD", or "WAIT" (uppercase).
- confidence must be between 0.0 and 1.0.
- size_pct must be between 0.0 and 5.0 (max 5%% of portfolio per position).
- Do not output markdown, explanations, or any text outside the JSON object.
- If data is insufficient, return HOLD with low confidence.
- Never recommend size_pct > 0 for a HOLD or WAIT action.
- Focus on short/medium timeframe signals (1H, 4H candles) over daily trend context.

%s

ENTRY DECISION GUIDE:
- BUY: momentum is intact, price is consolidating or pulling back cleanly, volume confirms, risk/reward >= 1.2:1 with a clear stop below recent structure.
- SELL: catalyst is negative, price shows exhaustion or a failed breakout, or a long position should exit.
- WAIT: use ONLY when you can name a specific, concrete blocker (e.g. "price is $X above named resistance with no pullback", "earnings in under 4 hours", "gap >20%% with zero intraday range printed"). Vague timing language ("setup forming", "wait for clarity", "consolidation in progress") is NOT a valid blocker — propose BUY/SELL with reduced confidence instead, or HOLD if the setup is genuinely dead.
- HOLD: the original momentum has faded, the catalyst is fully priced in, or the setup has failed. Remove this ticker from active consideration.

R:R SELF-FILTER (MANDATORY): Compute R:R as: reward = abs(take_profit - signal_price); risk = abs(signal_price - stop_loss); R:R = reward / risk. If R:R < 1.2, widen the take_profit toward the next structural target, or tighten the stop_loss to nearer structure — do not propose entry below 1.2. Only emit BUY if R:R >= 1.2.

SIZING: momentum trades carry higher short-term risk — keep size_pct conservative (1–3%% range) unless conviction is very high.`, strategy, ConfidenceAnchors)
}

// strategyGuidance returns a strategy-specific decision block appended to the
// generalist system prompt. Before this, the strategy name interpolation was
// the ONLY strategy-specific content — notably for news_sentiment, which
// produced 2/3 of all closed trades at a 25% win rate. Its worst losers
// (OPTU −26%, SBET −20%, TE −15%) were longs chasing catalysts after the move:
// the guidance below targets exactly that failure mode. Language mirrors the
// dormant sentiment specialist prompt in roles.go, which already had it right.
func strategyGuidance(strategy string) string {
	switch strategy {
	case "news_sentiment":
		return `

STRATEGY GUIDANCE — news_sentiment:
- Your edge is FRESHNESS: a catalyst under ~24h old that price has not yet fully absorbed. A bullish two-day-old headline is not a fresh signal.
- If price has already gapped or moved materially since the headline published, the edge is gone — HOLD. Do not chase a move that already happened; late entries on stale catalysts are this strategy's primary historical failure mode.
- Sentiment must be CONFIRMED by the price/volume response, not assumed from the headline alone. A bullish headline with flat or fading volume is a warning, not an entry.
- Weigh source credibility: multiple independent credible sources beat one aggregator restating the same item.`
	default:
		return ""
	}
}

// buildSystem returns the generalist analyst system prompt.
//
// NOTE on the performance-context ceiling: the "<45% win rate → confidence
// ceiling 0.65" rule sits BELOW risk.confidence_threshold (0.68 as of
// 2026-07-07), so a ceilinged ticker is deliberately hard-blocked from new
// entries — not merely penalized. This is intentional (the one ticker
// currently qualifying, SBET, ran 20% over 5 trades); revisit the ceiling if
// the gate ever moves.
func buildSystem(strategy string) string {
	return fmt.Sprintf(`You are a disciplined quantitative trading analyst running the "%s" strategy.

Your task is to analyse the provided market data and return a trade recommendation.

OUTPUT FORMAT — respond with ONLY a valid JSON object and no other text:
{
  "action":      "BUY" | "SELL" | "HOLD" | "WAIT",
  "confidence":  <float 0.0–1.0>,
  "size_pct":    <float, recommended position size as %% of portfolio>,
  "stop_loss":   <float price, or 0 if HOLD/WAIT>,
  "take_profit": <float price, or 0 if HOLD/WAIT>,
  "reasoning":   "<concise explanation referencing specific data points>"
}

RULES:
- action must be exactly "BUY", "SELL", "HOLD", or "WAIT" (uppercase).
- confidence must be between 0.0 and 1.0.
- size_pct must be between 0.0 and 5.0 (max 5%% of portfolio per position).
- Do not output markdown, explanations, or any text outside the JSON object.
- If data is insufficient, return HOLD with low confidence.
- Never recommend size_pct > 0 for a HOLD or WAIT action.
- Use HOLD when there is no meaningful catalyst or setup worth tracking.
- Use WAIT ONLY when you can name a specific, concrete blocker preventing
  entry on this cycle. Acceptable blockers are factual: "price is $2.40 above
  the named resistance at $X with no pullback printed", "earnings release in
  under 4 hours", "gap >20%% with zero intraday range and no volume
  confirmation". Vague language ("timing feels off", "wait for clarity",
  "consolidation in progress", "let it settle") is NOT a valid blocker. If
  the setup has a reasonable entry with a defined stop, propose BUY or SELL
  with reduced confidence rather than WAIT. Refusing to act has a cost too.
- Use BUY or SELL when there is a directional catalyst and the trade can be
  structured with a defined stop and target. You do not need perfect timing.
- R:R SELF-FILTER (MANDATORY): Compute R:R as: reward = abs(take_profit - signal_price); risk = abs(signal_price - stop_loss); R:R = reward / risk. If R:R < 1.2, widen the take_profit toward the next structural target, or tighten the stop_loss to nearer structure — do not propose entry below 1.2. Only emit BUY if R:R >= 1.2.

%s

USING PERFORMANCE CONTEXT (apply only when the ticker line shows at least 5 trades in last 30 days):
- Ticker win rate < 45%%: your confidence ceiling is 0.65 regardless of the technical setup. This is intentionally restrictive — a ticker with a demonstrated losing record must sit out until its record improves; do not stretch the score to rescue the setup.
- Ticker win rate >= 65%%: full confidence range (0.0–1.0) is available.
- Ticker win rate 45%%–65%%: no constraint — use your normal judgment.
- Ticker avg PnL is negative: lower your confidence by 0.05–0.10 but still propose BUY/SELL if the setup is otherwise sound. Do not use this as a blanket reason to WAIT.

USING SIMILAR PAST ANALYSES:
- If the majority of setups with known outcomes were losses, treat this as a headwind — lower your confidence by at least 0.10.
- If the majority of setups with known outcomes were gains, treat this as a tailwind — you may raise your confidence by up to 0.10.
- Entries marked "outcome unknown" carry no weight.%s`, strategy, ConfidenceAnchors, strategyGuidance(strategy))
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
	// The R:R self-filter in every system prompt computes against
	// "signal_price", which was otherwise never supplied — models had to guess
	// it meant the latest close. Define it explicitly.
	if len(candles) > 0 {
		b.WriteString(fmt.Sprintf("CURRENT PRICE (latest close): %.2f — use this as signal_price in the R:R self-filter.\n\n", candles[0].Close))
	}
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

	// Portfolio context section. Always print the actual cash figure — the old
	// "(not yet available — Phase 4)" placeholder fired on any cash <= 0, telling
	// the model data was missing when the account may simply be fully invested.
	b.WriteString("\n### PORTFOLIO CONTEXT\n")
	b.WriteString(fmt.Sprintf("Cash available: $%.2f\n", portfolio.CashAvailable))
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
