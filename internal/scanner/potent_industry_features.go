package scanner

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

const potentIndustryFeatureVersion = "potent-industry-v1"

// PotentIndustryScoringPolicy is the deterministic portion of the potent
// industry configuration. The runner maps operator configuration into this
// narrow contract; provider, scheduling, persistence, and promotion controls
// deliberately do not belong in the pure scoring function.
type PotentIndustryScoringPolicy struct {
	MinHistorySessions       int
	TaxonomyMaxAge           time.Duration
	MinGroupMembers          int
	MinGroupCoverage         float64
	MinFreshTaxonomyCoverage float64
	MinPotencyScore          float64
	SelectionPercentile      float64
	MinAdvanceShare          float64
	MinMedianReturn5         float64
	MinPersistenceSessions   int
	MaxCapEqualDivergence    float64
	MaxMembersPerGroup       int
	MinMemberScore           float64
	SectorBenchmarks         map[string]string
	IndustryBenchmarks       map[string]string
}

// PotentIndustryGroupFeatures retain raw, decision-bearing measurements.
// Returns are log returns. Shares and coverage are in [0,1].
type PotentIndustryGroupFeatures struct {
	MemberCount           int     `json:"member_count"`
	FreshTaxonomyCount    int     `json:"fresh_taxonomy_count"`
	ValidMemberCount      int     `json:"valid_member_count"`
	FreshTaxonomyCoverage float64 `json:"fresh_taxonomy_coverage"`
	Coverage              float64 `json:"coverage"`
	EqualReturn1          float64 `json:"equal_return_1d"`
	EqualReturn5          float64 `json:"equal_return_5d"`
	EqualReturn20         float64 `json:"equal_return_20d"`
	CapReturn1            float64 `json:"cap_return_1d"`
	CapReturn5            float64 `json:"cap_return_5d"`
	CapReturn20           float64 `json:"cap_return_20d"`
	MedianReturn1         float64 `json:"median_return_1d"`
	MedianReturn5         float64 `json:"median_return_5d"`
	MedianReturn20        float64 `json:"median_return_20d"`
	MarketExcess1         float64 `json:"market_excess_1d"`
	MarketExcess5         float64 `json:"market_excess_5d"`
	MarketExcess20        float64 `json:"market_excess_20d"`
	SectorExcess1         float64 `json:"sector_excess_1d"`
	SectorExcess5         float64 `json:"sector_excess_5d"`
	SectorExcess20        float64 `json:"sector_excess_20d"`
	AdvanceShare          float64 `json:"advance_share"`
	AdvanceDecline        float64 `json:"advance_decline_normalized"`
	UpVolumeShare         float64 `json:"up_volume_share"`
	AboveEMA20Share       float64 `json:"above_ema20_share"`
	AboveEMA50Share       float64 `json:"above_ema50_share"`
	AtHigh20Share         float64 `json:"at_20d_high_share"`
	AtHigh252Share        float64 `json:"at_52w_high_share"`
	MedianRVOL20          float64 `json:"median_rvol_20d"`
	MedianDollarExpand20  float64 `json:"median_dollar_volume_expansion_20d"`
	BreadthAcceleration   float64 `json:"breadth_acceleration"`
	PersistenceSessions   int     `json:"persistence_sessions"`
	CapEqualDivergence    float64 `json:"cap_equal_divergence"`
}

// PotentIndustryGroupComponents are bounded [0,1] score components.
type PotentIndustryGroupComponents struct {
	Strength         float64 `json:"strength"`
	Breadth          float64 `json:"breadth"`
	VolumeExpansion  float64 `json:"volume_expansion"`
	Acceleration     float64 `json:"breadth_acceleration"`
	Persistence      float64 `json:"persistence"`
	DominancePenalty float64 `json:"dominance_penalty"`
}

type PotentIndustryGroupEvaluation struct {
	Industry        string
	Sector          string
	Benchmark       string
	Outcome         string
	FailureStage    string
	FailureReason   string
	Archetype       string
	Features        *PotentIndustryGroupFeatures
	Components      *PotentIndustryGroupComponents
	PotencyScore    *float64
	ScorePercentile *float64
	Rank            *int
	Qualified       bool
}

type PotentIndustryMemberFeatures struct {
	ClosePrice           float64 `json:"close_price"`
	Return1              float64 `json:"return_1d"`
	Return5              float64 `json:"return_5d"`
	Return20             float64 `json:"return_20d"`
	IndustryExcess1      float64 `json:"industry_excess_1d"`
	IndustryExcess5      float64 `json:"industry_excess_5d"`
	IndustryExcess20     float64 `json:"industry_excess_20d"`
	MarketExcess1        float64 `json:"market_excess_1d"`
	MarketExcess5        float64 `json:"market_excess_5d"`
	MarketExcess20       float64 `json:"market_excess_20d"`
	EMA20                float64 `json:"ema_20"`
	EMA50                float64 `json:"ema_50"`
	RegressionR2         float64 `json:"regression_r2"`
	Efficiency20         float64 `json:"efficiency_20d"`
	MaxDrawdown20        float64 `json:"max_drawdown_20d"`
	High52WeekProximity  float64 `json:"high_52w_proximity"`
	MedianDollarVolume20 float64 `json:"median_dollar_volume_20d"`
	RVOL20               float64 `json:"rvol_20d"`
}

type PotentIndustryMemberComponents struct {
	GroupStrength float64 `json:"group_strength"`
	IndustryRS    float64 `json:"stock_vs_industry"`
	MarketRS      float64 `json:"stock_vs_market"`
	TrendQuality  float64 `json:"trend_quality"`
	Liquidity     float64 `json:"liquidity_participation"`
}

type PotentIndustryMemberEvaluation struct {
	Ticker          string
	Industry        string
	Sector          string
	Outcome         string
	FailureStage    string
	FailureReason   string
	HistoryFirstAt  *time.Time
	HistoryLastAt   *time.Time
	HistorySessions int
	BarSetHash      string
	Features        *PotentIndustryMemberFeatures
	Components      *PotentIndustryMemberComponents
	MemberScore     *float64
	Rank            *int
	Qualified       bool
}

type PotentIndustryEvaluation struct {
	Groups  []PotentIndustryGroupEvaluation
	Members []PotentIndustryMemberEvaluation
}

type potentIndustryPreparedMember struct {
	member  store.SwingUniverseMember
	bars    []data.Candle
	feature PotentIndustryMemberFeatures
}

type potentIndustryPreparedGroup struct {
	index      int
	members    []*potentIndustryPreparedMember
	marketBars []data.Candle
	sectorBars []data.Candle
}

type potentIndustryRawComponents struct {
	index int
	value PotentIndustryGroupComponents
}

// EvaluatePotentIndustries is point-in-time and side-effect free. It evaluates
// every taxonomy-labelled industry, including groups that fail evidence gates.
func EvaluatePotentIndustries(
	members []store.SwingUniverseMember,
	barsByTicker map[string][]data.Candle,
	marketBars []data.Candle,
	benchmarkBars map[string][]data.Candle,
	targetSession, asOf, decisionAt time.Time,
	policy PotentIndustryScoringPolicy,
) PotentIndustryEvaluation {
	policy = normalizePotentIndustryPolicy(policy)
	result := PotentIndustryEvaluation{
		Groups:  make([]PotentIndustryGroupEvaluation, 0),
		Members: make([]PotentIndustryMemberEvaluation, 0, len(members)),
	}

	completedMarket, marketErr := potentAlignedBenchmark(
		marketBars, targetSession, asOf, policy.MinHistorySessions,
	)
	groups := make(map[string][]store.SwingUniverseMember)
	for _, member := range members {
		if !member.Eligible {
			result.Members = append(result.Members, PotentIndustryMemberEvaluation{
				Ticker: member.Ticker, Industry: member.Industry, Sector: member.Sector,
				Outcome: "failed", FailureStage: "eligibility",
				FailureReason: "universe member is not eligible",
			})
			continue
		}
		industry := strings.TrimSpace(member.Industry)
		if industry == "" || strings.EqualFold(industry, store.UnknownTaxonomy) {
			member.Industry = store.UnknownTaxonomy
			industry = store.UnknownTaxonomy
		}
		groups[normalizePotentLabel(industry)] = append(groups[normalizePotentLabel(industry)], member)
	}

	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	preparedGroups := make([]potentIndustryPreparedGroup, 0, len(keys))
	for _, key := range keys {
		groupMembers := groups[key]
		sort.Slice(groupMembers, func(i, j int) bool {
			return groupMembers[i].Ticker < groupMembers[j].Ticker
		})
		industry := strings.TrimSpace(groupMembers[0].Industry)
		sector, sectorAmbiguous := potentGroupSector(groupMembers)
		benchmark := resolvePotentBenchmark(industry, sector, policy)
		evaluation := PotentIndustryGroupEvaluation{
			Industry: industry, Sector: sector, Benchmark: benchmark, Outcome: "failed",
		}
		groupIndex := len(result.Groups)
		result.Groups = append(result.Groups, evaluation)
		prepared := potentIndustryPreparedGroup{index: groupIndex, marketBars: completedMarket}
		freshTaxonomyCount := 0

		if marketErr != nil {
			result.Groups[groupIndex].FailureStage = "market_benchmark"
			result.Groups[groupIndex].FailureReason = marketErr.Error()
		}
		if strings.EqualFold(industry, store.UnknownTaxonomy) &&
			result.Groups[groupIndex].FailureStage == "" {
			result.Groups[groupIndex].FailureStage = "taxonomy"
			result.Groups[groupIndex].FailureReason =
				"industry taxonomy is unavailable"
		}
		if sectorAmbiguous && result.Groups[groupIndex].FailureStage == "" {
			result.Groups[groupIndex].FailureStage = "sector_taxonomy"
			result.Groups[groupIndex].FailureReason =
				"industry members have conflicting sector classifications"
		}
		var sectorHistory []data.Candle
		if benchmark == "" {
			if result.Groups[groupIndex].FailureStage == "" {
				result.Groups[groupIndex].FailureStage = "benchmark_unmapped"
				result.Groups[groupIndex].FailureReason =
					"no industry override or sector benchmark is configured"
			}
		} else {
			var err error
			sectorHistory, err = potentAlignedMemberBars(
				benchmarkBars[strings.ToUpper(benchmark)], completedMarket,
				targetSession, asOf,
				policy.MinHistorySessions,
			)
			if err != nil && result.Groups[groupIndex].FailureStage == "" {
				result.Groups[groupIndex].FailureStage = "sector_benchmark"
				result.Groups[groupIndex].FailureReason = err.Error()
			}
		}
		prepared.sectorBars = sectorHistory

		for _, member := range groupMembers {
			memberEvaluation := PotentIndustryMemberEvaluation{
				Ticker: member.Ticker, Industry: member.Industry, Sector: member.Sector,
				Outcome: "failed",
			}
			memberIndex := len(result.Members)
			result.Members = append(result.Members, memberEvaluation)
			if reason := potentTaxonomyFailure(member, decisionAt, policy.TaxonomyMaxAge); reason != "" {
				result.Members[memberIndex].FailureStage = "taxonomy"
				result.Members[memberIndex].FailureReason = reason
				continue
			}
			freshTaxonomyCount++
			if member.MarketCapMM == nil || *member.MarketCapMM <= 0 ||
				math.IsNaN(*member.MarketCapMM) || math.IsInf(*member.MarketCapMM, 0) {
				result.Members[memberIndex].FailureStage = "market_cap"
				result.Members[memberIndex].FailureReason =
					"positive point-in-time market cap is required"
				continue
			}
			bars, err := potentAlignedMemberBars(
				barsByTicker[member.Ticker], completedMarket,
				targetSession, asOf, policy.MinHistorySessions,
			)
			if len(bars) > 0 {
				first, last := bars[0].Timestamp, bars[len(bars)-1].Timestamp
				result.Members[memberIndex].HistoryFirstAt = &first
				result.Members[memberIndex].HistoryLastAt = &last
				result.Members[memberIndex].HistorySessions = len(bars)
				result.Members[memberIndex].BarSetHash = weeklyLeaderBarSetHash(bars)
			}
			if err != nil {
				result.Members[memberIndex].FailureStage = "history"
				result.Members[memberIndex].FailureReason = err.Error()
				continue
			}
			feature := potentMemberFeatures(bars)
			result.Members[memberIndex].Features = &feature
			result.Members[memberIndex].Outcome = "not_ranked"
			prepared.members = append(prepared.members, &potentIndustryPreparedMember{
				member: member, bars: bars, feature: feature,
			})
		}

		features := potentGroupFeatures(prepared.members, completedMarket, sectorHistory)
		features.MemberCount = len(groupMembers)
		features.FreshTaxonomyCount = freshTaxonomyCount
		features.ValidMemberCount = len(prepared.members)
		features.FreshTaxonomyCoverage = fraction(freshTaxonomyCount, len(groupMembers))
		features.Coverage = fraction(len(prepared.members), len(groupMembers))
		result.Groups[groupIndex].Features = &features
		if result.Groups[groupIndex].FailureStage == "" {
			switch {
			case len(groupMembers) < policy.MinGroupMembers:
				result.Groups[groupIndex].FailureStage = "member_count"
				result.Groups[groupIndex].FailureReason = fmt.Sprintf(
					"need %d members; got %d", policy.MinGroupMembers, len(groupMembers),
				)
			case features.FreshTaxonomyCoverage < policy.MinFreshTaxonomyCoverage:
				result.Groups[groupIndex].FailureStage = "taxonomy_coverage"
				result.Groups[groupIndex].FailureReason = fmt.Sprintf(
					"fresh taxonomy coverage %.3f below %.3f",
					features.FreshTaxonomyCoverage, policy.MinFreshTaxonomyCoverage,
				)
			case features.ValidMemberCount < policy.MinGroupMembers:
				result.Groups[groupIndex].FailureStage = "evaluated_member_count"
				result.Groups[groupIndex].FailureReason = fmt.Sprintf(
					"need %d fully evaluated members; got %d",
					policy.MinGroupMembers, features.ValidMemberCount,
				)
			case features.Coverage < policy.MinGroupCoverage:
				result.Groups[groupIndex].FailureStage = "coverage"
				result.Groups[groupIndex].FailureReason = fmt.Sprintf(
					"coverage %.3f below %.3f", features.Coverage, policy.MinGroupCoverage,
				)
			}
		}
		result.Groups[groupIndex].Archetype = potentIndustryArchetype(features, policy)
		preparedGroups = append(preparedGroups, prepared)
	}

	scorePotentIndustryGroups(&result, preparedGroups, policy)
	rankPotentIndustryMembers(&result, preparedGroups, policy)
	sort.Slice(result.Members, func(i, j int) bool {
		if result.Members[i].Industry == result.Members[j].Industry {
			return result.Members[i].Ticker < result.Members[j].Ticker
		}
		return result.Members[i].Industry < result.Members[j].Industry
	})
	return result
}

func normalizePotentIndustryPolicy(p PotentIndustryScoringPolicy) PotentIndustryScoringPolicy {
	if p.MinHistorySessions < 253 {
		p.MinHistorySessions = 253
	}
	if p.MinGroupMembers <= 0 {
		p.MinGroupMembers = 5
	}
	if p.MinGroupCoverage <= 0 || p.MinGroupCoverage > 1 {
		p.MinGroupCoverage = .80
	}
	if p.MinFreshTaxonomyCoverage <= 0 || p.MinFreshTaxonomyCoverage > 1 {
		p.MinFreshTaxonomyCoverage = 1
	}
	if p.MinPotencyScore <= 0 || p.MinPotencyScore > 1 {
		p.MinPotencyScore = .70
	}
	if p.SelectionPercentile <= 0 || p.SelectionPercentile >= 1 {
		p.SelectionPercentile = .80
	}
	if p.MinAdvanceShare <= 0 || p.MinAdvanceShare > 1 {
		p.MinAdvanceShare = .60
	}
	if p.MinPersistenceSessions <= 0 || p.MinPersistenceSessions > 5 {
		p.MinPersistenceSessions = 3
	}
	if p.MaxCapEqualDivergence <= 0 {
		p.MaxCapEqualDivergence = .08
	}
	if p.MaxMembersPerGroup <= 0 {
		p.MaxMembersPerGroup = 3
	}
	if p.MinMemberScore <= 0 || p.MinMemberScore > 1 {
		p.MinMemberScore = .65
	}
	if p.TaxonomyMaxAge <= 0 {
		p.TaxonomyMaxAge = 7 * 24 * time.Hour
	}
	return p
}

func potentAlignedBenchmark(
	input []data.Candle, target, asOf time.Time, minHistory int,
) ([]data.Candle, error) {
	bars, err := completedDailyBars(input, target, asOf)
	if err != nil {
		return bars, err
	}
	if len(bars) < minHistory {
		return bars, fmt.Errorf("need %d completed sessions; got %d", minHistory, len(bars))
	}
	return bars[len(bars)-minHistory:], nil
}

func potentAlignedMemberBars(
	input, sessions []data.Candle,
	target, asOf time.Time,
	minHistory int,
) ([]data.Candle, error) {
	bars, err := completedDailyBars(input, target, asOf)
	if err != nil {
		return bars, err
	}
	location := target.Location()
	byDate := make(map[string]data.Candle, len(bars))
	for _, bar := range bars {
		byDate[bar.Timestamp.In(location).Format("2006-01-02")] = bar
	}
	if len(sessions) < minHistory {
		return nil, fmt.Errorf("benchmark has fewer than %d aligned sessions", minHistory)
	}
	aligned := make([]data.Candle, 0, minHistory)
	for _, session := range sessions[len(sessions)-minHistory:] {
		key := session.Timestamp.In(location).Format("2006-01-02")
		bar, ok := byDate[key]
		if !ok {
			return aligned, fmt.Errorf("missing completed session %s", key)
		}
		aligned = append(aligned, bar)
	}
	return aligned, nil
}

func potentTaxonomyFailure(
	member store.SwingUniverseMember, decisionAt time.Time, maxAge time.Duration,
) string {
	industry := strings.TrimSpace(member.Industry)
	if industry == "" || strings.EqualFold(industry, store.UnknownTaxonomy) {
		return "industry taxonomy is unavailable"
	}
	if member.TaxonomyStatus != "known" {
		return fmt.Sprintf("taxonomy status is %s", member.TaxonomyStatus)
	}
	if member.TaxonomyAsOf == nil {
		return "taxonomy as-of is unavailable"
	}
	if member.TaxonomyAsOf.After(decisionAt) {
		return "taxonomy became effective after the decision cutoff"
	}
	if maxAge > 0 && member.TaxonomyAsOf.Before(decisionAt.Add(-maxAge)) {
		return "taxonomy is stale at the decision cutoff"
	}
	return ""
}

func potentMemberFeatures(bars []data.Candle) PotentIndustryMemberFeatures {
	n := len(bars)
	ret := func(h int) float64 { return logRatio(bars[n-1].Close, bars[n-1-h].Close) }
	ema20, ema50 := potentEMA(bars, 20), potentEMA(bars, 50)
	_, r2 := logPriceRegression(bars[n-20:])
	high := bars[n-252].High
	for _, bar := range bars[n-252:] {
		high = math.Max(high, bar.High)
	}
	medianDV := potentMedianDollarVolume(bars[n-21 : n-1])
	volumes := make([]float64, 0, 20)
	for _, bar := range bars[n-21 : n-1] {
		volumes = append(volumes, float64(bar.Volume))
	}
	medianVolume := potentMedian(volumes)
	rvol := 0.0
	if medianVolume > 0 {
		rvol = float64(bars[n-1].Volume) / medianVolume
	}
	return PotentIndustryMemberFeatures{
		ClosePrice: bars[n-1].Close,
		Return1:    ret(1), Return5: ret(5), Return20: ret(20),
		EMA20: ema20, EMA50: ema50, RegressionR2: r2,
		Efficiency20:         efficiencyRatio(bars[n-21:]),
		MaxDrawdown20:        closeMaxDrawdown(bars[n-20:]),
		High52WeekProximity:  clamp(bars[n-1].Close/high, 0, 1),
		MedianDollarVolume20: medianDV, RVOL20: rvol,
	}
}

func potentGroupFeatures(
	members []*potentIndustryPreparedMember,
	market, sector []data.Candle,
) PotentIndustryGroupFeatures {
	var f PotentIndustryGroupFeatures
	if len(members) == 0 {
		return f
	}
	returns := map[int][]float64{1: {}, 5: {}, 20: {}}
	capReturns := map[int]float64{1: 0, 5: 0, 20: 0}
	totalCap := 0.0
	var advances, declines int
	var upVolume, downVolume float64
	var above20, above50, high20, high252 int
	rvol, dollarExpansion := make([]float64, 0, len(members)), make([]float64, 0, len(members))
	for _, item := range members {
		bars := item.bars
		bn := len(bars)
		for _, h := range []int{1, 5, 20} {
			value := logRatio(bars[bn-1].Close, bars[bn-1-h].Close)
			returns[h] = append(returns[h], value)
			if item.member.MarketCapMM != nil && *item.member.MarketCapMM > 0 {
				capReturns[h] += value * *item.member.MarketCapMM
			}
		}
		if item.member.MarketCapMM != nil && *item.member.MarketCapMM > 0 {
			totalCap += *item.member.MarketCapMM
		}
		if item.feature.Return1 > 0 {
			advances++
			upVolume += float64(bars[bn-1].Volume)
		} else if item.feature.Return1 < 0 {
			declines++
			downVolume += float64(bars[bn-1].Volume)
		}
		if bars[bn-1].Close > item.feature.EMA20 {
			above20++
		}
		if bars[bn-1].Close > item.feature.EMA50 {
			above50++
		}
		if potentAtClosingHigh(bars[bn-20:]) {
			high20++
		}
		if potentAtClosingHigh(bars[bn-252:]) {
			high252++
		}
		rvol = append(rvol, item.feature.RVOL20)
		currentDV := bars[bn-1].Close * float64(bars[bn-1].Volume)
		expansion := 0.0
		if item.feature.MedianDollarVolume20 > 0 {
			expansion = currentDV / item.feature.MedianDollarVolume20
		}
		dollarExpansion = append(dollarExpansion, expansion)
	}
	f.EqualReturn1, f.EqualReturn5, f.EqualReturn20 =
		potentMean(returns[1]), potentMean(returns[5]), potentMean(returns[20])
	f.MedianReturn1, f.MedianReturn5, f.MedianReturn20 =
		potentMedian(returns[1]), potentMedian(returns[5]), potentMedian(returns[20])
	if totalCap > 0 {
		f.CapReturn1, f.CapReturn5, f.CapReturn20 =
			capReturns[1]/totalCap, capReturns[5]/totalCap, capReturns[20]/totalCap
	} else {
		f.CapReturn1, f.CapReturn5, f.CapReturn20 =
			f.EqualReturn1, f.EqualReturn5, f.EqualReturn20
	}
	if len(market) >= 21 {
		n := len(market)
		marketReturn := func(h int) float64 {
			return logRatio(market[n-1].Close, market[n-1-h].Close)
		}
		f.MarketExcess1, f.MarketExcess5, f.MarketExcess20 =
			f.EqualReturn1-marketReturn(1), f.EqualReturn5-marketReturn(5),
			f.EqualReturn20-marketReturn(20)
	}
	if len(sector) >= 21 {
		n := len(sector)
		sectorReturn := func(h int) float64 {
			return logRatio(sector[n-1].Close, sector[n-1-h].Close)
		}
		f.SectorExcess1, f.SectorExcess5, f.SectorExcess20 =
			f.EqualReturn1-sectorReturn(1), f.EqualReturn5-sectorReturn(5),
			f.EqualReturn20-sectorReturn(20)
	}
	f.AdvanceShare = fraction(advances, len(members))
	f.AdvanceDecline = fraction(advances-declines, len(members))
	if upVolume+downVolume > 0 {
		f.UpVolumeShare = upVolume / (upVolume + downVolume)
	}
	f.AboveEMA20Share, f.AboveEMA50Share =
		fraction(above20, len(members)), fraction(above50, len(members))
	f.AtHigh20Share, f.AtHigh252Share =
		fraction(high20, len(members)), fraction(high252, len(members))
	f.MedianRVOL20, f.MedianDollarExpand20 =
		potentMedian(rvol), potentMedian(dollarExpansion)
	f.BreadthAcceleration = potentBreadthAcceleration(members)
	if len(market) >= 5 && len(sector) >= 5 {
		f.PersistenceSessions = potentPersistence(members, market, sector)
	}
	f.CapEqualDivergence = potentCapLeadershipDivergence(f)
	return f
}

func potentCapLeadershipDivergence(f PotentIndustryGroupFeatures) float64 {
	return math.Max(
		0,
		math.Max(
			f.CapReturn1-f.EqualReturn1,
			math.Max(
				f.CapReturn5-f.EqualReturn5,
				f.CapReturn20-f.EqualReturn20,
			),
		),
	)
}

func potentBreadthAcceleration(members []*potentIndustryPreparedMember) float64 {
	currentAdvance, currentEMA := potentBreadthAt(members, 0)
	priorAdvance, priorEMA := 0.0, 0.0
	for offset := 1; offset <= 5; offset++ {
		advance, ema := potentBreadthAt(members, offset)
		priorAdvance += advance
		priorEMA += ema
	}
	return .5*(currentAdvance-priorAdvance/5) + .5*(currentEMA-priorEMA/5)
}

func potentBreadthAt(
	members []*potentIndustryPreparedMember, offset int,
) (advanceShare, aboveEMA20Share float64) {
	advance, above := 0, 0
	for _, item := range members {
		n := len(item.bars) - offset
		if item.bars[n-1].Close > item.bars[n-2].Close {
			advance++
		}
		if item.bars[n-1].Close > potentEMA(item.bars[:n], 20) {
			above++
		}
	}
	return fraction(advance, len(members)), fraction(above, len(members))
}

func potentPersistence(
	members []*potentIndustryPreparedMember, market, sector []data.Candle,
) int {
	count := 0
	for offset := 0; offset < 5; offset++ {
		values := make([]float64, 0, len(members))
		advances := 0
		for _, item := range members {
			n := len(item.bars) - offset
			value := logRatio(item.bars[n-1].Close, item.bars[n-2].Close)
			values = append(values, value)
			if value > 0 {
				advances++
			}
		}
		mn, sn := len(market)-offset, len(sector)-offset
		marketReturn := logRatio(market[mn-1].Close, market[mn-2].Close)
		sectorReturn := logRatio(sector[sn-1].Close, sector[sn-2].Close)
		equal := potentMean(values)
		if equal > marketReturn && equal > sectorReturn &&
			potentMedian(values) > 0 && fraction(advances, len(members)) >= .5 {
			count++
		}
	}
	return count
}

func potentIndustryArchetype(
	f PotentIndustryGroupFeatures, policy PotentIndustryScoringPolicy,
) string {
	strong := f.MedianReturn5 > policy.MinMedianReturn5 &&
		f.MarketExcess5 > 0 && f.MarketExcess20 > 0 &&
		f.SectorExcess5 > 0 && f.SectorExcess20 > 0
	broad := f.AdvanceShare >= policy.MinAdvanceShare &&
		f.AboveEMA20Share >= .5 &&
		f.PersistenceSessions >= policy.MinPersistenceSessions
	switch {
	case strong && broad:
		return "broad_strong"
	case strong:
		return "strong_narrow"
	case broad:
		return "broad_weak"
	default:
		return "weak"
	}
}

func scorePotentIndustryGroups(
	result *PotentIndustryEvaluation,
	prepared []potentIndustryPreparedGroup,
	policy PotentIndustryScoringPolicy,
) {
	raw := make([]potentIndustryRawComponents, 0, len(prepared))
	for _, group := range prepared {
		f := result.Groups[group.index].Features
		if result.Groups[group.index].FailureStage != "" ||
			f == nil || f.ValidMemberCount == 0 || len(group.marketBars) < 21 ||
			len(group.sectorBars) < 21 {
			continue
		}
		strength := .15*potentRelativeLevel(f.MarketExcess1, f.SectorExcess1, 1) +
			.35*potentRelativeLevel(f.MarketExcess5, f.SectorExcess5, 5) +
			.50*potentRelativeLevel(f.MarketExcess20, f.SectorExcess20, 20)
		breadth := .25*f.AdvanceShare + .10*f.UpVolumeShare +
			.20*f.AboveEMA20Share + .15*f.AboveEMA50Share +
			.15*f.AtHigh20Share + .15*f.AtHigh252Share
		volume := .5*clamp(f.MedianRVOL20/2, 0, 1) +
			.5*clamp(f.MedianDollarExpand20/2, 0, 1)
		acceleration := clamp(.5+f.BreadthAcceleration, 0, 1)
		persistence := float64(f.PersistenceSessions) / 5
		penalty := 1 - .35*clamp(
			f.CapEqualDivergence/policy.MaxCapEqualDivergence, 0, 1,
		)
		raw = append(raw, potentIndustryRawComponents{index: group.index, value: PotentIndustryGroupComponents{
			Strength: strength, Breadth: breadth, VolumeExpansion: volume,
			Acceleration: acceleration, Persistence: persistence,
			DominancePenalty: penalty,
		}})
	}
	// Normalize explicitly to keep the component-to-field mapping auditable.
	strengthPct := potentComponentPercentiles(raw,
		func(c PotentIndustryGroupComponents) float64 { return c.Strength })
	breadthPct := potentComponentPercentiles(raw,
		func(c PotentIndustryGroupComponents) float64 { return c.Breadth })
	volumePct := potentComponentPercentiles(raw,
		func(c PotentIndustryGroupComponents) float64 { return c.VolumeExpansion })
	accelPct := potentComponentPercentiles(raw,
		func(c PotentIndustryGroupComponents) float64 { return c.Acceleration })
	persistPct := potentComponentPercentiles(raw,
		func(c PotentIndustryGroupComponents) float64 { return c.Persistence })
	type ranked struct {
		index int
		score float64
	}
	ranking := make([]ranked, 0, len(raw))
	for _, item := range raw {
		c := item.value
		c.Strength = .5*c.Strength + .5*strengthPct[item.index]
		c.Breadth = .5*c.Breadth + .5*breadthPct[item.index]
		c.VolumeExpansion = .5*c.VolumeExpansion + .5*volumePct[item.index]
		c.Acceleration = .5*c.Acceleration + .5*accelPct[item.index]
		c.Persistence = .5*c.Persistence + .5*persistPct[item.index]
		score := (.30*c.Strength + .35*c.Breadth + .10*c.VolumeExpansion +
			.10*c.Acceleration + .15*c.Persistence) * c.DominancePenalty
		result.Groups[item.index].Components = &c
		result.Groups[item.index].PotencyScore = floatPtr(score)
		ranking = append(ranking, ranked{index: item.index, score: score})
	}
	sort.Slice(ranking, func(i, j int) bool {
		if ranking[i].score == ranking[j].score {
			return result.Groups[ranking[i].index].Industry <
				result.Groups[ranking[j].index].Industry
		}
		return ranking[i].score > ranking[j].score
	})
	for start := 0; start < len(ranking); {
		end := start + 1
		for end < len(ranking) && ranking[end].score == ranking[start].score {
			end++
		}
		percentile := 1.0
		if len(ranking) > 1 {
			percentile = 1 - float64(start+end-1)/2/float64(len(ranking)-1)
		}
		for i := start; i < end; i++ {
			group := &result.Groups[ranking[i].index]
			group.Rank, group.ScorePercentile = intPtr(i+1), floatPtr(percentile)
			if group.FailureStage == "" {
				switch {
				case group.Archetype != "broad_strong":
					group.Outcome = "not_qualified"
					group.FailureStage = "breadth_strength_gate"
					group.FailureReason = "group is " + group.Archetype
				case ranking[i].score < policy.MinPotencyScore ||
					percentile < policy.SelectionPercentile:
					group.Outcome = "not_qualified"
					group.FailureStage = "score_threshold"
					group.FailureReason = fmt.Sprintf(
						"score %.4f/percentile %.4f below %.4f/%.4f",
						ranking[i].score, percentile, policy.MinPotencyScore,
						policy.SelectionPercentile,
					)
				default:
					group.Outcome, group.Qualified = "qualified", true
				}
			}
		}
		start = end
	}
}

func potentComponentPercentiles(
	items []potentIndustryRawComponents,
	metric func(PotentIndustryGroupComponents) float64,
) map[int]float64 {
	type value struct {
		index int
		score float64
	}
	values := make([]value, 0, len(items))
	for _, item := range items {
		values = append(values, value{item.index, metric(item.value)})
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].score == values[j].score {
			return values[i].index < values[j].index
		}
		return values[i].score < values[j].score
	})
	out := make(map[int]float64, len(values))
	if len(values) == 1 {
		out[values[0].index] = .5
		return out
	}
	for start := 0; start < len(values); {
		end := start + 1
		for end < len(values) && values[end].score == values[start].score {
			end++
		}
		p := float64(start+end-1) / 2 / float64(len(values)-1)
		for i := start; i < end; i++ {
			out[values[i].index] = p
		}
		start = end
	}
	return out
}

func rankPotentIndustryMembers(
	result *PotentIndustryEvaluation,
	groups []potentIndustryPreparedGroup,
	policy PotentIndustryScoringPolicy,
) {
	memberByTicker := make(map[string]*PotentIndustryMemberEvaluation, len(result.Members))
	for i := range result.Members {
		memberByTicker[result.Members[i].Ticker] = &result.Members[i]
	}
	marketReturns := [3]float64{}
	for _, group := range groups {
		if len(group.marketBars) >= 21 {
			n := len(group.marketBars)
			marketReturns = [3]float64{
				logRatio(group.marketBars[n-1].Close, group.marketBars[n-2].Close),
				logRatio(group.marketBars[n-1].Close, group.marketBars[n-6].Close),
				logRatio(group.marketBars[n-1].Close, group.marketBars[n-21].Close),
			}
			break
		}
	}
	for _, prepared := range groups {
		group := &result.Groups[prepared.index]
		if !group.Qualified || group.Features == nil {
			for _, item := range prepared.members {
				evaluation := memberByTicker[item.member.Ticker]
				if evaluation == nil || evaluation.Outcome != "not_ranked" {
					continue
				}
				evaluation.Outcome = "not_qualified"
				evaluation.FailureStage = "group_not_qualified"
				evaluation.FailureReason = fmt.Sprintf(
					"industry group %q did not qualify at %s: %s",
					group.Industry, group.FailureStage, group.FailureReason,
				)
			}
			continue
		}
		type ranked struct {
			item       *potentIndustryPreparedMember
			score      float64
			components PotentIndustryMemberComponents
		}
		rows := make([]ranked, 0, len(prepared.members))
		industryReturns := [3]float64{
			group.Features.EqualReturn1, group.Features.EqualReturn5,
			group.Features.EqualReturn20,
		}
		industryRaw, marketRaw, trendRaw, liquidityRaw := make(map[string]float64),
			make(map[string]float64), make(map[string]float64), make(map[string]float64)
		for _, item := range prepared.members {
			f := &item.feature
			returns := [3]float64{f.Return1, f.Return5, f.Return20}
			peerReturns := industryReturns
			if len(prepared.members) > 1 {
				for i := range peerReturns {
					peerReturns[i] = (industryReturns[i]*float64(len(prepared.members)) -
						returns[i]) / float64(len(prepared.members)-1)
				}
			}
			f.IndustryExcess1, f.IndustryExcess5, f.IndustryExcess20 =
				returns[0]-peerReturns[0], returns[1]-peerReturns[1],
				returns[2]-peerReturns[2]
			f.MarketExcess1, f.MarketExcess5, f.MarketExcess20 =
				returns[0]-marketReturns[0], returns[1]-marketReturns[1],
				returns[2]-marketReturns[2]
			industryRaw[item.member.Ticker] = .15*potentExcessLevel(f.IndustryExcess1, 1) +
				.35*potentExcessLevel(f.IndustryExcess5, 5) +
				.50*potentExcessLevel(f.IndustryExcess20, 20)
			marketRaw[item.member.Ticker] = .15*potentExcessLevel(f.MarketExcess1, 1) +
				.35*potentExcessLevel(f.MarketExcess5, 5) +
				.50*potentExcessLevel(f.MarketExcess20, 20)
			close := item.bars[len(item.bars)-1].Close
			alignment := 0.0
			if close > f.EMA20 {
				alignment = .5
				if f.EMA20 > f.EMA50 {
					alignment = 1
				}
			}
			trendRaw[item.member.Ticker] = potentMean([]float64{
				alignment, f.RegressionR2, f.Efficiency20,
				clamp(1-f.MaxDrawdown20/.20, 0, 1), f.High52WeekProximity,
			})
		}
		dvPct := potentMemberPercentiles(prepared.members,
			func(m *potentIndustryPreparedMember) float64 {
				return m.feature.MedianDollarVolume20
			})
		rvolPct := potentMemberPercentiles(prepared.members,
			func(m *potentIndustryPreparedMember) float64 { return m.feature.RVOL20 })
		industryPct := potentStringPercentiles(industryRaw)
		marketPct := potentStringPercentiles(marketRaw)
		trendPct := potentStringPercentiles(trendRaw)
		for _, item := range prepared.members {
			ticker := item.member.Ticker
			industryScore := .5*industryRaw[ticker] + .5*industryPct[ticker]
			marketScore := .5*marketRaw[ticker] + .5*marketPct[ticker]
			trendScore := .5*trendRaw[ticker] + .5*trendPct[ticker]
			liquidityRaw[ticker] = .5*clamp(item.feature.RVOL20/2, 0, 1) +
				.5*dvPct[ticker]
			liquidity := .5*liquidityRaw[ticker] + .5*rvolPct[ticker]
			components := PotentIndustryMemberComponents{
				GroupStrength: *group.PotencyScore, IndustryRS: industryScore,
				MarketRS: marketScore, TrendQuality: trendScore, Liquidity: liquidity,
			}
			score := .20*components.GroupStrength + .30*components.IndustryRS +
				.25*components.MarketRS + .15*components.TrendQuality +
				.10*components.Liquidity
			rows = append(rows, ranked{item: item, score: score, components: components})
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].score == rows[j].score {
				return rows[i].item.member.Ticker < rows[j].item.member.Ticker
			}
			return rows[i].score > rows[j].score
		})
		for i := range rows {
			evaluation := memberByTicker[rows[i].item.member.Ticker]
			evaluation.Features = &rows[i].item.feature
			evaluation.Components = &rows[i].components
			evaluation.MemberScore = floatPtr(rows[i].score)
			evaluation.Rank = intPtr(i + 1)
			if i < policy.MaxMembersPerGroup &&
				rows[i].score >= policy.MinMemberScore &&
				evaluation.Features.IndustryExcess5 > 0 &&
				evaluation.Features.IndustryExcess20 > 0 {
				evaluation.Outcome, evaluation.Qualified = "qualified", true
			} else {
				evaluation.Outcome = "not_qualified"
				if rows[i].score < policy.MinMemberScore {
					evaluation.FailureStage = "member_score"
					evaluation.FailureReason = fmt.Sprintf(
						"member score %.4f below %.4f",
						rows[i].score, policy.MinMemberScore,
					)
				} else if evaluation.Features.IndustryExcess5 <= 0 ||
					evaluation.Features.IndustryExcess20 <= 0 {
					evaluation.FailureStage = "stock_industry_strength"
					evaluation.FailureReason =
						"5-session and 20-session stock-vs-industry strength must be positive"
				} else {
					evaluation.FailureStage = "member_rank"
					evaluation.FailureReason = fmt.Sprintf(
						"ranked beyond top %d members", policy.MaxMembersPerGroup,
					)
				}
			}
		}
	}
}

func resolvePotentBenchmark(
	industry, sector string, p PotentIndustryScoringPolicy,
) string {
	if symbol := potentBenchmarkMapValue(p.IndustryBenchmarks, industry); symbol != "" {
		return strings.ToUpper(strings.TrimSpace(symbol))
	}
	return strings.ToUpper(strings.TrimSpace(
		potentBenchmarkMapValue(p.SectorBenchmarks, sector),
	))
}

func potentBenchmarkMapValue(values map[string]string, label string) string {
	normalized := normalizePotentLabel(label)
	if value := values[normalized]; value != "" {
		return value
	}
	for key, value := range values {
		if normalizePotentLabel(key) == normalized {
			return value
		}
	}
	return ""
}

func potentGroupSector(members []store.SwingUniverseMember) (string, bool) {
	counts := make(map[string]int)
	labels := make(map[string]string)
	for _, member := range members {
		sector := strings.TrimSpace(member.Sector)
		if sector == "" || strings.EqualFold(sector, store.UnknownTaxonomy) {
			continue
		}
		key := normalizePotentLabel(sector)
		counts[key]++
		labels[key] = sector
	}
	ambiguous := len(counts) > 1
	best, bestCount := "", 0
	for key, count := range counts {
		if count > bestCount || count == bestCount && key < best {
			best, bestCount = key, count
		}
	}
	if best == "" {
		return store.UnknownTaxonomy, false
	}
	return labels[best], ambiguous
}

func normalizePotentLabel(value string) string {
	return strings.ToUpper(strings.Join(strings.Fields(value), " "))
}

func potentRelativeLevel(market, sector float64, sessions int) float64 {
	return .5*potentExcessLevel(market, sessions) +
		.5*potentExcessLevel(sector, sessions)
}

func potentExcessLevel(value float64, sessions int) float64 {
	if sessions <= 0 {
		return .5
	}
	annualized := value * 252 / float64(sessions)
	return .5 + .5*math.Tanh(annualized)
}

func potentEMA(bars []data.Candle, period int) float64 {
	if len(bars) == 0 {
		return 0
	}
	if period <= 1 {
		return bars[len(bars)-1].Close
	}
	if len(bars) < period {
		value := 0.0
		for _, bar := range bars {
			value += bar.Close
		}
		return value / float64(len(bars))
	}
	value := 0.0
	for _, bar := range bars[:period] {
		value += bar.Close
	}
	value /= float64(period)
	alpha := 2.0 / float64(period+1)
	for _, bar := range bars[period:] {
		value = alpha*bar.Close + (1-alpha)*value
	}
	return value
}

func potentAtClosingHigh(bars []data.Candle) bool {
	if len(bars) == 0 {
		return false
	}
	close := bars[len(bars)-1].Close
	for _, bar := range bars[:len(bars)-1] {
		if close < bar.Close {
			return false
		}
	}
	return true
}

func potentMean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func potentMedian(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

func potentMedianDollarVolume(bars []data.Candle) float64 {
	values := make([]float64, 0, len(bars))
	for _, bar := range bars {
		values = append(values, bar.Close*float64(bar.Volume))
	}
	return potentMedian(values)
}

func potentMemberPercentiles(
	members []*potentIndustryPreparedMember,
	value func(*potentIndustryPreparedMember) float64,
) map[string]float64 {
	raw := make(map[string]float64, len(members))
	for _, member := range members {
		raw[member.member.Ticker] = value(member)
	}
	return potentStringPercentiles(raw)
}

func potentStringPercentiles(raw map[string]float64) map[string]float64 {
	type item struct {
		ticker string
		value  float64
	}
	items := make([]item, 0, len(raw))
	for ticker, value := range raw {
		items = append(items, item{ticker, value})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].value == items[j].value {
			return items[i].ticker < items[j].ticker
		}
		return items[i].value < items[j].value
	})
	out := make(map[string]float64, len(items))
	if len(items) == 1 {
		out[items[0].ticker] = .5
		return out
	}
	for start := 0; start < len(items); {
		end := start + 1
		for end < len(items) && items[end].value == items[start].value {
			end++
		}
		p := float64(start+end-1) / 2 / float64(len(items)-1)
		for i := start; i < end; i++ {
			out[items[i].ticker] = p
		}
		start = end
	}
	return out
}
