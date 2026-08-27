package scanner

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// SwingCandidate is the enriched candidate passed to the LLM for scoring.
type SwingCandidate struct {
	Ticker       string
	Price        float64
	ChangePct    float64 // % price change today (from movers); 0 if from most-actives only
	Volume       int64   // today's volume
	TradeCount   int64   // number of trades today
	Return1M     float64
	Return3M     float64
	Return6M     float64
	MarketCapMM  float64
	DollarVolume float64
	// PullbackQualified marks candidates that cleared the point-in-time
	// liquidity, market-cap, momentum, and top-quartile research universe.
	// It is persisted as an additional watchlist strategy so the engine can
	// evaluate the setup without making this universe the scanner's only path.
	PullbackQualified bool
	// PullbackQualificationKnown means the required history and fundamentals
	// were available, so a legacy quarantined row may safely be reactivated
	// whether it qualified or definitively failed the objective screen.
	PullbackQualificationKnown bool
	Sources                    []string // includes mover, active, graph, weekly, industry, and premarket sources
	// SourceScores retains each discovery source's independent score, including
	// an explicit zero. It is evidence/lineage rather than an aggregate rank.
	SourceScores map[string]float64
	// promotionSourceRanks is assigned by the source-local promotion selector.
	// It is intentionally transient: ranks are audit/notification metadata for
	// this run, not durable candidate state.
	promotionSourceRanks map[string]int
	UniverseMember       *bool
	Sector               string
	Industry             string
	TaxonomyStatus       string
	TaxonomyAsOf         *time.Time
	TaxonomyProvider     string
	TaxonomyEvidence     json.RawMessage
	// promotionTaxonomyExcluded means this candidate's source taxonomy was not
	// present in the scanner run's selected universe. Keep the source evidence
	// for audit/prompting, but do not use it for this run's cap.
	promotionTaxonomyExcluded bool
	// WeeklyLeaderRunID links this source to its immutable full-universe
	// evaluation. It is empty for all other discovery sources.
	WeeklyLeaderRunID      string
	WeeklyLeaderClaimToken string
	// WeeklyLeaderEvidence summarizes the immutable cross-sectional outcome
	// that caused this candidate to be promoted. It is deliberately separate
	// from GraphContext because it is ranking evidence, not a relationship
	// thesis.
	WeeklyLeaderEvidence string
	weeklyLeaderFallback *SwingCandidate
	// PotentIndustry lineage identifies the exact group/member observation and
	// token-fences its optional promotion into the scanner.
	PotentIndustryRunID      string
	PotentIndustryGroupType  string
	PotentIndustryGroupKey   string
	PotentIndustryClaimToken string
	PotentIndustryEvidence   string
	// Potent-industry attempts are selected in group/member ranking order, not
	// by member score alone. These transient values keep the shared source cap
	// and operator outcome ranks aligned with that source's durable ordering.
	potentIndustryGroupRank  int
	potentIndustryMemberRank int
	potentIndustryFallback   *SwingCandidate
	potentIndustrySource     *SwingCandidate
	// PremarketEpisodic lineage token-fences a consolidated, catalyst-aware
	// pre-open observation when it joins the ordinary scanner cycle.
	PremarketEpisodicRunID      string
	PremarketEpisodicClaimToken string
	PremarketEpisodicEvidence   string
	// PoliticalDisclosureClaimToken fences all pending filing rows for this
	// ticker that were claimed by the current scanner cycle.
	PoliticalDisclosureClaimToken     string
	PoliticalDisclosureClaimReportIDs []string
	PoliticalDisclosureEvidence       string
	PoliticalDisclosureFacts          json.RawMessage
	politicalDisclosureFallback       *SwingCandidate
	politicalDisclosureSource         *SwingCandidate
	UniverseVersionID                 string
	UniverseAsOf                      time.Time
	RemovalStage                      string
	FailureStage                      string
	FailureReason                     string
	News                              []SwingNewsItem
	// sourceMarketObservations retains the raw observation belonging to each
	// ordinary screener source. Later source merges intentionally replace the
	// shared candidate fields for scoring, but must not rewrite a mover or
	// activity lease's independent identity.
	sourceMarketObservations map[string]map[string]any
	sourceGraphContexts      map[string]string
	// GraphContext, when set, explains why a knowledge-graph neighbour was surfaced
	// (e.g. "TSMC (on watchlist) supplies_to this company"). Empty for screener
	// candidates. Injected into the prompt so the LLM weighs the relational thesis.
	GraphContext         string
	SympathySeed         string
	SympathyRelationship string
	// SympathyTrigger is the immutable seed article that belongs only to the
	// news-sympathy source. Candidate News always describes this ticker, so an
	// overlapping mover/activity source cannot inherit the seed's evidence.
	SympathyTrigger *SwingNewsItem
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
- timeframes: momentum plays should use ["1H", "4H"] for entry timing; add "1D" only for longer multi-day swings. Prefer ["1H", "4H"] for momentum_breakout and news_catalyst setups. Use ["1D"] only for oversold_bounce or "other" where the move is likely to play out over several days
- if a "Graph link" is shown, the candidate was surfaced because it is related (supply chain, competition, etc.) to a ticker already being watched. Treat the link as a thesis to confirm, not a reason to add on its own: require a credible swing setup (catalyst, volume, or momentum) in the candidate itself before "add".
- if the Graph link begins with "SYMPATHY TRIGGER", fresh news just hit the RELATED ticker, not this one. Reason through the spillover explicitly: (1) direction — a supplier/partner/sector peer usually moves WITH the news, while a competitor may move AGAINST it (their loss can be this company's gain); judge from the specific relationship, not a fixed rule. (2) magnitude — how much of the related ticker's news plausibly transfers to this name's revenue or sentiment. (3) timing — check this candidate's own price/volume: if it has already repriced with the story, the sympathy edge is gone ("skip"); if it has not yet reacted and the spillover logic is sound, that unpriced lag IS the setup.
- if "Weekly leader evidence" is shown, treat it as quantitative multi-week momentum and trend-quality evidence from the full eligible universe. It is not a graph relationship or permission to add; confirm that the candidate still has a credible swing setup and reasonable entry.
- if "Potent industry evidence" is shown, treat it as quantitative sector/industry breadth, relative-strength, and member-leadership evidence. It is not permission to add; confirm that the stock itself still has a credible swing setup and reasonable entry.
- if "Premarket episodic evidence" is shown, treat it as point-in-time consolidated gap, participation, executable-liquidity, volatility-fit, and catalyst evidence captured before the open. It is discovery evidence, not entry permission; reject names that are already extended or lack a credible swing follow-through.
- This is a discovery watchlist — add anything with a credible setup worth monitoring; the trading engine applies its own signal gates before acting.`, convictionStr)

	var sb strings.Builder
	direction := "up"
	if candidate.ChangePct < 0 {
		direction = "down"
	}

	sb.WriteString("## CANDIDATE\n")
	sb.WriteString(fmt.Sprintf("Ticker:       %s\n", candidate.Ticker))
	if candidate.Price > 0 {
		sb.WriteString(fmt.Sprintf("Price:        %.2f\n", candidate.Price))
	} else {
		sb.WriteString("Market data:  unavailable; do not infer price, liquidity, or momentum\n")
	}
	if candidate.ChangePct != 0 {
		sb.WriteString(fmt.Sprintf("Change:       %+.2f%% (%s)\n", candidate.ChangePct, direction))
	}
	if candidate.Volume > 0 {
		sb.WriteString(fmt.Sprintf("Volume:       %d\n", candidate.Volume))
	}
	if candidate.TradeCount > 0 {
		sb.WriteString(fmt.Sprintf("Trade count:  %d\n", candidate.TradeCount))
	}
	if candidate.MarketCapMM > 0 {
		sb.WriteString(fmt.Sprintf("Market cap:   $%.1fM\n", candidate.MarketCapMM))
	}
	if candidate.DollarVolume > 0 {
		sb.WriteString(fmt.Sprintf("Dollar vol:   $%.0f\n", candidate.DollarVolume))
	}
	if candidate.Return1M != 0 || candidate.Return3M != 0 || candidate.Return6M != 0 {
		sb.WriteString(fmt.Sprintf("Momentum:     1m %+.1f%% · 3m %+.1f%% · 6m %+.1f%%\n",
			candidate.Return1M, candidate.Return3M, candidate.Return6M))
	}
	sb.WriteString(fmt.Sprintf("Surfaced by:  %s\n", strings.Join(candidate.Sources, ", ")))
	if candidate.GraphContext != "" {
		sb.WriteString(fmt.Sprintf("Graph link:   %s\n", candidate.GraphContext))
	}
	if candidate.WeeklyLeaderEvidence != "" {
		sb.WriteString(fmt.Sprintf(
			"Weekly leader evidence: %s\n", candidate.WeeklyLeaderEvidence,
		))
	}
	if candidate.PotentIndustryEvidence != "" {
		sb.WriteString(fmt.Sprintf(
			"Potent industry evidence: %s\n", candidate.PotentIndustryEvidence,
		))
	}
	if candidate.PremarketEpisodicEvidence != "" {
		sb.WriteString(fmt.Sprintf(
			"Premarket episodic evidence: %s\n",
			candidate.PremarketEpisodicEvidence,
		))
	}
	if candidate.PoliticalDisclosureEvidence != "" {
		sb.WriteString(fmt.Sprintf(
			"Political disclosure evidence: %s\n",
			candidate.PoliticalDisclosureEvidence,
		))
	}
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
