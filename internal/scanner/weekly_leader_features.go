package scanner

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/indicators"
	"github.com/romeo-folie/trade-engine/internal/store"
)

const weeklyLeaderFeatureVersion = "weekly-leader-v4"

// WeeklyLeaderFeatures are immutable, auditable measurements derived only from
// bars completed by the run's decision cutoff. Returns are decimal fractions.
type WeeklyLeaderFeatures struct {
	ClosePrice                float64  `json:"close_price"`
	Return5                   float64  `json:"return_5d"`
	Return20                  float64  `json:"return_20d"`
	Return60                  float64  `json:"return_60d"`
	Return126                 float64  `json:"return_126d"`
	High52WeekProximity       float64  `json:"high_52w_proximity"`
	MarketRS5                 float64  `json:"market_rs_5d"`
	MarketRS20                float64  `json:"market_rs_20d"`
	MarketRS60                float64  `json:"market_rs_60d"`
	MarketRS126               float64  `json:"market_rs_126d"`
	IndustryRS60              *float64 `json:"industry_rs_60d,omitempty"`
	RSAcceleration            float64  `json:"rs_acceleration"`
	RegressionAnnualizedSlope float64  `json:"regression_annualized_slope"`
	RegressionR2              float64  `json:"regression_r2"`
	Efficiency60              float64  `json:"efficiency_60d"`
	MaxDrawdown126            float64  `json:"max_drawdown_126d"`
	NATR14                    float64  `json:"natr_14"`
	VolAdjustedReturn60       float64  `json:"vol_adjusted_return_60d"`
	MedianDollarVolume20      float64  `json:"median_dollar_volume_20d"`
	DollarVolumeStability20   float64  `json:"dollar_volume_stability_20d"`
	NormalizedOBV20           float64  `json:"normalized_obv_20d"`
	CMF20                     float64  `json:"cmf_20d"`
	GapDomination60           float64  `json:"gap_domination_60d"`
	LargestPositiveGap60      float64  `json:"largest_positive_gap_60d"`
	PositiveProgress60        float64  `json:"positive_progress_60d"`
}

// WeeklyLeaderComponents records normalized component scores independently
// from the raw measurements so ranking decisions can be reconstructed.
type WeeklyLeaderComponents struct {
	Momentum        float64  `json:"momentum"`
	HighProximity   float64  `json:"high_proximity"`
	MarketRS        float64  `json:"market_relative_strength"`
	IndustryRS      *float64 `json:"industry_relative_strength,omitempty"`
	Acceleration    float64  `json:"rs_acceleration"`
	TrendQuality    float64  `json:"trend_quality"`
	VolAdjusted     float64  `json:"volatility_adjusted_return"`
	Liquidity       float64  `json:"dollar_volume_stability"`
	Accumulation    float64  `json:"accumulation"`
	FeatureCoverage float64  `json:"feature_coverage"`
	GapPenalty      float64  `json:"gap_penalty"`
}

// WeeklyLeaderEvaluation is produced for every eligible universe member,
// including explicit data failures and non-qualifiers.
type WeeklyLeaderEvaluation struct {
	Ticker          string
	Sector          string
	Industry        string
	MarketCapBand   string
	VolatilityBand  string
	Outcome         string
	FailureStage    string
	FailureReason   string
	HistoryFirstAt  *time.Time
	HistoryLastAt   *time.Time
	HistorySessions int
	BarSetHash      string
	Features        *WeeklyLeaderFeatures
	Components      *WeeklyLeaderComponents
	CompositeScore  *float64
	Percentile      *float64
	Rank            *int
	Qualified       bool
}

type weeklyLeaderRaw struct {
	member  store.SwingUniverseMember
	bars    []data.Candle
	f       WeeklyLeaderFeatures
	capBand string
	volBand string
}

type weeklyLeaderIndustryItem struct {
	ticker string
	value  float64
}

// EvaluateWeeklyLeaders performs deterministic cross-sectional ranking. It
// never fetches data and therefore cannot move its own as-of boundary.
func EvaluateWeeklyLeaders(
	members []store.SwingUniverseMember,
	barsByTicker map[string][]data.Candle,
	benchmarkBars []data.Candle,
	targetSession, asOf, decisionAt time.Time,
	cfg config.WeeklyLeaderConfig,
) []WeeklyLeaderEvaluation {
	results := make([]WeeklyLeaderEvaluation, 0, len(members))
	minHistory := cfg.MinHistorySessions
	if minHistory <= 0 {
		minHistory = 253
	}
	benchmark, benchmarkErr := completedDailyBars(
		benchmarkBars, targetSession, asOf,
	)
	var benchmarkFeatures WeeklyLeaderFeatures
	if benchmarkErr == nil && len(benchmark) >= minHistory {
		benchmarkFeatures, benchmarkErr = extractWeeklyLeaderFeatures(benchmark)
	} else if benchmarkErr == nil {
		benchmarkErr = fmt.Errorf("need %d completed sessions; got %d", minHistory, len(benchmark))
	}

	raw := make([]weeklyLeaderRaw, 0, len(members))
	for _, member := range members {
		evaluation := WeeklyLeaderEvaluation{
			Ticker: member.Ticker, Sector: member.Sector, Industry: member.Industry,
			Outcome: "failed",
		}
		if benchmarkErr != nil {
			evaluation.FailureStage = "benchmark_history"
			evaluation.FailureReason = benchmarkErr.Error()
			results = append(results, evaluation)
			continue
		}
		bars, err := completedDailyBars(
			barsByTicker[member.Ticker], targetSession, asOf,
		)
		if len(bars) > 0 {
			first, last := bars[0].Timestamp, bars[len(bars)-1].Timestamp
			evaluation.HistoryFirstAt, evaluation.HistoryLastAt = &first, &last
			evaluation.HistorySessions = len(bars)
			evaluation.BarSetHash = weeklyLeaderBarSetHash(bars)
		}
		if err != nil {
			evaluation.FailureStage = "bar_validation"
			evaluation.FailureReason = err.Error()
			results = append(results, evaluation)
			continue
		}
		if len(bars) < minHistory {
			evaluation.FailureStage = "insufficient_history"
			evaluation.FailureReason = fmt.Sprintf(
				"need %d completed sessions; got %d", minHistory, len(bars),
			)
			results = append(results, evaluation)
			continue
		}
		features, err := extractWeeklyLeaderFeatures(bars)
		if err != nil {
			evaluation.FailureStage = "feature_extraction"
			evaluation.FailureReason = err.Error()
			results = append(results, evaluation)
			continue
		}
		features.MarketRS5 = features.Return5 - benchmarkFeatures.Return5
		features.MarketRS20 = features.Return20 - benchmarkFeatures.Return20
		features.MarketRS60 = features.Return60 - benchmarkFeatures.Return60
		features.MarketRS126 = features.Return126 - benchmarkFeatures.Return126
		raw = append(raw, weeklyLeaderRaw{
			member: member, bars: bars, f: features, capBand: marketCapBand(member.MarketCapMM),
		})
		results = append(results, evaluation)
	}

	assignIndustryStrength(raw, cfg.IndustryMinPeers, decisionAt)
	assignVolatilityBands(raw)
	scoreWeeklyLeaderRows(raw, results, cfg)
	sort.Slice(results, func(i, j int) bool { return results[i].Ticker < results[j].Ticker })
	return results
}

func completedDailyBars(
	input []data.Candle, targetSession, asOf time.Time,
) ([]data.Candle, error) {
	location := targetSession.Location()
	bySession := make(map[string]data.Candle, len(input))
	for _, bar := range input {
		if bar.Timestamp.After(asOf) {
			continue
		}
		if !positiveFinite(bar.Open) || !positiveFinite(bar.High) ||
			!positiveFinite(bar.Low) || !positiveFinite(bar.Close) ||
			bar.Volume <= 0 || bar.High < math.Max(bar.Open, bar.Close) ||
			bar.Low > math.Min(bar.Open, bar.Close) || bar.Low > bar.High {
			return nil, fmt.Errorf("invalid OHLCV at %s", bar.Timestamp.Format(time.RFC3339))
		}
		key := bar.Timestamp.In(location).Format("2006-01-02")
		if previous, ok := bySession[key]; !ok || bar.Timestamp.After(previous.Timestamp) {
			bySession[key] = bar
		}
	}
	out := make([]data.Candle, 0, len(bySession))
	for _, bar := range bySession {
		out = append(out, bar)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	targetDate := targetSession.In(location).Format("2006-01-02")
	latestDate := "none"
	if len(out) > 0 {
		latestDate = out[len(out)-1].Timestamp.In(location).Format("2006-01-02")
	}
	if latestDate != targetDate {
		return out, fmt.Errorf(
			"target session %s completed bar is unavailable; latest is %s",
			targetDate, latestDate,
		)
	}
	return out, nil
}

func extractWeeklyLeaderFeatures(bars []data.Candle) (WeeklyLeaderFeatures, error) {
	if len(bars) < 253 {
		return WeeklyLeaderFeatures{}, fmt.Errorf("need 253 completed sessions; got %d", len(bars))
	}
	n := len(bars)
	ret := func(horizon int) float64 {
		return logRatio(bars[n-1].Close, bars[n-1-horizon].Close)
	}
	highStart := n - 252
	high := bars[highStart].High
	for _, bar := range bars[highStart:] {
		high = math.Max(high, bar.High)
	}
	slope, r2 := logPriceRegression(bars[n-60:])
	annualizedSlope := math.Expm1(clamp(slope*252, -20, 20))
	efficiency := efficiencyRatio(bars[n-61:])
	drawdown := closeMaxDrawdown(bars[n-126:])
	atr := indicators.ComputeATR(bars[n-15:], 14)
	natr := atr / bars[n-1].Close
	r60 := ret(60)
	volAdjusted := 0.0
	if natr > 0 {
		volAdjusted = r60 / (natr * math.Sqrt(60))
	}
	medianDV, dvStability := dollarVolumeStats(bars[n-20:])
	obv, cmf := accumulationStats(bars[n-21:])
	gapDominance, largestGap, positiveProgress := gapDomination(bars[n-61:])
	return WeeklyLeaderFeatures{
		ClosePrice: bars[n-1].Close,
		Return5:    ret(5), Return20: ret(20), Return60: r60, Return126: ret(126),
		High52WeekProximity:       clamp(bars[n-1].Close/high, 0, 1),
		RegressionAnnualizedSlope: annualizedSlope,
		RegressionR2:              r2,
		Efficiency60:              efficiency,
		MaxDrawdown126:            drawdown,
		NATR14:                    natr,
		VolAdjustedReturn60:       volAdjusted,
		MedianDollarVolume20:      medianDV,
		DollarVolumeStability20:   dvStability,
		NormalizedOBV20:           obv,
		CMF20:                     cmf,
		GapDomination60:           gapDominance,
		LargestPositiveGap60:      largestGap,
		PositiveProgress60:        positiveProgress,
	}, nil
}

func assignIndustryStrength(rows []weeklyLeaderRaw, minPeers int, decisionAt time.Time) {
	if minPeers <= 0 {
		minPeers = 5
	}
	groups := make(map[string][]weeklyLeaderIndustryItem)
	for _, row := range rows {
		if !weeklyLeaderTaxonomyKnownAsOf(row.member, decisionAt) ||
			strings.EqualFold(row.member.Industry, store.UnknownTaxonomy) ||
			strings.TrimSpace(row.member.Industry) == "" {
			continue
		}
		key := strings.ToUpper(strings.TrimSpace(row.member.Industry))
		groups[key] = append(groups[key], weeklyLeaderIndustryItem{
			ticker: row.member.Ticker, value: row.f.Return60,
		})
	}
	positions := make(map[string]map[string]int, len(groups))
	for key := range groups {
		sort.Slice(groups[key], func(i, j int) bool {
			if groups[key][i].value == groups[key][j].value {
				return groups[key][i].ticker < groups[key][j].ticker
			}
			return groups[key][i].value < groups[key][j].value
		})
		positions[key] = make(map[string]int, len(groups[key]))
		for index, item := range groups[key] {
			positions[key][item.ticker] = index
		}
	}
	for i := range rows {
		key := strings.ToUpper(strings.TrimSpace(rows[i].member.Industry))
		peers := groups[key]
		position, exists := positions[key][rows[i].member.Ticker]
		if !weeklyLeaderTaxonomyKnownAsOf(rows[i].member, decisionAt) || !exists ||
			len(peers)-1 < minPeers {
			continue
		}
		value := rows[i].f.Return60 - medianIndustryExcluding(peers, position)
		rows[i].f.IndustryRS60 = &value
	}
}

func weeklyLeaderTaxonomyKnownAsOf(
	member store.SwingUniverseMember, decisionAt time.Time,
) bool {
	return member.TaxonomyStatus == "known" &&
		member.TaxonomyAsOf != nil && !member.TaxonomyAsOf.After(decisionAt)
}

func assignVolatilityBands(rows []weeklyLeaderRaw) {
	values := make([]float64, 0, len(rows))
	for _, row := range rows {
		values = append(values, row.f.NATR14)
	}
	low, high := quantile(values, 1.0/3.0), quantile(values, 2.0/3.0)
	for i := range rows {
		switch {
		case rows[i].f.NATR14 <= low:
			rows[i].volBand = "low"
		case rows[i].f.NATR14 <= high:
			rows[i].volBand = "medium"
		default:
			rows[i].volBand = "high"
		}
	}
}

func scoreWeeklyLeaderRows(
	rows []weeklyLeaderRaw, results []WeeklyLeaderEvaluation, cfg config.WeeklyLeaderConfig,
) {
	if len(rows) == 0 {
		return
	}
	byTicker := make(map[string]int, len(results))
	for i := range results {
		byTicker[results[i].Ticker] = i
	}
	metrics := map[string]func(weeklyLeaderRaw) (float64, bool){
		"return5":   func(r weeklyLeaderRaw) (float64, bool) { return r.f.Return5, true },
		"return20":  func(r weeklyLeaderRaw) (float64, bool) { return r.f.Return20, true },
		"return60":  func(r weeklyLeaderRaw) (float64, bool) { return r.f.Return60, true },
		"return126": func(r weeklyLeaderRaw) (float64, bool) { return r.f.Return126, true },
		"high":      func(r weeklyLeaderRaw) (float64, bool) { return r.f.High52WeekProximity, true },
		"mrs5":      func(r weeklyLeaderRaw) (float64, bool) { return r.f.MarketRS5, true },
		"mrs20":     func(r weeklyLeaderRaw) (float64, bool) { return r.f.MarketRS20, true },
		"mrs60":     func(r weeklyLeaderRaw) (float64, bool) { return r.f.MarketRS60, true },
		"mrs126":    func(r weeklyLeaderRaw) (float64, bool) { return r.f.MarketRS126, true },
		"industry": func(r weeklyLeaderRaw) (float64, bool) {
			if r.f.IndustryRS60 == nil {
				return 0, false
			}
			return *r.f.IndustryRS60, true
		},
		"slope":       func(r weeklyLeaderRaw) (float64, bool) { return r.f.RegressionAnnualizedSlope, true },
		"r2":          func(r weeklyLeaderRaw) (float64, bool) { return r.f.RegressionR2, true },
		"efficiency":  func(r weeklyLeaderRaw) (float64, bool) { return r.f.Efficiency60, true },
		"drawdown":    func(r weeklyLeaderRaw) (float64, bool) { return -r.f.MaxDrawdown126, true },
		"voladjusted": func(r weeklyLeaderRaw) (float64, bool) { return r.f.VolAdjustedReturn60, true },
		"dvstability": func(r weeklyLeaderRaw) (float64, bool) { return r.f.DollarVolumeStability20, true },
		"obv":         func(r weeklyLeaderRaw) (float64, bool) { return r.f.NormalizedOBV20, true },
		"cmf":         func(r weeklyLeaderRaw) (float64, bool) { return r.f.CMF20, true },
	}
	scores := make(map[string]map[string]float64, len(metrics))
	for name, getter := range metrics {
		scores[name] = bandAwarePercentiles(rows, getter)
	}
	for i := range rows {
		ticker := rows[i].member.Ticker
		shortRS := mean(
			marketRelativeScore(scores["mrs5"][ticker], rows[i].f.MarketRS5, 5),
			marketRelativeScore(scores["mrs20"][ticker], rows[i].f.MarketRS20, 20),
		)
		longRS := mean(
			marketRelativeScore(scores["mrs60"][ticker], rows[i].f.MarketRS60, 60),
			marketRelativeScore(scores["mrs126"][ticker], rows[i].f.MarketRS126, 126),
		)
		rows[i].f.RSAcceleration = shortRS - longRS
	}
	accelerationScores := bandAwarePercentiles(rows, func(r weeklyLeaderRaw) (float64, bool) {
		return r.f.RSAcceleration, true
	})

	type ranked struct {
		ticker string
		score  float64
	}
	ranking := make([]ranked, 0, len(rows))
	for _, row := range rows {
		ticker := row.member.Ticker
		momentum := 0.10*scores["return5"][ticker] +
			0.25*scores["return20"][ticker] +
			0.35*scores["return60"][ticker] +
			0.30*scores["return126"][ticker]
		marketRS := mean(
			marketRelativeScore(scores["mrs20"][ticker], row.f.MarketRS20, 20),
			marketRelativeScore(scores["mrs60"][ticker], row.f.MarketRS60, 60),
			marketRelativeScore(scores["mrs126"][ticker], row.f.MarketRS126, 126),
		)
		trend := mean(scores["slope"][ticker], scores["r2"][ticker],
			scores["efficiency"][ticker], scores["drawdown"][ticker])
		accumulation := mean(scores["obv"][ticker], scores["cmf"][ticker])
		industryScore, hasIndustry := scores["industry"][ticker]
		coverage := 1.0
		rs := marketRS
		var industryPtr *float64
		if hasIndustry {
			rs = 0.60*marketRS + 0.40*industryScore
			value := industryScore
			industryPtr = &value
		} else {
			// Industry RS is one of fourteen independent measurement groups.
			// Its missing weight is reallocated to market RS and never imputed.
			coverage = 13.0 / 14.0
		}
		prePenalty := 0.25*momentum +
			0.10*scores["high"][ticker] +
			0.20*rs +
			0.10*accelerationScores[ticker] +
			0.15*trend +
			0.05*scores["voladjusted"][ticker] +
			0.05*scores["dvstability"][ticker] +
			0.10*accumulation
		gapPenalty := 1 - 0.35*row.f.GapDomination60
		composite := prePenalty * gapPenalty
		components := WeeklyLeaderComponents{
			Momentum: momentum, HighProximity: scores["high"][ticker],
			MarketRS: marketRS, IndustryRS: industryPtr,
			Acceleration: accelerationScores[ticker], TrendQuality: trend,
			VolAdjusted: scores["voladjusted"][ticker],
			Liquidity:   scores["dvstability"][ticker], Accumulation: accumulation,
			FeatureCoverage: coverage, GapPenalty: gapPenalty,
		}
		result := &results[byTicker[ticker]]
		result.Features, result.Components = &row.f, &components
		result.MarketCapBand, result.VolatilityBand = row.capBand, row.volBand
		result.CompositeScore = floatPtr(composite)
		result.Outcome = "not_qualified"
		minCoverage := cfg.MinFeatureCoverage
		if minCoverage <= 0 {
			minCoverage = 0.90
		}
		switch {
		case coverage < minCoverage:
			result.FailureStage = "feature_coverage"
			result.FailureReason = fmt.Sprintf("%.3f below %.3f", coverage, minCoverage)
		case row.f.Return60 <= 0 || row.f.Return126 <= 0:
			result.FailureStage = "trend_floor"
			result.FailureReason = "60-session and 126-session returns must both be positive"
		}
		ranking = append(ranking, ranked{ticker: ticker, score: composite})
	}
	sort.Slice(ranking, func(i, j int) bool {
		if ranking[i].score == ranking[j].score {
			return ranking[i].ticker < ranking[j].ticker
		}
		return ranking[i].score > ranking[j].score
	})
	selection := cfg.SelectionPercentile
	if selection <= 0 || selection >= 1 {
		selection = 0.90
	}
	minScore := cfg.MinCompositeScore
	if minScore <= 0 {
		minScore = 0.70
	}
	for start := 0; start < len(ranking); {
		end := start + 1
		for end < len(ranking) && ranking[end].score == ranking[start].score {
			end++
		}
		percentile := 1.0
		if len(ranking) > 1 {
			midrank := float64(start+end-1) / 2
			percentile = 1 - midrank/float64(len(ranking)-1)
		}
		for i := start; i < end; i++ {
			row := ranking[i]
			result := &results[byTicker[row.ticker]]
			result.Rank, result.Percentile = intPtr(i+1), floatPtr(percentile)
			if result.FailureStage == "" && percentile >= selection && row.score >= minScore {
				result.Outcome, result.Qualified = "qualified", true
			} else if result.FailureStage == "" {
				result.FailureStage = "score_threshold"
				result.FailureReason = fmt.Sprintf(
					"score %.4f/percentile %.4f below %.4f/%.4f",
					row.score, percentile, minScore, selection,
				)
			}
		}
		start = end
	}
}

// marketRelativeScore retains both kinds of leadership evidence: where the
// stock ranks within this run and whether it actually outperformed the market.
// Subtracting one benchmark return from every stock preserves cross-sectional
// ordering, so a percentile alone would be identical to raw momentum. The
// bounded annualized excess-return term makes the benchmark level material
// without allowing an extreme short-horizon observation to dominate.
func marketRelativeScore(percentile, excessLogReturn float64, sessions int) float64 {
	if sessions <= 0 {
		return percentile
	}
	annualizedExcess := excessLogReturn * 252 / float64(sessions)
	absolute := 0.5 + 0.5*math.Tanh(annualizedExcess)
	return 0.5*percentile + 0.5*absolute
}

func bandAwarePercentiles(
	rows []weeklyLeaderRaw, getter func(weeklyLeaderRaw) (float64, bool),
) map[string]float64 {
	global := percentileForRows(rows, getter)
	capGroups, volGroups := make(map[string][]weeklyLeaderRaw), make(map[string][]weeklyLeaderRaw)
	for _, row := range rows {
		capGroups[row.capBand] = append(capGroups[row.capBand], row)
		volGroups[row.volBand] = append(volGroups[row.volBand], row)
	}
	capScores := make(map[string]map[string]float64, len(capGroups))
	for band, group := range capGroups {
		groupScores := percentileForRows(group, getter)
		if len(groupScores) >= 5 {
			capScores[band] = groupScores
		}
	}
	volScores := make(map[string]map[string]float64, len(volGroups))
	for band, group := range volGroups {
		groupScores := percentileForRows(group, getter)
		if len(groupScores) >= 5 {
			volScores[band] = groupScores
		}
	}
	out := make(map[string]float64, len(global))
	for _, row := range rows {
		base, ok := global[row.member.Ticker]
		if !ok {
			continue
		}
		score, weight := 0.70*base, 0.70
		if groupScores := capScores[row.capBand]; groupScores != nil {
			if value, exists := groupScores[row.member.Ticker]; exists {
				score, weight = score+0.15*value, weight+0.15
			}
		}
		if groupScores := volScores[row.volBand]; groupScores != nil {
			if value, exists := groupScores[row.member.Ticker]; exists {
				score, weight = score+0.15*value, weight+0.15
			}
		}
		out[row.member.Ticker] = score / weight
	}
	return out
}

func medianIndustryExcluding(
	items []weeklyLeaderIndustryItem,
	excluded int,
) float64 {
	remaining := len(items) - 1
	valueAt := func(index int) float64 {
		if index >= excluded {
			index++
		}
		return items[index].value
	}
	middle := remaining / 2
	if remaining%2 == 1 {
		return valueAt(middle)
	}
	return (valueAt(middle-1) + valueAt(middle)) / 2
}

func percentileForRows(
	rows []weeklyLeaderRaw, getter func(weeklyLeaderRaw) (float64, bool),
) map[string]float64 {
	type item struct {
		ticker string
		value  float64
	}
	items := make([]item, 0, len(rows))
	for _, row := range rows {
		if value, ok := getter(row); ok && !math.IsNaN(value) && !math.IsInf(value, 0) {
			items = append(items, item{ticker: row.member.Ticker, value: value})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].value == items[j].value {
			return items[i].ticker < items[j].ticker
		}
		return items[i].value < items[j].value
	})
	out := make(map[string]float64, len(items))
	if len(items) == 1 {
		out[items[0].ticker] = 0.5
		return out
	}
	for start := 0; start < len(items); {
		end := start + 1
		for end < len(items) && items[end].value == items[start].value {
			end++
		}
		midrank := float64(start+end-1) / 2
		score := midrank / float64(len(items)-1)
		for i := start; i < end; i++ {
			out[items[i].ticker] = score
		}
		start = end
	}
	return out
}

func logPriceRegression(bars []data.Candle) (slope, r2 float64) {
	n := float64(len(bars))
	var sumX, sumY, sumXX, sumXY float64
	for i, bar := range bars {
		x, y := float64(i), math.Log(bar.Close)
		sumX, sumY = sumX+x, sumY+y
		sumXX, sumXY = sumXX+x*x, sumXY+x*y
	}
	denominator := n*sumXX - sumX*sumX
	if denominator == 0 {
		return 0, 0
	}
	slope = (n*sumXY - sumX*sumY) / denominator
	meanY := sumY / n
	intercept := (sumY - slope*sumX) / n
	var residual, total float64
	for i, bar := range bars {
		y := math.Log(bar.Close)
		diff := y - (intercept + slope*float64(i))
		residual += diff * diff
		centered := y - meanY
		total += centered * centered
	}
	if total > 0 {
		r2 = clamp(1-residual/total, 0, 1)
	}
	return slope, r2
}

func efficiencyRatio(bars []data.Candle) float64 {
	if len(bars) < 2 {
		return 0
	}
	net := math.Abs(logRatio(bars[len(bars)-1].Close, bars[0].Close))
	var path float64
	for i := 1; i < len(bars); i++ {
		path += math.Abs(logRatio(bars[i].Close, bars[i-1].Close))
	}
	if path == 0 {
		return 0
	}
	return clamp(net/path, 0, 1)
}

func closeMaxDrawdown(bars []data.Candle) float64 {
	peak, maximum := bars[0].Close, 0.0
	for _, bar := range bars[1:] {
		peak = math.Max(peak, bar.Close)
		maximum = math.Max(maximum, (peak-bar.Close)/peak)
	}
	return clamp(maximum, 0, 1)
}

func dollarVolumeStats(bars []data.Candle) (float64, float64) {
	values, logs := make([]float64, 0, len(bars)), make([]float64, 0, len(bars))
	for _, bar := range bars {
		value := bar.Close * float64(bar.Volume)
		values, logs = append(values, value), append(logs, math.Log(value))
	}
	med := median(values)
	logMed := median(logs)
	deviations := make([]float64, len(logs))
	for i, value := range logs {
		deviations[i] = math.Abs(value - logMed)
	}
	return med, 1 / (1 + median(deviations))
}

func accumulationStats(bars []data.Candle) (float64, float64) {
	var signedVolume, totalVolume, moneyFlowVolume float64
	for i := 1; i < len(bars); i++ {
		volume := float64(bars[i].Volume)
		totalVolume += volume
		switch {
		case bars[i].Close > bars[i-1].Close:
			signedVolume += volume
		case bars[i].Close < bars[i-1].Close:
			signedVolume -= volume
		}
		if span := bars[i].High - bars[i].Low; span > 0 {
			multiplier := ((bars[i].Close - bars[i].Low) -
				(bars[i].High - bars[i].Close)) / span
			moneyFlowVolume += multiplier * volume
		}
	}
	if totalVolume == 0 {
		return 0, 0
	}
	return clamp(signedVolume/totalVolume, -1, 1),
		clamp(moneyFlowVolume/totalVolume, -1, 1)
}

func gapDomination(bars []data.Candle) (float64, float64, float64) {
	var largestGap, positiveProgress float64
	for i := 1; i < len(bars); i++ {
		gap := logRatio(bars[i].Open, bars[i-1].Close)
		if gap > largestGap {
			largestGap = gap
		}
		progress := logRatio(bars[i].Close, bars[i-1].Close)
		if progress > 0 {
			positiveProgress += progress
		}
	}
	if positiveProgress <= 0 {
		return 0, largestGap, positiveProgress
	}
	return clamp(largestGap/positiveProgress, 0, 1), largestGap, positiveProgress
}

func weeklyLeaderBarSetHash(bars []data.Candle) string {
	hash := sha256.New()
	var buffer [8]byte
	writeUint64 := func(value uint64) {
		binary.BigEndian.PutUint64(buffer[:], value)
		_, _ = hash.Write(buffer[:])
	}
	for _, bar := range bars {
		writeUint64(uint64(bar.Timestamp.UTC().UnixNano()))
		writeUint64(math.Float64bits(bar.Open))
		writeUint64(math.Float64bits(bar.High))
		writeUint64(math.Float64bits(bar.Low))
		writeUint64(math.Float64bits(bar.Close))
		writeUint64(uint64(bar.Volume))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func marketCapBand(value *float64) string {
	if value == nil {
		return "unknown"
	}
	switch {
	case *value < 2_000:
		return "small"
	case *value < 10_000:
		return "mid"
	default:
		return "large"
	}
}

func median(input []float64) float64 {
	if len(input) == 0 {
		return 0
	}
	values := append([]float64(nil), input...)
	sort.Float64s(values)
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return (values[middle-1] + values[middle]) / 2
}

func quantile(input []float64, q float64) float64 {
	if len(input) == 0 {
		return 0
	}
	values := append([]float64(nil), input...)
	sort.Float64s(values)
	position := clamp(q, 0, 1) * float64(len(values)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return values[lower]
	}
	return values[lower] + (values[upper]-values[lower])*(position-float64(lower))
}

func mean(values ...float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

func clamp(value, minimum, maximum float64) float64 {
	return math.Min(maximum, math.Max(minimum, value))
}

func positiveFinite(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func logRatio(numerator, denominator float64) float64 {
	return math.Log(numerator) - math.Log(denominator)
}

func floatPtr(value float64) *float64 { return &value }
func intPtr(value int) *int           { return &value }
