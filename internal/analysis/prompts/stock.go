// Package prompts builds structured LLM prompts for stock analysis.
// PRD §8.1 — prompt structure.
package prompts

import (
	"fmt"
	"math"
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

// Portfolio carries current portfolio state. OmittedPositions counts open
// positions dropped by the conversion cap (toPromptPortfolio) so the prompt can
// say "+N more" instead of silently understating exposure.
type Portfolio struct {
	CashAvailable float64
	// CashKnown distinguishes a real balance (including $0 fully deployed)
	// from an unpopulated tracker; false renders a "not provided" marker.
	CashKnown        bool
	OpenPositions    []Position
	OmittedPositions int
}

// Position is a condensed open position. Side is "long"/"short" when known.
type Position struct {
	Ticker, Sentiment, Side                            string
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

// ─── Candidate lenses ─────────────────────────────────────────────────────────

// analysisLenses is the set of strategy labels naming an analytical LENS — a
// way of reading the chart. The watchlist strategies column also carries entry
// -gate TAGS (notably store.EntrySetupPullbackStrength, appended by the
// scanner's candidateStrategies), which are not lenses and must never be
// offered to an analyst as one.
var analysisLenses = map[string]bool{
	"trend_following": true,
	"mean_reversion":  true,
	"momentum":        true,
	"news_sentiment":  true,
}

// defaultLens matches the engine's historical fallback for a watchlist row
// carrying no usable strategy.
const defaultLens = "trend_following"

// IsLens reports whether s names an analytical lens. Callers use it to decide
// whether a model-reported selected_strategy can be trusted to key
// strategy-specific behaviour — a hallucinated or unknown label must fall back
// to the configured value rather than silently disabling that behaviour.
func IsLens(s string) bool {
	return analysisLenses[strings.ToLower(strings.TrimSpace(s))]
}

// Lenses filters a raw watchlist strategies list down to the analytical lenses
// an analyst may choose among, preserving order and dropping duplicates,
// unknown labels, and entry-gate tags. Never returns empty: a row with no
// recognisable lens falls back to defaultLens, matching prior engine behaviour.
func Lenses(strategies []string) []string {
	out := make([]string, 0, len(strategies))
	seen := make(map[string]bool, len(strategies))
	for _, s := range strategies {
		s = strings.ToLower(strings.TrimSpace(s))
		if !analysisLenses[s] || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return []string{defaultLens}
	}
	return out
}

// candidateLensBlock renders the lens-selection contract.
//
// The strategy label used to be injected as an IDENTITY ("You are an analyst
// running the X strategy"), which made the model an advocate for X: a
// mean_reversion analyst finds something to revert because that is who it is,
// and no prompt path permitted the answer "this strategy does not fit this
// ticker". Since the per-strategy config knobs are read nowhere (issue #509),
// that framing pressure was the label's ENTIRE practical effect.
//
// Presenting the admissible lenses as candidates removes the advocacy — but a
// menu of lenses is also a menu of justifications, and unconstrained it is
// strictly MORE permissive than a single assignment (trending? trend_following
// says buy; ranging? mean_reversion says buy the dip). Step 3 is the guard:
// naming why the rejected lenses do not fit is cheap when the selection is
// honest and awkward when it is post-hoc, and leaves an audit trail either way.
// Step 4 is the escape hatch that did not previously exist.
func candidateLensBlock(lenses []string) string {
	return fmt.Sprintf(`CANDIDATE LENSES: %s

These are the lenses admissible for this ticker. They are candidates, not
assignments. Before evaluating any setup:

1. Classify the current regime from the indicator block and price action alone.
2. Name which candidate lens that regime supports — if any.
3. State, in one clause each, why every REJECTED lens does not fit.
4. If no candidate fits, return HOLD, set "selected_strategy" to "none", and
   open your reasoning with "regime mismatch: <named reading>". This is a
   CORRECT and expected answer, not a failure to find a trade.

You may not evaluate a setup under a lens whose stated preconditions the data
contradicts. Selecting a lens because it yields a tradeable setup — rather than
because the regime supports it — is the specific failure this structure exists
to prevent.`, strings.Join(lenses, ", "))
}

// newsSentimentGuidance targets the news_sentiment failure mode measured in
// live trading: it produced 2/3 of all closed trades at a 25% win rate, and its
// worst losers (OPTU −26%, SBET −20%, TE −15%) were longs chasing catalysts
// after the move. Language mirrors the sentiment specialist prompt in roles.go,
// which already had it right.
const newsSentimentGuidance = `

STRATEGY GUIDANCE — news_sentiment:
- Your edge is FRESHNESS: a catalyst under ~24h old that price has not yet fully absorbed. A bullish two-day-old headline is not a fresh signal.
- If price has already gapped or moved materially since the headline published, the edge is gone — do not select this lens. Do not chase a move that already happened; late entries on stale catalysts are this lens's primary historical failure mode.
- Sentiment must be CONFIRMED by the price/volume response, not assumed from the headline alone. A bullish headline with flat or fading volume is a warning, not an entry.
- Weigh source credibility: multiple independent credible sources beat one aggregator restating the same item.`

// lensGuidance returns the per-lens decision blocks for every admissible lens.
// Guidance is attached when a lens is a CANDIDATE (not merely when it is the
// configured primary), because the analyst may now select any of them.
func lensGuidance(lenses []string) string {
	var b strings.Builder
	for _, lens := range lenses {
		if lens == "news_sentiment" {
			b.WriteString(newsSentimentGuidance)
		}
	}
	return b.String()
}

// ─── Shared calibration blocks ────────────────────────────────────────────────

// OutputSchema is the JSON contract shared by every analyst prompt.
//
// NB: substituted via a %s verb (an argument, not a format string), so percent
// signs must be SINGLE here — a double-escaped %% renders literally.
//
// "reasoning" is deliberately the FIRST key. Models generate left-to-right, so
// the previous schema (action first, reasoning last) committed to an action and
// then produced a justification for it: the stored reasoning was post-hoc
// rationalisation, not the process that reached the decision.
const OutputSchema = `OUTPUT FORMAT — respond with ONLY a valid JSON object and no other text:
{
  "reasoning":           "<work through the evidence BEFORE committing to an action>",
  "regime":              "trending" | "ranging" | "transitional",
  "selected_strategy":   "<the candidate lens you selected, or none>",
  "rejected_strategies": {"<lens>": "<one clause on why it does not fit>"},
  "action":              "BUY" | "SELL" | "HOLD" | "WAIT",
  "confidence":          <float 0.0–1.0>,
  "size_pct":            <float, recommended position size as % of portfolio>,
  "stop_loss":           <float price, or 0 if HOLD/WAIT>,
  "take_profit":         <float price, or 0 if HOLD/WAIT>
}

RULES:
- Emit the keys in the order shown. "reasoning" comes FIRST — reach your decision by writing it out; do not justify a decision already made.
- action must be exactly "BUY", "SELL", "HOLD", or "WAIT" (uppercase).
- confidence must be between 0.0 and 1.0.
- size_pct must be between 0.0 and 5.0 (max 5% of portfolio per position).
- selected_strategy must be exactly one of the candidate lenses listed above, or "none".
- rejected_strategies must name every candidate lens you did not select, each with a one-clause reason. Use {} only when a single lens was offered and you selected it.
- Do not output markdown, explanations, or any text outside the JSON object.
- If data is insufficient, return HOLD with low confidence.
- Never recommend size_pct > 0 for a HOLD or WAIT action.`

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

// PortfolioRules tells analysts how to use the PORTFOLIO CONTEXT section — and
// how not to. Grounded in the block-reason study: 24% of all blocked signals
// were SELLs on tickers we did not hold (analysts were portfolio-blind), and
// another 27% hit per-ticker/pyramid/max-position limits they could not see.
// The final rule guards against anchoring: seeing red P&L elsewhere must not
// re-introduce the WAIT/HOLD conservatism the prompts were de-biased of.
const PortfolioRules = `USING PORTFOLIO CONTEXT:
- SELL is only valid for this ticker if it appears in open positions as a long. Never propose SELL for a ticker not held — such signals are discarded by the engine.
- If this ticker is already held long, a BUY is a pyramid add-on: judge it against the existing position's basis and unrealized P&L, and keep combined exposure within the 5% per-position cap.
- Use the open-position list for correlation awareness: several same-direction positions in the same sector/theme argue for smaller size or lower confidence on a new correlated entry.
- Unrealized losses on OTHER positions are NOT a reason to WAIT or HOLD on this setup — evaluate this ticker on its own merits.`

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
//
// strategies is the ticker's admissible lens set (the watchlist strategies
// column). It is filtered through Lenses, so entry-gate tags and unknown labels
// are dropped and an empty list falls back to defaultLens.
func BuildStockPrompt(
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
	momentumPlay bool,
) (systemPrompt, userMessage string) {
	lenses := Lenses(strategies)
	if momentumPlay {
		systemPrompt = buildMomentumSystem(lenses)
	} else {
		systemPrompt = buildSystem(lenses)
	}
	userMessage = buildUser(ticker, timeframe, candles, ind, news, portfolio, knowledgeContext, similarSetups, performanceContext)
	return
}

// buildMomentumSystem returns a system prompt for scanner-sourced tickers.
// It reframes the question from "swing-trade evaluation" to "is this momentum
// entry still valid?" — appropriate for intraday movers and news catalysts.
func buildMomentumSystem(lenses []string) string {
	return fmt.Sprintf(`You are a short-term momentum trader evaluating a live entry opportunity.

This ticker was flagged by an intraday screener (momentum move, news catalyst, or volume surge). Your job is to determine whether the entry is STILL valid given the current market data — the stock has already moved, so assess entry quality, not the original catalyst.

%s

%s

TIMEFRAME FOCUS:
- Focus on short/medium timeframe signals (1H, 4H candles) over daily trend context.

%s

ENTRY DECISION GUIDE:
- BUY: momentum is intact, price is consolidating or pulling back cleanly, volume confirms, risk/reward >= 1.2:1 with a clear stop below recent structure.
- SELL: catalyst is negative, price shows exhaustion or a failed breakout, or a long position should exit.
- WAIT: use ONLY when you can name a specific, concrete blocker (e.g. "price is $X above named resistance with no pullback", "earnings in under 4 hours", "gap >20%% with zero intraday range printed"). Vague timing language ("setup forming", "wait for clarity", "consolidation in progress") is NOT a valid blocker — propose BUY/SELL with reduced confidence instead, or HOLD if the setup is genuinely dead.
- HOLD: the original momentum has faded, the catalyst is fully priced in, or the setup has failed. Remove this ticker from active consideration.

R:R SELF-FILTER (MANDATORY): Compute R:R as: reward = abs(take_profit - signal_price); risk = abs(signal_price - stop_loss); R:R = reward / risk. If R:R < 1.2, widen the take_profit toward the next structural target, or tighten the stop_loss to nearer structure — do not propose entry below 1.2. Only emit BUY if R:R >= 1.2.

SIZING: momentum trades carry higher short-term risk — keep size_pct conservative (1–3%% range) unless conviction is very high.

%s%s`, candidateLensBlock(lenses), OutputSchema, ConfidenceAnchors, PortfolioRules,
		lensGuidance(lenses))
}

// buildSystem returns the generalist analyst system prompt.
//
// NOTE on the performance-context ceiling: the "<45% win rate → confidence
// ceiling 0.65" rule sits BELOW risk.confidence_threshold (0.68 as of
// 2026-07-07), so a ceilinged ticker is deliberately hard-blocked from new
// entries — not merely penalized. This is intentional (the one ticker
// currently qualifying, SBET, ran 20% over 5 trades); revisit the ceiling if
// the gate ever moves.
func buildSystem(lenses []string) string {
	return fmt.Sprintf(`You are a disciplined quantitative trading analyst.

Your task is to analyse the provided market data and return a trade recommendation.

%s

%s

DECISION RULES:
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

%s

USING PERFORMANCE CONTEXT (apply only when the ticker line shows at least 5 trades in last 30 days):
- Ticker win rate < 45%%: your confidence ceiling is 0.65 regardless of the technical setup. This is intentionally restrictive — a ticker with a demonstrated losing record must sit out until its record improves; do not stretch the score to rescue the setup.
- Ticker win rate >= 65%%: full confidence range (0.0–1.0) is available.
- Ticker win rate 45%%–65%%: no constraint — use your normal judgment.
- Ticker avg PnL is negative: lower your confidence by 0.05–0.10 but still propose BUY/SELL if the setup is otherwise sound. Do not use this as a blanket reason to WAIT.

USING SIMILAR PAST ANALYSES:
- If the majority of setups with known outcomes were losses, treat this as a headwind — lower your confidence by at least 0.10.
- If the majority of setups with known outcomes were gains, treat this as a tailwind — you may raise your confidence by up to 0.10.
- Entries marked "outcome unknown" carry no weight.%s`,
		candidateLensBlock(lenses), OutputSchema, ConfidenceAnchors, PortfolioRules,
		lensGuidance(lenses))
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
	// Render every candle the caller passed. CALLER CONTRACT: candles is
	// already capped — the engine clamps it to min(history_window_by_strategy,
	// analysis.max_candles_per_prompt) before building the request.
	//
	// This used to re-cap at a hardcoded 10, an undocumented third limit that
	// silently overrode both config knobs: an operator setting
	// max_candles_per_prompt: 20 with trend_following: 20 still got 10 candles,
	// and widening the window across the candidate lens set would have changed
	// nothing at all. Keeping the cap in exactly one config-driven place is the
	// point; to restore the old payload size, set max_candles_per_prompt: 10.
	for _, c := range candles {
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

	// Portfolio context section. The live engine still passes an empty
	// PortfolioContext (engine.go runAnalysis, "Phase 4"), so cash <= 0 almost
	// always means "not wired", not "fully invested". Rendering it as $0.00
	// would feed analysts a false zero-cash state and suppress BUY/sizing
	// (Codex #391 P1) — keep an explicit unknown marker instead, worded so the
	// model doesn't treat it as a constraint.
	// Rendering is deliberately CACHE-STABLE: raw CurrentValue / UnrealizedPL
	// change on every tick, which would make every prompt unique and neuter
	// both analysis_cache and the material-change gate. So only stable fields
	// (ticker, side, qty, avg cost) print exactly; cash rounds to the nearest
	// $100 and unrealized P&L renders as a percentage bucketed to 5 points —
	// the text only changes when something meaningful changes.
	b.WriteString("\n### PORTFOLIO CONTEXT\n")
	switch {
	case portfolio.CashKnown && math.Round(portfolio.CashAvailable/100)*100 <= 0:
		// Known non-positive balance: the account is fully deployed (or in a
		// margin debit). Rendering this as "not provided" would invite BUY
		// proposals the cash gates must then discard (Codex #392 P2).
		b.WriteString("Cash available: ≈$0 (fully deployed — a new long entry requires freeing capital first)\n")
	case portfolio.CashKnown || portfolio.CashAvailable > 0:
		b.WriteString(fmt.Sprintf("Cash available: ≈$%.0f\n", math.Round(portfolio.CashAvailable/100)*100))
	default:
		b.WriteString("Cash available: (not provided — do not assume zero; apply sizing rules normally)\n")
	}
	if len(portfolio.OpenPositions) > 0 {
		b.WriteString("Open positions (side qty @ avg cost, unrealized P&L bucketed to 5%):\n")
		for _, p := range portfolio.OpenPositions {
			side := p.Side
			if side == "" {
				side = "long"
			}
			line := fmt.Sprintf("  %s: %s %.4f @ %.2f", p.Ticker, side, p.Quantity, p.AvgCostBasis)
			// Print the P&L bucket only when the source actually knows it:
			// store-fallback positions (paper mode) carry no mark-to-market,
			// and a fabricated ≈+0% would misinform.
			if basis := p.Quantity * p.AvgCostBasis; basis != 0 && (p.UnrealizedPL != 0 || p.CurrentValue != 0) {
				bucket := math.Round(p.UnrealizedPL/math.Abs(basis)*100/5) * 5
				line += fmt.Sprintf("  unrealized≈%+.0f%%", bucket)
			}
			b.WriteString(line + "\n")
		}
		if portfolio.OmittedPositions > 0 {
			b.WriteString(fmt.Sprintf("  (+%d more open positions not listed)\n", portfolio.OmittedPositions))
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
