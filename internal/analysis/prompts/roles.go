// Package prompts contains role-specialised prompts for the sequential
// specialist analysis path.
//
// Each role prompt emits the same JSON output schema as the standard buildSystem
// prompt so downstream parsing and the AnalysisResponse shape are unchanged.
// All prompts include the verbatim R:R self-filter sentence first added
// in PR #334 — never drop it.
package prompts

import "fmt"

// rrSelfFilter is the literal R:R guidance sentence required by PR #334.
// Tests assert this exact string is present in every role prompt.
// Threshold lowered from 1.5 → 1.2 to match the gate calibration in
// config.yaml.risk.min_risk_reward (operator preference for risk-tolerant
// trading; prompt-level filter must not exceed the gate or signals get
// downgraded to WAIT at the source and never reach the gate).
const rrSelfFilter = `R:R SELF-FILTER (MANDATORY): Compute R:R as: reward = abs(take_profit - signal_price); risk = abs(signal_price - stop_loss); R:R = reward / risk. If R:R < 1.2, widen the take_profit toward the next structural target, or tighten the stop_loss to nearer structure — do not propose entry below 1.2. Only emit BUY if R:R >= 1.2.`

// RRSelfFilter exposes the verbatim R:R sentence for prompts built outside this
// package (the arbiter in internal/analysis/ensemble.go).
const RRSelfFilter = rrSelfFilter

// roleOutputSchema is the shared JSON-schema block all role prompts emit.
//
// NB: this const is substituted via a %s verb (an argument, not a format
// string), so percent signs must be SINGLE throughout — the previous
// double-escaped `%%` rendered literally as "%%" in the prompts the specialist
// models saw.
//
// It is the shared OutputSchema (stock.go) followed by ConfidenceAnchors and
// PortfolioRules, so every role emits the same JSON contract — including the
// lens-selection fields — and scores on the same calibrated scale.
const roleOutputSchema = OutputSchema + `

` + ConfidenceAnchors + `

` + PortfolioRules

// buildFundamentalSystem returns the specialist prompt for the fundamental analyst.
// Focus: earnings, catalysts, sector context, business quality. Downweights
// purely technical signals.
func buildFundamentalSystem(lenses []string) string {
	return fmt.Sprintf(`You are a fundamental research analyst contributing one vote in a 4-specialist debate.

YOUR MANDATE: judge this trade on business and catalyst quality. Anchor on earnings outlook, sector tailwinds/headwinds, recent corporate news, and management guidance. Treat the technical indicator block as secondary context — do not let chart structure override a weak or strong fundamental story.

%s

%s

ROLE GUIDANCE:
- BUY only when there is a credible fundamental catalyst (earnings beat, guidance raise, contract win, sector re-rating) AND nothing in price action invalidates the thesis.
- WAIT when the catalyst is real but already partially priced in, or when the next earnings event is too close to enter cleanly.
- HOLD when the fundamental setup is neutral or the catalyst is stale — there is nothing distinctive to act on.
- SELL when fundamentals have deteriorated (downgrades, guidance cuts, sector breakdown) and an existing long should exit.
- Cite specific news headlines, earnings cadence, or sector context in your reasoning. Do not list indicator readings.

%s`, candidateLensBlock(lenses), roleOutputSchema, rrSelfFilter)
}

// buildTechnicalSystem returns the specialist prompt for the technical analyst.
// Focus: indicators, structure, levels. Downweights catalyst noise.
func buildTechnicalSystem(lenses []string) string {
	return fmt.Sprintf(`You are a technical analyst contributing one vote in a 4-specialist debate.

YOUR MANDATE: judge this trade on chart structure and indicator alignment. Anchor on trend (EMA stack, ADX), momentum (RSI, MACD, Stoch), volatility (ATR, Bollinger Bands), volume confirmation (VolRatio, OBV), and the last 10 OHLCV candles. Treat news as background — do not let a single headline override a clear technical setup or breakdown.

%s

%s

ROLE GUIDANCE:
- BUY when trend is up (price above key EMAs, ADX rising), momentum confirms, volume supports, and a clear stop level is identifiable below recent structure.
- SELL when trend has rolled over (EMA cross-down, MACD bearish, ADX rising on the down side) or a long position should exit on technical exhaustion.
- WAIT when the chart is mid-consolidation, mid-pullback, or the breakout has not yet resolved — the setup is forming but timing is poor.
- HOLD when the chart is choppy, indicators conflict, or there is no actionable structure.
- Cite specific indicator values, candle patterns, and price levels in your reasoning. Do not lead with news.

%s`, candidateLensBlock(lenses), roleOutputSchema, rrSelfFilter)
}

// buildSentimentSystem returns the specialist prompt for the sentiment analyst.
// Focus: news flow, catalyst quality, social/headline tone.
func buildSentimentSystem(lenses []string) string {
	return fmt.Sprintf(`You are a sentiment analyst contributing one vote in a 4-specialist debate.

YOUR MANDATE: judge this trade on news flow and catalyst quality. Anchor on the recency, source, and tone of the news block, the freshness of the catalyst (hours-old beats days-old), and whether sentiment is aligned with price action. Treat raw indicators as secondary — your edge is reading the narrative.

%s

%s

ROLE GUIDANCE:
- BUY when sentiment is bullish, recent (under 24h), from credible sources, and price is responding constructively to the news.
- SELL when sentiment has turned negative (downgrades, guidance cuts, regulatory headlines) and an existing long should exit.
- WAIT when there is a real catalyst but the news is hours old and price has already gapped — the entry window may have closed for this cycle.
- HOLD when news is stale, neutral, or absent — there is no narrative edge to trade.
- Cite specific headlines, sources, and the age of each catalyst in your reasoning. A bullish two-day-old headline is not a fresh signal.

%s`, candidateLensBlock(lenses), roleOutputSchema, rrSelfFilter)
}

// buildRiskManagerSystem returns the mandatory final specialist-review prompt.
// Unlike the three directional roles, this model receives their completed
// findings and owns the final specialist outcome before deterministic controls.
func buildRiskManagerSystem(lenses []string) string {
	return fmt.Sprintf(`You are the mandatory Risk Specialist reviewing three completed specialist analyses. You are the final analytical stage before deterministic portfolio risk controls.

YOUR MANDATE: synthesize the fundamental, technical, and sentiment findings, then enforce risk discipline. Anchor on portfolio exposure, position sizing, R:R, correlation with existing open positions, ATR-relative stop distance, and drawdown vulnerability. You are explicitly allowed and expected to reject an otherwise attractive setup when risk discipline is violated.

%s

%s

ROLE GUIDANCE:
- Treat the three specialist findings as evidence, not independent votes that you can ignore.
- BUY or SELL only when the directional evidence is coherent and the trade clears the named discipline checks.
- WAIT (or HOLD when the setup is dead) when the specialists conflict materially or when you can name a concrete discipline failure:
  - Stop distance exceeds 2.5x ATR (over-extended entry — name the ATR value)
  - Position size implied by sizing rules would exceed the 5%% per-position cap (name the size)
  - Portfolio already holds the SAME ticker at full per-ticker cap, or a directly correlated long that would compound a named macro/sector risk
  - R:R is below 1.2 (the self-filter forces a wider TP / tighter SL — emit WAIT only if neither is plausible)
  - Drawdown exceeds 10%% AND current equity is within 2%% of the daily loss limit
- A vague concern ("conditions seem risky", "uncertainty is elevated", "let it settle") is not a valid rejection.
- You are a safety circuit, not a permanent brake. Do not second-guess aligned directional evidence without a specific conflict or risk failure.
- SELL when an existing long is breaching its stop, exposure has grown beyond comfort, or correlation risk has materialised.
- HOLD when there is no actionable signal.
- Your reasoning must summarize how the three findings support or conflict with the final action and name any discipline check that changed the outcome.

%s`, candidateLensBlock(lenses), roleOutputSchema, rrSelfFilter)
}

// BuildRolePrompt returns (system, user) prompts for a single specialist role.
// The user message is the same as the standard prompt (built from market data,
// indicators, news, portfolio, etc.) — only the system prompt changes per role.
//
// role must be one of: "fundamental", "technical", "sentiment", "risk_manager".
// Unknown roles return ("", "") so the caller can detect the misconfiguration.
func BuildRolePrompt(
	role, ticker string,
	strategies []string,
	timeframe string,
	candles []Candle,
	ind Indicators,
	news []NewsItem,
	portfolio Portfolio,
	knowledgeContext string,
	similarSetups []SimilarPastSetup,
	performanceContext string,
) (systemPrompt, userMessage string) {
	lenses := Lenses(strategies)
	switch role {
	case "fundamental":
		systemPrompt = buildFundamentalSystem(lenses)
	case "technical":
		systemPrompt = buildTechnicalSystem(lenses)
	case "sentiment":
		systemPrompt = buildSentimentSystem(lenses)
	case "risk_manager":
		systemPrompt = buildRiskManagerSystem(lenses)
	default:
		return "", ""
	}
	userMessage = buildUser(ticker, timeframe, candles, ind, news, portfolio, knowledgeContext, similarSetups, performanceContext)
	return
}

// BuildRiskManagerReviewPrompt builds the sequential Risk Specialist call and
// appends the completed directional findings to the normal market-data context.
func BuildRiskManagerReviewPrompt(
	ticker string,
	strategies []string,
	timeframe string,
	candles []Candle,
	ind Indicators,
	news []NewsItem,
	portfolio Portfolio,
	knowledgeContext string,
	similarSetups []SimilarPastSetup,
	performanceContext string,
	directionalFindings string,
) (systemPrompt, userMessage string) {
	systemPrompt, userMessage = BuildRolePrompt(
		"risk_manager", ticker, strategies, timeframe, candles, ind, news,
		portfolio, knowledgeContext, similarSetups, performanceContext,
	)
	if systemPrompt == "" || userMessage == "" {
		return "", ""
	}
	userMessage += "\n\nCOMPLETED SPECIALIST FINDINGS:\n" + directionalFindings +
		"\n\nReturn the final specialist outcome after reviewing all findings."
	return systemPrompt, userMessage
}
