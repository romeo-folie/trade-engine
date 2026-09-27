// Package prompts builds structured LLM prompts for stock analysis.
// PRD §8.1 — prompt structure.
package prompts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/strategy/antichase"
	"github.com/romeo-folie/trade-engine/internal/strategy/pullback"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
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
	TimingSnapshot                      *antichase.Snapshot
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
	"trend_following":   true,
	"mean_reversion":    true,
	"momentum":          true,
	"news_sentiment":    true,
	"pullback_strength": true,
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

// exitOnlyStrategies are watchlist strategies whose ONLY valid directional
// recommendation is SELL. They are not lenses — there is nothing to select
// among — and they must never reach the candidate-lens contract: falling back
// to defaultLens would hand an exit-only row a trend_following prompt that can
// answer BUY, turning a position the operator wanted closed into a pyramid.
//
// MUST stay in sync with risk.IsSellOnlyStrategy, which is canonical. The list
// is duplicated rather than imported to keep the prompt layer free of a
// dependency on risk; TestExitOnlyStrategies_MatchRiskCanonicalList in the
// engine package fails if the two ever drift.
var exitOnlyStrategies = map[string]bool{
	"sell_only":      true,
	"exit_signal":    true,
	"close_position": true,
}

// IsExitOnlyStrategy reports whether s is an exit-only strategy.
func IsExitOnlyStrategy(s string) bool {
	return exitOnlyStrategies[strings.ToLower(strings.TrimSpace(s))]
}

// exitOnlyMandate replaces the candidate-lens contract for exit-only rows. It
// preserves the pre-existing contract the engine and risk manager rely on: the
// post-LLM ownership gate discards anything but SELL for these strategies
// (risk.IsSellOnlyStrategy), so offering the analyst a choice would only
// produce signals that are thrown away — or worse, a BUY that reads as a
// legitimate add.
func exitOnlyMandate(strategy string) string {
	return fmt.Sprintf(`EXIT MANDATE: this ticker is on the watchlist under %q, an exit-only strategy.

You are being asked ONE question: should the existing long position be closed now?

- SELL is the only valid directional recommendation. Never propose BUY — this row exists to exit a position, not to add to one.
- HOLD when the position should stay open and there is nothing to act on.
- WAIT when an exit is warranted but you can name a specific, concrete blocker on this cycle.
- Do not treat this as a fresh entry evaluation, and do not classify a regime or select a lens: "selected_strategy" must be %q and "rejected_strategies" must be {}.`,
		strategy, strategy)
}

// lensContract returns the framing block for a watchlist row: the exit mandate
// when the row is exit-only, otherwise the candidate-lens contract.
func lensContract(strategies []string) string {
	for _, s := range strategies {
		if IsExitOnlyStrategy(s) {
			return exitOnlyMandate(strings.ToLower(strings.TrimSpace(s)))
		}
	}
	return candidateLensBlock(Lenses(strategies))
}

// ExitOnlyOverride returns a trailing override block for exit-only rows, or ""
// for every other row.
//
// The generic guidance in the specialist ROLE GUIDANCE blocks and in the
// arbiter prompt authorises BUY unconditionally — neither was ever exit-aware
// (they predate this change). On an exit-only ticker that guidance directly
// contradicts the EXIT MANDATE, and the contradiction is resolvable in the
// wrong direction: nothing downstream blocks a BUY on a row the operator put on
// the watchlist to close, because the position-ownership gate only constrains
// SELL.
//
// Callers MUST append this LAST, after all role/arbiter guidance, so the
// override is the final instruction the model reads.
func ExitOnlyOverride(strategies []string) string {
	for _, s := range strategies {
		if !IsExitOnlyStrategy(s) {
			continue
		}
		return fmt.Sprintf(`

EXIT-ONLY OVERRIDE (takes precedence over every instruction above):
This ticker is on the watchlist under %q, an exit-only strategy. Any guidance above that authorises BUY does not apply here. The only valid directional recommendation is SELL; otherwise return HOLD or WAIT. Set "selected_strategy" to %q and "rejected_strategies" to {}.`,
			strings.ToLower(strings.TrimSpace(s)), strings.ToLower(strings.TrimSpace(s)))
	}
	return ""
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

const pullbackStrengthGuidance = `

STRATEGY GUIDANCE — pullback_strength:
- The PULLBACK STRENGTH section is a deterministic daily evaluation, not a thesis supplied by the scanner or a pattern for you to infer from a few candles.
- Select this lens only when its stated daily structure supports the current regime. Cite the named support, reclaim, and low-of-day invalidation in your reasoning.
- “Qualified: yes” reports that the deterministic setup passed at the observed daily mark. It does not by itself require a BUY; assess broader regime and risk/reward as usual.
- “Qualified: no” means the named failed check prevented qualification at that mark. Do not reinterpret or waive that check. A separate deterministic entry gate remains authoritative for every eventual pullback-strength trade.`

// lensGuidance returns the per-lens decision blocks for every admissible lens.
// Guidance is attached when a lens is a CANDIDATE (not merely when it is the
// configured primary), because the analyst may now select any of them.
func lensGuidance(lenses []string) string {
	var b strings.Builder
	for _, lens := range lenses {
		if lens == "news_sentiment" {
			b.WriteString(newsSentimentGuidance)
		}
		if lens == "pullback_strength" {
			b.WriteString(pullbackStrengthGuidance)
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
// KEY ORDER IS A RECOVERABILITY CONTRACT, not a style choice. Every compact
// decision field precedes the free-text ones, because models generate
// left-to-right and recoverPartialAnalysis needs BOTH action and confidence to
// salvage a vote from truncated output.
//
// An earlier revision of this schema put "reasoning" first, reasoning that
// action-first makes the stored explanation post-hoc rationalisation rather
// than the process that produced the decision. That is true, but it traded a
// real failure for a speculative gain: with most models configured at
// max_tokens 2048 and reasoning explicitly encouraged to work through the
// evidence, hitting the cap mid-prose emitted NO action and NO confidence, so
// the whole analyst vote was unrecoverable and ensemble quorum could break.
// The previous order survived exactly that truncation. Revisit only with a
// token-budget study behind it (see issue #509); the "reason before you
// commit" instruction below is the cheap part of the idea, kept without the
// truncation exposure.
const OutputSchema = `OUTPUT FORMAT — respond with ONLY a valid JSON object and no other text:
{
  "regime":              "trending" | "ranging" | "transitional",
  "selected_strategy":   "<the candidate lens you selected, or none>",
  "action":              "BUY" | "SELL" | "HOLD" | "WAIT",
  "confidence":          <float 0.0–1.0>,
  "size_pct":            <float, recommended position size as % of portfolio>,
  "stop_loss":           <float price, or 0 if HOLD/WAIT>,
  "take_profit":         <float price, or 0 if HOLD/WAIT>,
  "rejected_strategies": {"<lens>": "<one clause on why it does not fit>"},
  "decisive_reason":     "<one concrete sentence naming the fact that determined the final action>",
  "reasoning":           "<the evidence that decided it — specific values, not a summary>"
}

RULES:
- Emit the keys in the order shown.
- Reason through the evidence BEFORE you commit to an action. "reasoning" is emitted last for reliability, but it must record the analysis that produced your decision — not a justification assembled after it.
- Keep "reasoning" under 150 words. Exceeding your output limit mid-sentence discards the entire response.
- action must be exactly "BUY", "SELL", "HOLD", or "WAIT" (uppercase).
- confidence must be between 0.0 and 1.0.
- size_pct is the proposed incremental BUY order size; follow the BUY RISK POLICY for its limits.
- selected_strategy must be exactly one of the candidate lenses listed above, or "none".
- rejected_strategies must name every candidate lens you did not select, each with a one-clause reason. Use {} only when a single lens was offered and you selected it.
- decisive_reason is mandatory. For WAIT, name the concrete blocker; never write a vague timing phrase.
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
- If this ticker is already held long, a BUY is a pyramid add-on: judge it against the existing position's basis and unrealized P&L, and keep combined ticker exposure within the BUY RISK POLICY cap after this order.
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
	return BuildStockPromptWithDiscoveryContext(
		ticker, strategies, timeframe, candles, ind, news, portfolio,
		knowledgeContext, similarSetups, performanceContext, momentumPlay, nil,
	)
}

// BuildStockPromptWithDiscoveryContext extends the standard prompt with a
// scanner-owned, structured provenance block. The older builder remains for
// callers that have no scanner contract.
func BuildStockPromptWithDiscoveryContext(
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
	discoveryContext *tradeprofile.DiscoveryContext,
	pullbackContexts ...*pullback.PromptContext,
) (systemPrompt, userMessage string) {
	var pullbackContext *pullback.PromptContext
	if len(pullbackContexts) > 0 {
		pullbackContext = pullbackContexts[0]
	}
	return BuildStockPromptWithAnalysisContext(
		ticker, strategies, timeframe, candles, ind, news, portfolio,
		knowledgeContext, similarSetups, performanceContext, momentumPlay,
		discoveryContext, pullbackContext, TimeframeRolePrimaryThesis, nil,
	)
}

// BuildStockPromptWithAnalysisContext renders the standard prompt together
// with explicit timeframe-workflow context. It is used for both the primary
// thesis pass and the optional entry-confirmation pass. The older discovery
// context builder remains for callers that do not participate in the workflow.
func BuildStockPromptWithAnalysisContext(
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
	discoveryContext *tradeprofile.DiscoveryContext,
	pullbackContext *pullback.PromptContext,
	timeframeRole TimeframeRole,
	primaryThesis *PrimaryThesisContext,
	riskPolicies ...RiskPolicy,
) (systemPrompt, userMessage string) {
	var riskPolicy RiskPolicy
	if len(riskPolicies) > 0 {
		riskPolicy = riskPolicies[0]
	}
	lenses := Lenses(strategies)
	contract := lensContract(strategies)
	exitOverride := ExitOnlyOverride(strategies)
	if momentumPlay {
		systemPrompt = buildMomentumSystem(contract, exitOverride, lenses, riskPolicy)
	} else {
		systemPrompt = buildSystem(contract, exitOverride, lenses, riskPolicy)
	}
	systemPrompt += timeframeRoleInstruction(timeframeRole)
	userMessage = buildUserWithTimeframeContext(ticker, timeframe, candles, ind, news, portfolio, knowledgeContext, similarSetups, performanceContext, discoveryContext, pullbackContext, timeframeRole, primaryThesis)
	return
}

// buildMomentumSystem returns a system prompt for scanner-sourced tickers.
// It reframes the question from "swing-trade evaluation" to "is this momentum
// entry still valid?" — appropriate for intraday movers and news catalysts.
func buildMomentumSystem(contract, exitOverride string, lenses []string, riskPolicies ...RiskPolicy) string {
	var policy RiskPolicy
	if len(riskPolicies) > 0 {
		policy = riskPolicies[0]
	}
	return fmt.Sprintf(`You are a short-term momentum trader evaluating a live entry opportunity.

This ticker was flagged by an intraday screener (momentum move, news catalyst, or volume surge). Your job is to determine whether the entry is STILL valid given the current market data — the stock has already moved, so assess entry quality, not the original catalyst.

%s

%s

%s

TIMEFRAME FOCUS:
- Focus on short/medium timeframe signals (1H, 4H candles) over daily trend context.

%s

ENTRY DECISION GUIDE:
- BUY: momentum is intact, price is consolidating or pulling back cleanly, volume confirms, risk/reward meets the BUY RISK POLICY minimum, and a clear stop sits below recent structure.
- SELL: catalyst is negative, price shows exhaustion or a failed breakout, or a long position should exit.
- WAIT: use ONLY when you can name a specific, concrete blocker (e.g. "price is $X above named resistance with no pullback", "earnings in under 4 hours", "gap >20%% with zero intraday range printed"). Vague timing language ("setup forming", "wait for clarity", "consolidation in progress") is NOT a valid blocker — propose BUY/SELL with reduced confidence instead, or HOLD if the setup is genuinely dead.
- HOLD: the original momentum has faded, the catalyst is fully priced in, or the setup has failed. Remove this ticker from active consideration.

%s

SIZING: momentum trades carry higher short-term risk — keep size_pct conservative within the BUY RISK POLICY cap unless conviction is very high.

%s%s%s`, contract, OutputSchema, RenderRiskPolicy(policy), ConfidenceAnchors,
		RenderRRSelfFilter(policy), PortfolioRules,
		lensGuidance(lenses), exitOverride)
}

// buildSystem returns the generalist analyst system prompt.
//
// NOTE on the performance-context ceiling: the "<45% win rate → confidence
// ceiling 0.65" rule sits BELOW risk.confidence_threshold (0.68 as of
// 2026-07-07), so a ceilinged ticker is deliberately hard-blocked from new
// entries — not merely penalized. This is intentional (the one ticker
// currently qualifying, SBET, ran 20% over 5 trades); revisit the ceiling if
// the gate ever moves.
func buildSystem(contract, exitOverride string, lenses []string, riskPolicies ...RiskPolicy) string {
	var policy RiskPolicy
	if len(riskPolicies) > 0 {
		policy = riskPolicies[0]
	}
	return fmt.Sprintf(`You are a disciplined quantitative trading analyst.

Your task is to analyse the provided market data and return a trade recommendation.

%s

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
- %s

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
- Entries marked "outcome unknown" carry no weight.%s%s`,
		contract, OutputSchema, RenderRiskPolicy(policy), RenderRRSelfFilter(policy),
		ConfidenceAnchors, PortfolioRules,
		lensGuidance(lenses), exitOverride)
}

// buildUser is retained for focused prompt tests and older package-local
// helpers. Timeframe-aware callers use buildUserWithTimeframeContext.
func buildUser(
	ticker, timeframe string,
	candles []Candle,
	ind Indicators,
	news []NewsItem,
	portfolio Portfolio,
	knowledgeContext string,
	similarSetups []SimilarPastSetup,
	performanceContext string,
	discoveryContext *tradeprofile.DiscoveryContext,
	pullbackContext *pullback.PromptContext,
) string {
	return buildUserWithTimeframeContext(
		ticker, timeframe, candles, ind, news, portfolio, knowledgeContext,
		similarSetups, performanceContext, discoveryContext, pullbackContext,
		TimeframeRolePrimaryThesis, nil,
	)
}

func buildUserWithTimeframeContext(
	ticker, timeframe string,
	candles []Candle,
	ind Indicators,
	news []NewsItem,
	portfolio Portfolio,
	knowledgeContext string,
	similarSetups []SimilarPastSetup,
	performanceContext string,
	discoveryContext *tradeprofile.DiscoveryContext,
	pullbackContext *pullback.PromptContext,
	timeframeRole TimeframeRole,
	primaryThesis *PrimaryThesisContext,
) string {
	var b strings.Builder

	// Market data section
	b.WriteString(fmt.Sprintf("## TICKER: %s | TIMEFRAME: %s\n\n", ticker, timeframe))
	writePrimaryThesisContext(&b, timeframeRole, primaryThesis)
	writeDiscoveryContext(&b, discoveryContext)
	writePullbackContext(&b, pullbackContext)
	writeTimingSnapshot(&b, ind.TimingSnapshot)
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

// writeTimingSnapshot gives every analysis topology the same compact,
// deterministic view of entry extension. It is evidence, not a model-created
// strategy: knowledge-graph prose and reasoning never establish freshness.
func writeTimingSnapshot(b *strings.Builder, s *antichase.Snapshot) {
	if s == nil {
		return
	}
	b.WriteString("### DETERMINISTIC BUY TIMING SNAPSHOT\n")
	b.WriteString(fmt.Sprintf("Observed: %s | reference price: %.2f (%s) | evidence: %s | completed daily sessions: %d | history: %s\n",
		s.ObservedAt.UTC().Format(time.RFC3339), s.ReferencePrice, valueOrUnknown(s.ReferenceSource),
		s.EvidenceStatus, s.CompletedSessions, s.HistoryScope))
	if s.EvidenceReason != "" {
		b.WriteString("Evidence note: " + s.EvidenceReason + "\n")
	}
	b.WriteString(fmt.Sprintf("3-session return: %+.2f%% | EMA20: %.2f | ATR14: %.2f | extension above EMA20: %.2f ATR | volume ratio: %.2fx\n",
		s.Return3SessionPct, s.EMA20, s.ATR14, s.EMAExtensionATR, s.VolumeRatio))
	b.WriteString(fmt.Sprintf("RSI14: %.2f | Stoch K: %.2f | Bollinger upper: %.2f | prior 52-week high: %.2f | proximity: %.2f%%\n",
		s.RSI14, s.StochK, s.BBUpper, s.PriorHigh52Week, s.PriorHighProximity*100))
	if s.CatalystAt != nil {
		b.WriteString(fmt.Sprintf("Latest timestamped bullish catalyst: %s from %s (%.1fh old).\n", s.CatalystAt.UTC().Format(time.RFC3339), valueOrUnknown(s.CatalystSource), s.CatalystAgeHours))
	} else {
		b.WriteString("Latest timestamped bullish catalyst: unavailable. Knowledge-graph prose does NOT establish catalyst freshness.\n")
	}
	b.WriteString("The engine enforces this timing policy again at broker admission. A strong trend is not by itself proof that a late entry is safe.\n\n")
}

// writeDiscoveryContext renders scanner evidence as compact, structured
// context. It is deliberately separate from the candidate-lens contract:
// provenance explains why the ticker was discovered but never authorises a
// new strategy.
func writeDiscoveryContext(b *strings.Builder, context *tradeprofile.DiscoveryContext) {
	if context == nil {
		return
	}
	canonical, err := context.Canonicalize()
	if err != nil {
		return
	}
	b.WriteString("### DISCOVERY CONTEXT\n")
	b.WriteString("Scanner provenance only; use it as evidence, not as an execution lens.\n")
	b.WriteString("For political-disclosure evidence, a separately sourced board or committee relationship may corroborate the thesis, but is optional and must never be inferred from the filing or required for analysis.\n")
	b.WriteString("Sources: " + strings.Join(canonical.Sources, ", ") + "\n")
	b.WriteString("Thesis: " + truncateStr(canonical.Thesis, 320) + "\n")
	for _, source := range canonical.Sources {
		evidence := canonical.EvidenceBySource[source]
		if source == "political_disclosure" && writePoliticalDisclosureEvidence(b, evidence) {
			continue
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, evidence); err != nil {
			continue
		}
		b.WriteString(fmt.Sprintf("Evidence[%s]: %s\n", source, truncateStr(compact.String(), 600)))
	}
	b.WriteString("\n")
}

// writePoliticalDisclosureEvidence renders aggregate facts and independently
// bounded filer/report details. The generic 600-character source limit would
// otherwise erase the distinction between repeated filings and independent
// participants.
func writePoliticalDisclosureEvidence(b *strings.Builder, raw json.RawMessage) bool {
	var evidence tradeprofile.PoliticalDisclosureEvidence
	if err := json.Unmarshal(raw, &evidence); err != nil ||
		(evidence.Version != 1 && evidence.Version != tradeprofile.PoliticalDisclosureEvidenceVersion) {
		return false
	}
	if evidence.Version == 1 {
		b.WriteString(fmt.Sprintf(
			"Evidence[political_disclosure].aggregate: filers=%d; buyers=%d; reports=%d; amendments=%d; transactions=%d; purchases=%d; sales=%d; exchanges=%d; other=%d\n",
			evidence.DistinctFilerCount, evidence.DistinctBuyerCount,
			evidence.ReportCount, evidence.AmendmentReportCount,
			evidence.TransactionCount, evidence.PurchaseCount,
			evidence.SaleCount, evidence.ExchangeCount, evidence.OtherTransactionCount,
		))
	} else {
		b.WriteString(fmt.Sprintf(
			"Evidence[political_disclosure].aggregate: filers=%d; buyers=%d; reports=%d; independent_reports=%d; amendments=%d; raw_transaction_rows=%d (independent=%d amendment=%d); independent_purchases=%d; independent_sales=%d; independent_exchanges=%d; independent_other=%d. Amendment rows remain in report detail/raw counts but are excluded from independent semantic aggregates.\n",
			evidence.DistinctFilerCount, evidence.DistinctBuyerCount,
			evidence.ReportCount, evidence.IndependentReportCount, evidence.AmendmentReportCount,
			evidence.TransactionCount, evidence.IndependentTransactionCount, evidence.AmendmentTransactionCount, evidence.PurchaseCount,
			evidence.SaleCount, evidence.ExchangeCount, evidence.OtherTransactionCount,
		))
	}
	b.WriteString(fmt.Sprintf(
		"Evidence[political_disclosure].amounts: purchases=%s; sales=%s\n",
		formatPoliticalAmountSummary(evidence.PurchaseAmounts),
		formatPoliticalAmountSummary(evidence.SaleAmounts),
	))
	b.WriteString(fmt.Sprintf(
		"Evidence[political_disclosure].timing: latest_transaction=%s; latest_filing_or_publication=%s; disclosure_lag=%s\n",
		valueOrUnknown(evidence.LatestTransactionDate),
		valueOrUnknown(evidence.LatestFilingOrPublicationDate),
		formatPoliticalLagSummary(evidence.DisclosureLagDays),
	))
	if evidence.Version >= tradeprofile.PoliticalDisclosureEvidenceVersion {
		b.WriteString(fmt.Sprintf(
			"Evidence[political_disclosure].qualifying_trigger: filers=%d; buyers=%d; purchase_minimum=$%d; latest_purchase=%s; latest_filing_or_publication=%s. These qualification fields apply only to the claimed trigger-window disclosures; older evidence remains context only.\n",
			evidence.QualifyingFilerCount, evidence.QualifyingBuyerCount,
			evidence.QualifyingPurchaseMinimum,
			valueOrUnknown(evidence.LatestQualifyingPurchaseDate),
			valueOrUnknown(evidence.LatestQualifyingFilingDate),
		))
	}
	b.WriteString(fmt.Sprintf(
		"Evidence[political_disclosure].patterns: repeated_filer=%t; independent_buyers=%t; contradictory_buying_and_selling=%t\n",
		evidence.RepeatedFilerActivity, evidence.IndependentBuyerActivity,
		evidence.ContradictoryActivity,
	))

	const maxPromptFilers = 6
	promptOmittedReports := 0
	for index, filer := range evidence.Filers {
		if index >= maxPromptFilers {
			break
		}
		if evidence.Version == 1 {
			b.WriteString(fmt.Sprintf(
				"Evidence[political_disclosure].filer[%d]: id=%s; name=%s; source=%s; chamber=%s; office=%s; owners=%s; reports=%d; amendments=%d; transactions=%d; purchases=%d (%s); sales=%d (%s); exchanges=%d; repeated=%t; contradictory=%t; latest_transaction=%s; latest_filing_or_publication=%s; lag=%s\n",
				index+1, truncateStr(filer.CanonicalFilerID, 100),
				truncateStr(filer.FilerName, 100), truncateStr(filer.Source, 30),
				truncateStr(valueOrUnknown(filer.Chamber), 40), truncateStr(valueOrUnknown(filer.Office), 100),
				truncateStr(valueOrUnknown(strings.Join(filer.ReportedOwners, ", ")), 100),
				filer.ReportCount, filer.AmendmentReportCount, filer.TransactionCount, filer.PurchaseCount,
				formatPoliticalAmountSummary(filer.PurchaseAmounts), filer.SaleCount,
				formatPoliticalAmountSummary(filer.SaleAmounts), filer.ExchangeCount, filer.RepeatedActivity,
				filer.ContradictoryActivity, valueOrUnknown(filer.LatestTransactionDate),
				valueOrUnknown(filer.LatestFilingOrPublicationDate), formatPoliticalLagSummary(filer.DisclosureLagDays),
			))
		} else {
			b.WriteString(fmt.Sprintf(
				"Evidence[political_disclosure].filer[%d]: id=%s; name=%s; source=%s; chamber=%s; office=%s; owners=%s; reports=%d; independent_reports=%d; amendments=%d; transactions=%d; purchases=%d (%s); sales=%d (%s); exchanges=%d; repeated=%t; contradictory=%t; latest_transaction=%s; latest_filing_or_publication=%s; lag=%s\n",
				index+1, truncateStr(filer.CanonicalFilerID, 100),
				truncateStr(filer.FilerName, 100), truncateStr(filer.Source, 30),
				truncateStr(valueOrUnknown(filer.Chamber), 40),
				truncateStr(valueOrUnknown(filer.Office), 100),
				truncateStr(valueOrUnknown(strings.Join(filer.ReportedOwners, ", ")), 100),
				filer.ReportCount, filer.IndependentReportCount, filer.AmendmentReportCount,
				filer.TransactionCount, filer.PurchaseCount,
				formatPoliticalAmountSummary(filer.PurchaseAmounts), filer.SaleCount,
				formatPoliticalAmountSummary(filer.SaleAmounts), filer.ExchangeCount, filer.RepeatedActivity,
				filer.ContradictoryActivity, valueOrUnknown(filer.LatestTransactionDate),
				valueOrUnknown(filer.LatestFilingOrPublicationDate),
				formatPoliticalLagSummary(filer.DisclosureLagDays),
			))
		}
		const maxPromptReportsPerFiler = 2
		if len(filer.Reports) > maxPromptReportsPerFiler {
			promptOmittedReports += len(filer.Reports) - maxPromptReportsPerFiler
		}
		for reportIndex, report := range filer.Reports {
			if reportIndex >= maxPromptReportsPerFiler {
				break
			}
			lag := "unknown"
			if report.DisclosureLagDays != nil {
				lag = fmt.Sprintf("%dd", *report.DisclosureLagDays)
			}
			b.WriteString(fmt.Sprintf(
				"  report[%d]: id=%s; type=%s; amendment=%t; filing_or_publication=%s; latest_transaction=%s; lag=%s; transactions=%d (purchases=%d sales=%d exchanges=%d); owners=%s; extraction=%s; document=%s\n",
				reportIndex+1, truncateStr(report.ReportID, 100),
				truncateStr(valueOrUnknown(report.ReportType), 60), report.IsAmendment,
				valueOrUnknown(report.FilingOrPublicationDate),
				valueOrUnknown(report.LatestTransactionDate), lag,
				report.TransactionCount, report.PurchaseCount, report.SaleCount,
				report.ExchangeCount,
				truncateStr(valueOrUnknown(strings.Join(report.ReportedOwners, ", ")), 100),
				truncateStr(valueOrUnknown(report.ExtractionStatus), 30),
				truncateStr(report.DocumentURL, 180),
			))
		}
	}
	limitations := append([]string(nil), evidence.DataQuality.Limitations...)
	if omitted := len(evidence.Filers) - maxPromptFilers; omitted > 0 {
		limitations = append(limitations, fmt.Sprintf("prompt omitted %d bounded filer detail record(s)", omitted))
	}
	if promptOmittedReports > 0 {
		limitations = append(limitations, fmt.Sprintf("prompt omitted %d bounded report detail record(s)", promptOmittedReports))
	}
	b.WriteString(fmt.Sprintf(
		"Evidence[political_disclosure].quality: identity_basis=%s; aggregate_omitted_filers=%d; aggregate_omitted_reports=%d; prompt_omitted_reports=%d; limitations=%s\n",
		truncateStr(evidence.DataQuality.CanonicalIdentityBasis, 160),
		evidence.DataQuality.OmittedFilerDetailCount,
		evidence.DataQuality.OmittedReportDetailCount,
		promptOmittedReports,
		truncateStr(valueOrUnknown(strings.Join(limitations, "; ")), 500),
	))
	return true
}

func formatPoliticalAmountSummary(summary tradeprofile.PoliticalDisclosureAmountSummary) string {
	if summary.KnownTransactionCount == 0 {
		return fmt.Sprintf("unknown (missing_ranges=%d)", summary.UnknownTransactionCount)
	}
	return fmt.Sprintf("$%d-$%d across %d known range(s); missing_ranges=%d",
		summary.MinimumTotal, summary.MaximumTotal,
		summary.KnownTransactionCount, summary.UnknownTransactionCount)
}

func formatPoliticalLagSummary(summary tradeprofile.PoliticalDisclosureLagSummary) string {
	if summary.KnownTransactionCount == 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d-%dd; latest_transaction=%dd; known=%d",
		summary.MinimumDays, summary.MaximumDays,
		summary.LatestTransactionDays, summary.KnownTransactionCount)
}

// writePullbackContext renders the evaluator's daily facts without turning
// them into a model-controlled entry rule. The execution gate independently
// re-evaluates these facts immediately before a pullback entry.
func writePullbackContext(b *strings.Builder, context *pullback.PromptContext) {
	if context == nil {
		return
	}
	b.WriteString("### PULLBACK STRENGTH (DETERMINISTIC DAILY EVALUATION)\n")
	b.WriteString("Evaluator-owned facts for lens selection only; deterministic entry qualification is rechecked before execution.\n")
	b.WriteString(fmt.Sprintf("Qualified: %t | failed check: %s\n", context.Qualified, valueOrUnknown(context.FailedCheck)))
	b.WriteString("Evaluator outcome: " + valueOrUnknown(context.Reason) + "\n")
	b.WriteString(fmt.Sprintf("Signal session: %s | mark/open/low: %s / %s / %s\n",
		formatPromptTime(context.SignalSession), formatPromptPrice(context.SignalPrice),
		formatPromptPrice(context.SessionOpen), formatPromptPrice(context.SessionLow)))
	b.WriteString(fmt.Sprintf("Momentum (1M/3M/6M): %s / %s / %s | EMA9/EMA21: %s / %s\n",
		formatPromptPct(context.Return1M), formatPromptPct(context.Return3M), formatPromptPct(context.Return6M),
		formatPromptPrice(context.EMA9), formatPromptPrice(context.EMA21)))
	b.WriteString(fmt.Sprintf("Base: %s sessions (%s to %s); width=%s; drift=%s; overlapping=%t; higher low=%t; range narrower=%t (%s early → %s recent)\n",
		formatPromptSessions(context.ConsolidationSessions), formatPromptTime(context.ConsolidationStart), formatPromptTime(context.ConsolidationEnd),
		formatPromptPct(context.ConsolidationWidthPct), formatPromptPct(context.ConsolidationDriftPct), context.ConsolidationOverlapping,
		context.ConsolidationHigherLow, context.ConsolidationRangeNarrower,
		formatPromptPct(context.EarlyRangePct), formatPromptPct(context.RecentRangePct)))
	b.WriteString(fmt.Sprintf("EMA structure: aligned/rising=%t; base closes at/above EMA21=%s; EMA interaction=%s\n",
		context.EMAAligned, formatPromptPct(context.EMA21HoldPct), formatPromptPct(context.EMAInteractionPct)))
	b.WriteString(fmt.Sprintf("AVWAP definition: %s\n", valueOrUnknown(context.AVWAPDefinition)))
	b.WriteString(fmt.Sprintf("AVWAPs: swing-high %s (anchor %s); swing-low %s (anchor %s)\n",
		formatPromptPrice(context.AVWAPHigh), formatPromptTime(context.AVWAPHighAnchor),
		formatPromptPrice(context.AVWAPLow), formatPromptTime(context.AVWAPLowAnchor)))
	b.WriteString(fmt.Sprintf("Support components: horizontal=%s (%d tests); selected AVWAP=%s + %s=%s; tolerance=%s; effective support=%s\n",
		formatPromptPrice(context.HorizontalSupport), context.HorizontalSupportTests, formatPromptPrice(context.StackedAVWAP),
		valueOrUnknown(context.StackedCompanionKind), formatPromptPrice(context.StackedCompanion),
		formatPromptPct(context.SupportTolerancePct), formatPromptPrice(context.Support)))
	b.WriteString(fmt.Sprintf("Signal checks: flushed into support=%t; positive reclaim=%t\n", context.FlushedIntoSupport, context.PositiveReclaim))
	b.WriteString(fmt.Sprintf("Low-of-day stop: %s (%s risk; maximum %s); execution invalidation: %s\n\n",
		formatPromptPrice(context.StopLoss), formatPromptPct(context.StopWidthPct), formatPromptPct(context.MaxStopWidthPct),
		valueOrUnknown(context.ExecutionInvalidation)))
}

func formatPromptTime(v time.Time) string {
	if v.IsZero() {
		return "not evaluated"
	}
	return v.UTC().Format("2006-01-02")
}

func formatPromptPrice(v float64) string {
	if v <= 0 {
		return "not evaluated"
	}
	return fmt.Sprintf("$%.2f", v)
}

func formatPromptSessions(v int) string {
	if v <= 0 {
		return "not evaluated"
	}
	return fmt.Sprintf("%d", v)
}

func formatPromptPct(v float64) string {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return "not evaluated"
	}
	return fmt.Sprintf("%.2f%%", v)
}

func valueOrUnknown(v string) string {
	if strings.TrimSpace(v) == "" {
		return "none"
	}
	return v
}

// truncateStr truncates s to at most n runes, appending "…" if truncated.
func truncateStr(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
