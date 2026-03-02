package prompts

import (
	"fmt"
	"strings"
	"time"
)

// ── Prediction prompt input types ─────────────────────────────────────────────

// ContractInfo holds current state of a prediction market contract.
type ContractInfo struct {
	Platform           string
	ContractID         string
	Title              string
	Categories         []string
	MarketPrice        float64 // current implied Yes probability (0–1)
	YesPrice           float64
	NoPrice            float64
	Bid                float64
	Ask                float64
	Volume24h          float64
	OpenInterest       float64
	ResolutionDeadline time.Time
}

// ContractPricePoint is a historical (timestamp, price) entry.
type ContractPricePoint struct {
	Timestamp time.Time
	Price     float64 // implied Yes probability (0–1)
}

// PredictionIndicators holds the computed indicators for the prompt.
type PredictionIndicators struct {
	PriceTrend          float64 // probability change per hour
	PriceVelocity       float64 // % change over window
	VolumeTrend         float64
	SpreadTrend         float64
	DaysToResolution    float64
	ProbabilityVelocity float64 // probability change per day
	Spread              float64
	SampleCount         int
}

// RelatedContract is a brief summary of a related contract on the same topic.
type RelatedContract struct {
	Title       string
	MarketPrice float64
	Platform    string
}

// ── Prompt builder ────────────────────────────────────────────────────────────

const predictionSystemPrompt = `You are an expert prediction market analyst with deep knowledge of probability calibration, Bayesian reasoning, and market microstructure.

Your task is to analyze a prediction market contract and estimate whether the market price accurately reflects the true probability of the event occurring.

You MUST respond with ONLY a valid JSON object matching this exact schema — no markdown, no preamble, no explanation outside the JSON:

{
  "action":                 "buy" | "sell" | "hold",
  "estimated_probability":  <float 0.0–1.0>,
  "market_probability":     <float 0.0–1.0>,
  "edge":                   <float, estimated_probability minus market_probability>,
  "confidence":             <float 0.0–1.0>,
  "reasoning":              "<concise reasoning, max 300 chars>",
  "key_factors":            ["<factor 1>", "<factor 2>", "<factor 3>"],
  "risks":                  ["<risk 1>", "<risk 2>"]
}

Rules:
- action "buy"  = buy YES contracts (you estimate true probability > market price)
- action "sell" = buy NO contracts  (you estimate true probability < market price)
- action "hold" = insufficient edge or high uncertainty
- Only recommend "buy" or "sell" when |edge| > 0.05 and confidence >= 0.60
- Be conservative — prediction markets are often well-calibrated
- Account for time decay: edge erodes as resolution deadline approaches
- Consider base rates, news sentiment, and related contract pricing`

// BuildPredictionPrompt returns (systemPrompt, userMessage) for a prediction
// market analysis request.  PRD §8.3.
func BuildPredictionPrompt(
	contract ContractInfo,
	history []ContractPricePoint,
	indicators *PredictionIndicators,
	related []RelatedContract,
	news []NewsItem,
) (systemPrompt, userMessage string) {
	systemPrompt = predictionSystemPrompt

	var sb strings.Builder

	// ── 1. Contract overview ──────────────────────────────────────────────────
	sb.WriteString("## CONTRACT\n")
	sb.WriteString(fmt.Sprintf("Platform:    %s\n", contract.Platform))
	sb.WriteString(fmt.Sprintf("Question:    %s\n", contract.Title))
	sb.WriteString(fmt.Sprintf("Categories:  %s\n", strings.Join(contract.Categories, ", ")))
	if !contract.ResolutionDeadline.IsZero() {
		daysLeft := time.Until(contract.ResolutionDeadline).Hours() / 24.0
		sb.WriteString(fmt.Sprintf("Resolves:    %s (%.1f days)\n",
			contract.ResolutionDeadline.Format("2006-01-02"), daysLeft))
	}
	sb.WriteString("\n")

	// ── 2. Current market pricing ─────────────────────────────────────────────
	sb.WriteString("## CURRENT MARKET PRICING\n")
	sb.WriteString(fmt.Sprintf("Yes price:    %.4f (%.1f%%)\n", contract.YesPrice, contract.YesPrice*100))
	sb.WriteString(fmt.Sprintf("No price:     %.4f (%.1f%%)\n", contract.NoPrice, contract.NoPrice*100))
	sb.WriteString(fmt.Sprintf("Bid/Ask:      %.4f / %.4f  (spread: %.4f)\n",
		contract.Bid, contract.Ask, contract.Ask-contract.Bid))
	sb.WriteString(fmt.Sprintf("Volume 24h:   $%.0f\n", contract.Volume24h))
	sb.WriteString(fmt.Sprintf("Open interest: $%.0f\n", contract.OpenInterest))
	sb.WriteString("\n")

	// ── 3. Price history ─────────────────────────────────────────────────────
	if len(history) > 0 {
		sb.WriteString("## PRICE HISTORY (oldest → newest)\n")
		// Show at most 20 snapshots, evenly spaced.
		step := 1
		if len(history) > 20 {
			step = len(history) / 20
		}
		for i := 0; i < len(history); i += step {
			p := history[i]
			sb.WriteString(fmt.Sprintf("  %s  %.4f (%.1f%%)\n",
				p.Timestamp.Format("2006-01-02 15:04"), p.Price, p.Price*100))
		}
		sb.WriteString("\n")
	}

	// ── 4. Indicators ─────────────────────────────────────────────────────────
	if indicators != nil {
		sb.WriteString("## MARKET INDICATORS\n")
		sb.WriteString(fmt.Sprintf("Price trend:          %+.5f prob/hr\n", indicators.PriceTrend))
		sb.WriteString(fmt.Sprintf("Price velocity:       %+.2f%% over window\n", indicators.PriceVelocity))
		sb.WriteString(fmt.Sprintf("Prob velocity:        %+.4f prob/day\n", indicators.ProbabilityVelocity))
		sb.WriteString(fmt.Sprintf("Volume trend:         %+.0f/snapshot\n", indicators.VolumeTrend))
		sb.WriteString(fmt.Sprintf("Spread trend:         %+.6f/snapshot\n", indicators.SpreadTrend))
		sb.WriteString(fmt.Sprintf("Days to resolution:   %.1f\n", indicators.DaysToResolution))
		sb.WriteString(fmt.Sprintf("Current spread:       %.4f\n", indicators.Spread))
		sb.WriteString(fmt.Sprintf("Snapshots analysed:   %d\n", indicators.SampleCount))
		sb.WriteString("\n")
	}

	// ── 5. Related contracts ──────────────────────────────────────────────────
	if len(related) > 0 {
		sb.WriteString("## RELATED CONTRACTS\n")
		for _, r := range related {
			sb.WriteString(fmt.Sprintf("  [%s] %s — %.1f%%\n", r.Platform, r.Title, r.MarketPrice*100))
		}
		sb.WriteString("\n")
	}

	// ── 6. Recent news ────────────────────────────────────────────────────────
	if len(news) > 0 {
		sb.WriteString("## RECENT NEWS\n")
		limit := 8
		if len(news) < limit {
			limit = len(news)
		}
		for _, n := range news[:limit] {
			age := time.Since(n.PublishedAt)
			ageStr := fmt.Sprintf("%.0fh ago", age.Hours())
			sb.WriteString(fmt.Sprintf("  [%s] [%s] %s\n", ageStr, n.Sentiment, n.Headline))
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("## RECENT NEWS\nNo relevant news found.\n\n")
	}

	// ── 7. Task ───────────────────────────────────────────────────────────────
	sb.WriteString("## TASK\n")
	sb.WriteString(fmt.Sprintf(
		"The market currently prices this at %.1f%% Yes probability. "+
			"Estimate the TRUE probability of YES resolution and determine whether there is a tradeable edge. "+
			"Return ONLY the JSON object described in the system prompt.",
		contract.MarketPrice*100,
	))

	return systemPrompt, sb.String()
}
