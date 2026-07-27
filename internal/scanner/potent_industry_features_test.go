package scanner

import (
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestEvaluatePotentIndustriesBroadStrengthAndMemberRanking(t *testing.T) {
	target, asOf, decisionAt := potentTestTimes()
	market := potentTestBars("SPY", target, 100, constantPotentReturns(253, .0002), 1)
	sector := potentTestBars("XLK", target, 100, constantPotentReturns(253, .0004), 1)
	members := potentTestMembers(decisionAt, "Software", "Technology", []float64{
		2_000, 1_500, 1_000, 800, 600,
	})
	bars := make(map[string][]data.Candle, len(members))
	for i, member := range members {
		bars[member.Ticker] = potentTestBars(
			member.Ticker, target, 30+float64(i),
			constantPotentReturns(253, .0012+.00025*float64(i)), 1.6,
		)
	}
	policy := potentTestPolicy()
	policy.MaxMembersPerGroup = 2
	got := EvaluatePotentIndustries(
		members, bars, market, map[string][]data.Candle{"XLK": sector},
		target, asOf, decisionAt, policy,
	)
	if len(got.Groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(got.Groups))
	}
	group := got.Groups[0]
	if !group.Qualified || group.Outcome != "qualified" ||
		group.Archetype != "broad_strong" {
		t.Fatalf("broad strong group did not qualify: %+v", group)
	}
	if group.Features == nil || group.Components == nil || group.PotencyScore == nil {
		t.Fatalf("group diagnostics incomplete: %+v", group)
	}
	if group.Features.AdvanceShare != 1 || group.Features.PersistenceSessions != 5 ||
		group.Features.MarketExcess20 <= 0 || group.Features.SectorExcess20 <= 0 {
		t.Fatalf("unexpected breadth/strength evidence: %+v", *group.Features)
	}
	if *group.PotencyScore < policy.MinPotencyScore {
		t.Fatalf("potency %.4f below configured floor %.4f",
			*group.PotencyScore, policy.MinPotencyScore)
	}

	qualified := 0
	var leader *PotentIndustryMemberEvaluation
	for i := range got.Members {
		member := &got.Members[i]
		if member.Qualified {
			qualified++
		}
		if member.Rank != nil && *member.Rank == 1 {
			leader = member
		}
	}
	if qualified != 2 {
		t.Fatalf("qualified members = %d, want 2: %+v", qualified, got.Members)
	}
	if leader == nil || leader.Ticker != "EIE" || leader.Components == nil ||
		leader.Features == nil {
		t.Fatalf("highest relative-strength member was not ranked first: %+v", leader)
	}
	if leader.Features.IndustryExcess20 <= 0 || leader.Features.MarketExcess20 <= 0 {
		t.Fatalf("leader relative-strength evidence missing: %+v", *leader.Features)
	}
}

func TestEvaluatePotentIndustriesMegaCapCannotQualifyWeakGroup(t *testing.T) {
	target, asOf, decisionAt := potentTestTimes()
	market := potentTestBars("SPY", target, 100, constantPotentReturns(253, .0002), 1)
	sector := potentTestBars("XLI", target, 100, constantPotentReturns(253, .0002), 1)
	caps := []float64{1_000_000, 100, 100, 100, 100}
	members := potentTestMembers(decisionAt, "Machinery", "Industrials", caps)
	bars := make(map[string][]data.Candle, len(members))
	for i, member := range members {
		daily := -.004
		if i == 0 {
			daily = .012
		}
		bars[member.Ticker] = potentTestBars(
			member.Ticker, target, 50, constantPotentReturns(253, daily), 1.2,
		)
	}
	got := EvaluatePotentIndustries(
		members, bars, market, map[string][]data.Candle{"XLI": sector},
		target, asOf, decisionAt, potentTestPolicy(),
	)
	group := got.Groups[0]
	if group.Qualified {
		t.Fatalf("mega-cap-dominated weak group qualified: %+v", group)
	}
	if group.Features == nil || group.Features.CapReturn20 <= 0 ||
		group.Features.EqualReturn20 >= 0 || group.Features.MedianReturn5 >= 0 {
		t.Fatalf("fixture did not distinguish cap strength from broad weakness: %+v",
			group.Features)
	}
	if group.Archetype != "weak" {
		t.Fatalf("archetype = %q, want weak", group.Archetype)
	}
	if group.Components == nil || group.Components.DominancePenalty >= 1 {
		t.Fatalf("dominance penalty was not applied: %+v", group.Components)
	}
	for _, member := range got.Members {
		if member.Rank != nil || member.Qualified {
			t.Fatalf("member ranked before group qualification: %+v", member)
		}
	}
}

func TestPotentCapLeadershipDivergenceDoesNotPenalizeEqualWeightLeadership(
	t *testing.T,
) {
	features := PotentIndustryGroupFeatures{
		EqualReturn1: .02, EqualReturn5: .08, EqualReturn20: .15,
		CapReturn1: .01, CapReturn5: .04, CapReturn20: .10,
	}
	if got := potentCapLeadershipDivergence(features); got != 0 {
		t.Fatalf("equal-weight leadership was treated as mega-cap dominance: %v", got)
	}
	features.CapReturn20 = .22
	if got := potentCapLeadershipDivergence(features); math.Abs(got-.07) > 1e-12 {
		t.Fatalf("positive cap-weight leadership divergence = %v, want .07", got)
	}
}

func TestPotentIndustryArchetypesDistinguishNarrowAndWeak(t *testing.T) {
	policy := potentTestPolicy()
	base := PotentIndustryGroupFeatures{
		MedianReturn5: .03, MarketExcess5: .02, MarketExcess20: .04,
		SectorExcess5: .01, SectorExcess20: .03,
		AdvanceShare: .8, AboveEMA20Share: .8, PersistenceSessions: 4,
	}
	if got := potentIndustryArchetype(base, policy); got != "broad_strong" {
		t.Fatalf("broad strong = %q", got)
	}
	narrow := base
	narrow.AdvanceShare = .4
	if got := potentIndustryArchetype(narrow, policy); got != "strong_narrow" {
		t.Fatalf("strong narrow = %q", got)
	}
	broadWeak := base
	broadWeak.MarketExcess20 = -.01
	if got := potentIndustryArchetype(broadWeak, policy); got != "broad_weak" {
		t.Fatalf("broad weak = %q", got)
	}
	weak := broadWeak
	weak.PersistenceSessions = 1
	if got := potentIndustryArchetype(weak, policy); got != "weak" {
		t.Fatalf("weak = %q", got)
	}
}

func TestEvaluatePotentIndustriesFailsClosedOnCoverageAndTaxonomy(t *testing.T) {
	target, asOf, decisionAt := potentTestTimes()
	market := potentTestBars("SPY", target, 100, constantPotentReturns(253, .0001), 1)
	sector := potentTestBars("XLV", target, 100, constantPotentReturns(253, .0001), 1)
	members := potentTestMembers(
		decisionAt, "Biotechnology", "Health Care",
		[]float64{1000, 900, 800, 700, 600},
	)
	bars := make(map[string][]data.Candle, len(members))
	for _, member := range members {
		bars[member.Ticker] = potentTestBars(
			member.Ticker, target, 20, constantPotentReturns(253, .002), 1.5,
		)
	}
	future := decisionAt.Add(time.Minute)
	members[0].TaxonomyAsOf = &future
	stale := decisionAt.Add(-8 * 24 * time.Hour)
	members[1].TaxonomyAsOf = &stale
	policy := potentTestPolicy()
	policy.MinGroupCoverage = .50
	policy.MinFreshTaxonomyCoverage = .80
	got := EvaluatePotentIndustries(
		members, bars, market, map[string][]data.Candle{"XLV": sector},
		target, asOf, decisionAt, policy,
	)
	group := got.Groups[0]
	if group.FailureStage != "taxonomy_coverage" || group.Qualified ||
		group.Features == nil || group.Features.Coverage != .6 ||
		group.Features.FreshTaxonomyCoverage != .6 {
		t.Fatalf("invalid taxonomy did not fail fresh-taxonomy coverage closed: %+v", group)
	}
	failures := 0
	for _, member := range got.Members {
		if member.FailureStage == "taxonomy" {
			failures++
		}
	}
	if failures != 2 {
		t.Fatalf("taxonomy failures = %d, want 2: %+v", failures, got.Members)
	}
}

func TestEvaluatePotentIndustriesDerivesSectorOnlyFromFreshTaxonomy(t *testing.T) {
	target, asOf, decisionAt := potentTestTimes()
	market := potentTestBars("SPY", target, 100, constantPotentReturns(253, .0001), 1)
	sector := potentTestBars("XLK", target, 100, constantPotentReturns(253, .0001), 1)
	members := potentTestMembers(
		decisionAt, "Software", store.UnknownTaxonomy,
		[]float64{1000, 900, 800, 700, 600, 500},
	)
	stale := decisionAt.Add(-8 * 24 * time.Hour)
	members[5].Sector = "Technology"
	members[5].TaxonomyAsOf = &stale
	bars := make(map[string][]data.Candle, len(members))
	for _, member := range members {
		bars[member.Ticker] = potentTestBars(
			member.Ticker, target, 20, constantPotentReturns(253, .002), 1.5,
		)
	}
	policy := potentTestPolicy()
	policy.MinGroupCoverage = .8
	policy.MinFreshTaxonomyCoverage = .8
	unmapped := EvaluatePotentIndustries(
		members, bars, market, map[string][]data.Candle{"XLK": sector},
		target, asOf, decisionAt, policy,
	)
	if group := unmapped.Groups[0]; group.Sector != store.UnknownTaxonomy ||
		group.Benchmark != "" || group.FailureStage != "benchmark_unmapped" {
		t.Fatalf("stale taxonomy selected the sector benchmark: %+v", group)
	}

	for i := range members[:5] {
		members[i].Sector = "Technology"
	}
	members[5].Sector = "Financials"
	freshConsensus := EvaluatePotentIndustries(
		members, bars, market, map[string][]data.Candle{"XLK": sector},
		target, asOf, decisionAt, policy,
	)
	if group := freshConsensus.Groups[0]; group.Sector != "Technology" ||
		group.Benchmark != "XLK" || group.FailureStage == "sector_taxonomy" {
		t.Fatalf("stale conflict contaminated fresh sector consensus: %+v", group)
	}
}

func TestEvaluatePotentIndustriesRetainsUnknownTaxonomyDenominator(t *testing.T) {
	target, asOf, decisionAt := potentTestTimes()
	market := potentTestBars("SPY", target, 100, constantPotentReturns(253, .0001), 1)
	member := potentTestMembers(
		decisionAt, "", store.UnknownTaxonomy, []float64{1000},
	)[0]
	member.TaxonomyStatus = "unknown"

	got := EvaluatePotentIndustries(
		[]store.SwingUniverseMember{member}, nil, market, nil,
		target, asOf, decisionAt, potentTestPolicy(),
	)
	if len(got.Groups) != 1 || got.Groups[0].Industry != store.UnknownTaxonomy ||
		got.Groups[0].FailureStage != "taxonomy" ||
		got.Groups[0].Features == nil || got.Groups[0].Features.MemberCount != 1 ||
		got.Groups[0].Features.FreshTaxonomyCoverage != 0 {
		t.Fatalf("unknown taxonomy group denominator was lost: %+v", got.Groups)
	}
	if len(got.Members) != 1 || got.Members[0].Industry != store.UnknownTaxonomy ||
		got.Members[0].FailureStage != "taxonomy" {
		t.Fatalf("unknown taxonomy member evidence was lost: %+v", got.Members)
	}
}

func TestEvaluatePotentIndustriesRequiresMappedCompletedBenchmarks(t *testing.T) {
	target, asOf, decisionAt := potentTestTimes()
	market := potentTestBars("SPY", target, 100, constantPotentReturns(253, .0001), 1)
	members := potentTestMembers(
		decisionAt, "Banks", "Financials", []float64{1000, 900, 800, 700, 600},
	)
	bars := make(map[string][]data.Candle, len(members))
	for _, member := range members {
		bars[member.Ticker] = potentTestBars(
			member.Ticker, target, 20, constantPotentReturns(253, .002), 1.5,
		)
	}
	policy := potentTestPolicy()
	delete(policy.SectorBenchmarks, "FINANCIALS")
	unmapped := EvaluatePotentIndustries(
		members, bars, market, nil, target, asOf, decisionAt, policy,
	)
	if unmapped.Groups[0].FailureStage != "benchmark_unmapped" ||
		unmapped.Groups[0].Qualified {
		t.Fatalf("unmapped benchmark did not fail closed: %+v", unmapped.Groups[0])
	}
	if unmapped.Groups[0].Features == nil ||
		unmapped.Groups[0].Features.AdvanceShare != 1 {
		t.Fatalf("available breadth diagnostics were erased by missing benchmark: %+v",
			unmapped.Groups[0].Features)
	}
	for _, member := range unmapped.Members {
		if member.Outcome != "not_qualified" ||
			member.FailureStage != "group_not_qualified" ||
			!strings.Contains(member.FailureReason, "benchmark_unmapped") {
			t.Fatalf("unmapped group left an unpersistable member outcome: %+v", member)
		}
	}

	policy.SectorBenchmarks["FINANCIALS"] = "XLF"
	missingTarget := potentTestBars(
		"XLF", target.AddDate(0, 0, -1), 100, constantPotentReturns(253, .0001), 1,
	)
	missing := EvaluatePotentIndustries(
		members, bars, market, map[string][]data.Candle{"XLF": missingTarget},
		target, asOf, decisionAt, policy,
	)
	if missing.Groups[0].FailureStage != "sector_benchmark" ||
		missing.Groups[0].Qualified {
		t.Fatalf("incomplete benchmark did not fail closed: %+v", missing.Groups[0])
	}
}

func TestEvaluatePotentIndustriesRequiresExactAlignedMemberHistory(t *testing.T) {
	target, asOf, decisionAt := potentTestTimes()
	market := potentTestBars("SPY", target, 100, constantPotentReturns(253, .0001), 1)
	sector := potentTestBars("XLB", target, 100, constantPotentReturns(253, .0002), 1)
	members := potentTestMembers(
		decisionAt, "Chemicals", "Materials", []float64{1000, 900, 800, 700, 600},
	)
	bars := make(map[string][]data.Candle, len(members))
	for _, member := range members {
		bars[member.Ticker] = potentTestBars(
			member.Ticker, target, 30, constantPotentReturns(253, .002), 1.5,
		)
	}
	// Keep the target bar but remove one benchmark session from a constituent.
	bars[members[0].Ticker] = append(
		append([]data.Candle(nil), bars[members[0].Ticker][:100]...),
		bars[members[0].Ticker][101:]...,
	)
	policy := potentTestPolicy()
	policy.SectorBenchmarks["MATERIALS"] = "XLB"
	policy.MinGroupCoverage = 1
	got := EvaluatePotentIndustries(
		members, bars, market, map[string][]data.Candle{"XLB": sector},
		target, asOf, decisionAt, policy,
	)
	if got.Groups[0].FailureStage != "evaluated_member_count" || got.Groups[0].Qualified {
		t.Fatalf("missing aligned session did not fail closed: %+v", got.Groups[0])
	}
	for _, member := range got.Members {
		if member.Ticker == members[0].Ticker && member.FailureStage != "history" {
			t.Fatalf("missing session failure not attributed to member: %+v", member)
		}
	}
}

func TestEvaluatePotentIndustriesIgnoresFutureBars(t *testing.T) {
	target, asOf, decisionAt := potentTestTimes()
	market := potentTestBars("SPY", target, 100, constantPotentReturns(253, .0001), 1)
	sector := potentTestBars("XLE", target, 100, constantPotentReturns(253, .0002), 1)
	members := potentTestMembers(
		decisionAt, "Oil & Gas", "Energy", []float64{1000, 900, 800, 700, 600},
	)
	baseBars := make(map[string][]data.Candle, len(members))
	futureBars := make(map[string][]data.Candle, len(members))
	for _, member := range members {
		history := potentTestBars(
			member.Ticker, target, 30, constantPotentReturns(253, .002), 1.5,
		)
		baseBars[member.Ticker] = history
		future := history[len(history)-1]
		future.Timestamp = asOf.Add(24 * time.Hour)
		future.Open, future.Low, future.High, future.Close =
			future.Close, future.Close, future.Close*100, future.Close*100
		futureBars[member.Ticker] = append(append([]data.Candle(nil), history...), future)
	}
	policy := potentTestPolicy()
	first := EvaluatePotentIndustries(
		members, baseBars, market, map[string][]data.Candle{"XLE": sector},
		target, asOf, decisionAt, policy,
	)
	second := EvaluatePotentIndustries(
		members, futureBars, market, map[string][]data.Candle{"XLE": sector},
		target, asOf, decisionAt, policy,
	)
	if len(first.Groups) != 1 || len(second.Groups) != 1 ||
		*first.Groups[0].PotencyScore != *second.Groups[0].PotencyScore {
		t.Fatalf("future bar changed score: first=%+v second=%+v",
			first.Groups, second.Groups)
	}
	for i := range first.Members {
		if *first.Members[i].MemberScore != *second.Members[i].MemberScore {
			t.Fatalf("future bar changed %s member score", first.Members[i].Ticker)
		}
	}
}

func TestEvaluatePotentIndustriesIsOrderDeterministic(t *testing.T) {
	target, asOf, decisionAt := potentTestTimes()
	market := potentTestBars("SPY", target, 100, constantPotentReturns(253, .0001), 1)
	sector := potentTestBars("XLY", target, 100, constantPotentReturns(253, .0002), 1)
	members := potentTestMembers(
		decisionAt, "Retail", "Consumer Discretionary",
		[]float64{1000, 900, 800, 700, 600},
	)
	bars := make(map[string][]data.Candle, len(members))
	for _, member := range members {
		bars[member.Ticker] = potentTestBars(
			member.Ticker, target, 30, constantPotentReturns(253, .002), 1.5,
		)
	}
	first := EvaluatePotentIndustries(
		members, bars, market, map[string][]data.Candle{"XLY": sector},
		target, asOf, decisionAt, potentTestPolicy(),
	)
	rand.New(rand.NewSource(7)).Shuffle(len(members), func(i, j int) {
		members[i], members[j] = members[j], members[i]
	})
	second := EvaluatePotentIndustries(
		members, bars, market, map[string][]data.Candle{"XLY": sector},
		target, asOf, decisionAt, potentTestPolicy(),
	)
	if *first.Groups[0].PotencyScore != *second.Groups[0].PotencyScore {
		t.Fatalf("group score depends on input order")
	}
	for i := range first.Members {
		if first.Members[i].Ticker != second.Members[i].Ticker ||
			!potentEqualFloatPointers(
				first.Members[i].MemberScore, second.Members[i].MemberScore,
			) ||
			!potentEqualIntPointers(first.Members[i].Rank, second.Members[i].Rank) {
			t.Fatalf("member order/rank is nondeterministic:\n%+v\n%+v",
				first.Members, second.Members)
		}
	}
}

func TestEvaluatePotentIndustriesEnforcesMemberScoreFloor(t *testing.T) {
	target, asOf, decisionAt := potentTestTimes()
	market := potentTestBars("SPY", target, 100, constantPotentReturns(253, .0001), 1)
	sector := potentTestBars("XLK", target, 100, constantPotentReturns(253, .0002), 1)
	members := potentTestMembers(
		decisionAt, "Semiconductors", "Technology",
		[]float64{1000, 900, 800, 700, 600},
	)
	bars := make(map[string][]data.Candle, len(members))
	for i, member := range members {
		bars[member.Ticker] = potentTestBars(
			member.Ticker, target, 30, constantPotentReturns(
				253, .0015+.0001*float64(i),
			), 1.5,
		)
	}
	policy := potentTestPolicy()
	policy.MinMemberScore = .99
	got := EvaluatePotentIndustries(
		members, bars, market, map[string][]data.Candle{"XLK": sector},
		target, asOf, decisionAt, policy,
	)
	if !got.Groups[0].Qualified {
		t.Fatalf("member threshold fixture group did not qualify: %+v", got.Groups[0])
	}
	foundScoreFailure := false
	for _, member := range got.Members {
		if member.Qualified {
			t.Fatalf("member below configured score floor qualified: %+v", member)
		}
		if member.FailureStage == "member_score" {
			foundScoreFailure = true
		}
		if member.Rank == nil || member.MemberScore == nil {
			t.Fatalf("score-floor rejection lost diagnostic rank/score: %+v", member)
		}
	}
	if !foundScoreFailure {
		t.Fatalf("member score threshold was not recorded: %+v", got.Members)
	}
}

func potentEqualFloatPointers(left, right *float64) bool {
	return left == nil && right == nil ||
		left != nil && right != nil && *left == *right
}

func potentEqualIntPointers(left, right *int) bool {
	return left == nil && right == nil ||
		left != nil && right != nil && *left == *right
}

func TestPotentIndustryExactHighAndPercentileTies(t *testing.T) {
	target, _, _ := potentTestTimes()
	bars := potentTestBars("AAA", target, 100, constantPotentReturns(20, .001), 1)
	if !potentAtClosingHigh(bars) {
		t.Fatal("new closing high was not recognized")
	}
	bars[len(bars)-2].Close = bars[len(bars)-1].Close + .01
	if potentAtClosingHigh(bars) {
		t.Fatal("close below the prior high was treated as at-high")
	}
	got := potentStringPercentiles(map[string]float64{"BBB": 1, "AAA": 1, "CCC": 2})
	if got["AAA"] != .25 || got["BBB"] != .25 || got["CCC"] != 1 {
		t.Fatalf("midrank tie percentiles = %+v", got)
	}
}

func potentTestPolicy() PotentIndustryScoringPolicy {
	return PotentIndustryScoringPolicy{
		MinHistorySessions: 253, TaxonomyMaxAge: 7 * 24 * time.Hour,
		MinGroupMembers: 5, MinGroupCoverage: .90,
		MinFreshTaxonomyCoverage: 1, MinPotencyScore: .60,
		SelectionPercentile: .50, MinAdvanceShare: .55,
		MinPersistenceSessions: 3, MaxCapEqualDivergence: .05,
		MaxMembersPerGroup: 3, MinMemberScore: .50,
		SectorBenchmarks: map[string]string{
			"TECHNOLOGY": "XLK", "INDUSTRIALS": "XLI", "HEALTH CARE": "XLV",
			"FINANCIALS": "XLF", "ENERGY": "XLE", "CONSUMER DISCRETIONARY": "XLY",
		},
	}
}

func potentTestTimes() (target, asOf, decisionAt time.Time) {
	location, _ := time.LoadLocation("America/New_York")
	target = time.Date(2026, time.July, 24, 0, 0, 0, 0, location)
	asOf = time.Date(2026, time.July, 24, 16, 0, 0, 0, location)
	decisionAt = asOf.Add(15 * time.Minute)
	return
}

func potentTestMembers(
	decisionAt time.Time, industry, sector string, caps []float64,
) []store.SwingUniverseMember {
	asOf := decisionAt.Add(-time.Hour)
	out := make([]store.SwingUniverseMember, 0, len(caps))
	for i, capValue := range caps {
		capCopy := capValue
		out = append(out, store.SwingUniverseMember{
			Ticker: string(rune('A'+i)) + "IE", Eligible: true,
			Industry: industry, Sector: sector, TaxonomyStatus: "known",
			TaxonomyAsOf: &asOf, MarketCapMM: &capCopy,
		})
	}
	return out
}

func constantPotentReturns(count int, daily float64) []float64 {
	out := make([]float64, count)
	for i := 1; i < count; i++ {
		out[i] = daily
	}
	return out
}

func potentTestBars(
	ticker string, target time.Time, start float64, returns []float64, finalRVOL float64,
) []data.Candle {
	out := make([]data.Candle, len(returns))
	price := start
	first := target.AddDate(0, 0, -(len(returns) - 1))
	for i, change := range returns {
		price *= math.Exp(change)
		volume := int64(1_000_000)
		if i == len(returns)-1 {
			volume = int64(float64(volume) * finalRVOL)
		}
		out[i] = data.Candle{
			Ticker: ticker, Timeframe: data.Timeframe1Day,
			Timestamp: first.AddDate(0, 0, i).Add(16 * time.Hour),
			Open:      price, High: price * 1.001, Low: price * .999,
			Close: price, Volume: volume,
		}
	}
	return out
}
