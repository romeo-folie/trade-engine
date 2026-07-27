package scanner

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

const premarketEpisodicFeatureVersion = "premarket-episodic-v1"

type PremarketCatalyst struct {
	Status      string    `json:"status"`
	ExternalID  string    `json:"external_id,omitempty"`
	Headline    string    `json:"headline,omitempty"`
	Source      string    `json:"source,omitempty"`
	URL         string    `json:"url,omitempty"`
	PublishedAt time.Time `json:"published_at,omitempty"`
}

type PremarketEpisodicFeatures struct {
	GapPct                float64 `json:"gap_pct"`
	PremarketVolume       int64   `json:"premarket_volume"`
	TimeAdjustedRVOL      float64 `json:"time_adjusted_rvol"`
	DollarVolume          float64 `json:"dollar_volume"`
	TradeCount            int64   `json:"trade_count"`
	SpreadBPS             float64 `json:"spread_bps"`
	QuoteAgeSeconds       float64 `json:"quote_age_seconds"`
	BarCoverage           float64 `json:"bar_coverage"`
	PremarketHigh         float64 `json:"premarket_high"`
	PremarketLow          float64 `json:"premarket_low"`
	PremarketLocation     float64 `json:"premarket_location"`
	LastPrice             float64 `json:"last_price"`
	PreviousClose         float64 `json:"previous_close"`
	MarketCapMM           float64 `json:"market_cap_mm"`
	MarketCapRisk         string  `json:"market_cap_risk"`
	FloatStatus           string  `json:"float_status"`
	ADR14Pct              float64 `json:"adr_14_pct"`
	NATR14Pct             float64 `json:"natr_14_pct"`
	HistoricalVolumeCount int     `json:"historical_volume_count"`
}

type PremarketEpisodicEvaluation struct {
	Ticker        string                    `json:"ticker"`
	Outcome       string                    `json:"outcome"`
	Qualified     bool                      `json:"qualified"`
	RemovalStage  string                    `json:"removal_stage,omitempty"`
	FailureReason string                    `json:"failure_reason,omitempty"`
	Score         float64                   `json:"score"`
	Features      PremarketEpisodicFeatures `json:"features"`
	Catalyst      PremarketCatalyst         `json:"catalyst"`
}

func EvaluatePremarketEpisodic(
	ticker string,
	evidence data.PremarketEvidence,
	marketCapMM float64,
	checkpoint time.Time,
	catalyst PremarketCatalyst,
	cfg config.PremarketEpisodicConfig,
) PremarketEpisodicEvaluation {
	result := PremarketEpisodicEvaluation{
		Ticker:  strings.ToUpper(strings.TrimSpace(ticker)),
		Outcome: "not_qualified", Catalyst: catalyst,
	}
	switch result.Catalyst.Status {
	case "confirmed", "confirmed_absent", "provider_unavailable":
	default:
		result.Catalyst.Status = "provider_unavailable"
	}
	fail := func(stage, reason string) PremarketEpisodicEvaluation {
		result.RemovalStage, result.FailureReason = stage, reason
		return result
	}
	if evidence.PreviousClose <= 0 {
		return fail("prior_close", "previous close missing")
	}
	if len(evidence.Bars) == 0 {
		return fail("premarket_bars", "premarket bars missing")
	}
	if evidence.Quote.Bid <= 0 || evidence.Quote.Ask <= evidence.Quote.Bid ||
		evidence.Quote.Timestamp.IsZero() {
		return fail("quote", "executable bid/ask quote missing")
	}
	quoteAge := checkpoint.Sub(evidence.Quote.Timestamp).Seconds()
	if quoteAge < 0 {
		return fail("quote", "consolidated quote timestamp is after checkpoint")
	}
	if quoteAge > float64(cfg.MaxQuoteAgeSeconds) {
		return fail("stale_quote", "latest consolidated quote is stale")
	}

	var volume, trades int64
	high, low, last := 0.0, math.MaxFloat64, 0.0
	uniqueMinutes := make(map[int64]struct{}, len(evidence.Bars))
	for _, bar := range evidence.Bars {
		// A minute bar stamped at the checkpoint is still being formed and
		// cannot be used by a point-in-time observation.
		if !bar.Timestamp.Before(checkpoint) {
			continue
		}
		volume += bar.Volume
		trades += bar.TradeCount
		if bar.High > high {
			high = bar.High
		}
		if bar.Low > 0 && bar.Low < low {
			low = bar.Low
		}
		if bar.Close > 0 {
			last = bar.Close
		}
		uniqueMinutes[bar.Timestamp.Unix()/60] = struct{}{}
	}
	if last <= 0 || low == math.MaxFloat64 {
		return fail("premarket_bars", "premarket OHLC is incomplete")
	}
	expectedMinutes := expectedPremarketMinutes(checkpoint)
	coverage := 0.0
	if expectedMinutes > 0 {
		coverage = math.Min(1, float64(len(uniqueMinutes))/float64(expectedMinutes))
	}
	if coverage < cfg.MinBarCoverage {
		return fail("bar_coverage", "premarket bars are materially incomplete")
	}
	if len(evidence.HistoricalVolumes) < cfg.HistorySessions {
		return fail("rvol_history", "same-clock premarket volume history is incomplete")
	}
	medianVolume := medianInt64(evidence.HistoricalVolumes)
	rvol := 0.0
	if medianVolume > 0 {
		rvol = float64(volume) / float64(medianVolume)
	}
	mid := (evidence.Quote.Bid + evidence.Quote.Ask) / 2
	spreadBPS := (evidence.Quote.Ask - evidence.Quote.Bid) / mid * 10_000
	gapPct := (last/evidence.PreviousClose - 1) * 100
	dollarVolume := float64(volume) * last
	location := 0.5
	if high > low {
		location = (last - low) / (high - low)
	}
	adr, natr := premarketVolatility(evidence.DailyBars)
	marketCapRisk := "normal"
	if marketCapMM < 300 {
		marketCapRisk = "small_cap"
	} else if marketCapMM < 2_000 {
		marketCapRisk = "mid_cap"
	}
	result.Features = PremarketEpisodicFeatures{
		GapPct: gapPct, PremarketVolume: volume, TimeAdjustedRVOL: rvol,
		DollarVolume: dollarVolume, TradeCount: trades, SpreadBPS: spreadBPS,
		QuoteAgeSeconds: quoteAge, BarCoverage: coverage,
		PremarketHigh: high, PremarketLow: low, PremarketLocation: location,
		LastPrice: last, PreviousClose: evidence.PreviousClose,
		MarketCapMM: marketCapMM, MarketCapRisk: marketCapRisk,
		FloatStatus: "unavailable", ADR14Pct: adr, NATR14Pct: natr,
		HistoricalVolumeCount: len(evidence.HistoricalVolumes),
	}
	switch {
	case math.Abs(gapPct) < cfg.MinGapPct:
		return fail("gap", "gap is below configured threshold")
	case last < cfg.MinPrice:
		return fail("price", "price is below configured threshold")
	case marketCapMM < cfg.MinMarketCapMM:
		return fail("market_cap", "market capitalization is below configured threshold")
	case volume < cfg.MinPremarketVolume:
		return fail("volume", "premarket volume is below configured threshold")
	case rvol < cfg.MinTimeAdjustedRVOL:
		return fail("relative_volume", "time-adjusted relative volume is below configured threshold")
	case dollarVolume < cfg.MinDollarVolume:
		return fail("dollar_volume", "premarket dollar volume is below configured threshold")
	case trades < cfg.MinTradeCount:
		return fail("trade_count", "premarket trade count is below configured threshold")
	case spreadBPS > cfg.MaxSpreadBPS:
		return fail("spread", "executable spread exceeds configured threshold")
	}

	catalystScore := 0.35
	if result.Catalyst.Status == "confirmed" {
		catalystScore = 1
	} else if result.Catalyst.Status == "provider_unavailable" {
		catalystScore = 0
	}
	gapScore := math.Min(1, math.Abs(gapPct)/(cfg.MinGapPct*3))
	rvolScore := math.Min(1, rvol/(cfg.MinTimeAdjustedRVOL*3))
	liquidityScore := math.Min(1, dollarVolume/(cfg.MinDollarVolume*5))
	volatilityFit := 0.5
	if adr > 0 && math.Abs(gapPct) >= adr*0.75 {
		volatilityFit = 1
	}
	result.Score = 0.25*gapScore + 0.25*rvolScore +
		0.25*liquidityScore + 0.15*catalystScore + 0.10*volatilityFit
	result.Qualified = true
	result.Outcome = "qualified"
	return result
}

func expectedPremarketMinutes(checkpoint time.Time) int {
	start := time.Date(
		checkpoint.Year(), checkpoint.Month(), checkpoint.Day(), 4, 0, 0, 0,
		checkpoint.Location(),
	)
	if !checkpoint.After(start) {
		return 0
	}
	return int(checkpoint.Sub(start) / time.Minute)
}

func medianInt64(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

func premarketVolatility(bars []data.Candle) (float64, float64) {
	if len(bars) < 15 {
		return 0, 0
	}
	bars = bars[len(bars)-15:]
	var rangeSum, trSum float64
	for i := 1; i < len(bars); i++ {
		if bars[i-1].Close <= 0 || bars[i].Close <= 0 {
			continue
		}
		rangeSum += (bars[i].High - bars[i].Low) / bars[i-1].Close * 100
		tr := math.Max(
			bars[i].High-bars[i].Low,
			math.Max(
				math.Abs(bars[i].High-bars[i-1].Close),
				math.Abs(bars[i].Low-bars[i-1].Close),
			),
		)
		trSum += tr / bars[i].Close * 100
	}
	return rangeSum / 14, trSum / 14
}
