package scanner

import (
	"math"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestEvaluateWeeklyLeadersNoLookaheadAndCompleteDenominator(t *testing.T) {
	asOf := time.Date(2026, time.July, 24, 20, 0, 0, 0, time.UTC)
	benchmark := leaderTestBars(asOf, 260, 0.0004, 1_000_000)
	aaa := leaderTestBars(asOf, 260, 0.0020, 500_000)
	bbb := leaderTestBars(asOf, 252, 0.0030, 500_000)
	future := aaa[len(aaa)-1]
	future.Timestamp = asOf.Add(24 * time.Hour)
	future.Open, future.High, future.Low, future.Close = 999, 1001, 998, 1000
	withFuture := append(append([]data.Candle(nil), aaa...), future)
	capMM := 5_000.0
	taxonomyAt := asOf.Add(-24 * time.Hour)
	members := []store.SwingUniverseMember{
		{Ticker: "AAA", Eligible: true, MarketCapMM: &capMM, Sector: "Tech",
			Industry: "Software", TaxonomyStatus: "known", TaxonomyAsOf: &taxonomyAt},
		{Ticker: "BBB", Eligible: true, MarketCapMM: &capMM, Sector: "Tech",
			Industry: "Software", TaxonomyStatus: "known", TaxonomyAsOf: &taxonomyAt},
	}
	cfg := leaderTestConfig()
	first := EvaluateWeeklyLeaders(members, map[string][]data.Candle{
		"AAA": aaa, "BBB": bbb,
	}, benchmark, asOf, asOf, asOf.Add(15*time.Minute), cfg)
	second := EvaluateWeeklyLeaders(members, map[string][]data.Candle{
		"AAA": withFuture, "BBB": bbb,
	}, append(benchmark, future), asOf, asOf, asOf.Add(15*time.Minute), cfg)
	if len(first) != len(members) || len(second) != len(members) {
		t.Fatalf("evaluations = %d/%d, want full denominator %d",
			len(first), len(second), len(members))
	}
	if first[0].Ticker != "AAA" || first[0].Features == nil {
		t.Fatalf("AAA evaluation missing: %+v", first[0])
	}
	if second[0].Features == nil ||
		first[0].Features.Return126 != second[0].Features.Return126 ||
		*first[0].CompositeScore != *second[0].CompositeScore ||
		first[0].BarSetHash != second[0].BarSetHash {
		t.Fatal("future bar changed the point-in-time evaluation")
	}
	if first[1].Outcome != "failed" || first[1].FailureStage != "insufficient_history" {
		t.Fatalf("short-history outcome = %+v", first[1])
	}
}

func TestEvaluateWeeklyLeadersRequiresTargetSessionBar(t *testing.T) {
	target := time.Date(2026, time.July, 24, 20, 0, 0, 0, time.UTC)
	stale := leaderTestBars(target.AddDate(0, 0, -1), 260, 0.001, 500_000)
	current := leaderTestBars(target, 260, 0.001, 500_000)
	future := current[len(current)-1]
	future.Timestamp = target.AddDate(0, 0, 1)
	member := store.SwingUniverseMember{
		Ticker: "AAA", Eligible: true, Sector: "Tech", Industry: "Software",
	}
	cfg := leaderTestConfig()

	staleTicker := EvaluateWeeklyLeaders(
		[]store.SwingUniverseMember{member},
		map[string][]data.Candle{"AAA": append(stale, future)},
		current, target, target, target.Add(15*time.Minute), cfg,
	)
	if staleTicker[0].Outcome != "failed" ||
		staleTicker[0].FailureStage != "bar_validation" ||
		staleTicker[0].Features != nil || staleTicker[0].Rank != nil {
		t.Fatalf("stale ticker was evaluated as current: %+v", staleTicker[0])
	}

	staleBenchmark := EvaluateWeeklyLeaders(
		[]store.SwingUniverseMember{member},
		map[string][]data.Candle{"AAA": current},
		append(stale, future), target, target, target.Add(15*time.Minute), cfg,
	)
	if staleBenchmark[0].Outcome != "failed" ||
		staleBenchmark[0].FailureStage != "benchmark_history" {
		t.Fatalf("stale benchmark was accepted: %+v", staleBenchmark[0])
	}
}

func TestWeeklyLeaderCompatibilityHashIgnoresOperationalSettings(t *testing.T) {
	cfg := config.Defaults().Scanner.WeeklyLeader
	_, original, err := weeklyLeaderConfigEvidence(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Enabled = !cfg.Enabled
	cfg.PromoteCandidates = !cfg.PromoteCandidates
	cfg.MaxPromotionsPerRun++
	cfg.AfterCloseDelayMinutes++
	cfg.BatchSize++
	cfg.RunLeaseMinutes++
	_, operational, err := weeklyLeaderConfigEvidence(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if original != operational {
		t.Fatal("operational deployment settings broke scoring-history compatibility")
	}
	cfg.MinCompositeScore += 0.01
	_, scoring, err := weeklyLeaderConfigEvidence(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if original == scoring {
		t.Fatal("scoring change did not change compatibility hash")
	}
}

func TestProviderFailureSuppressesCrossSectionalQualification(t *testing.T) {
	score := 0.9
	evaluations := []WeeklyLeaderEvaluation{
		{Ticker: "AAA", Outcome: "qualified", Qualified: true, CompositeScore: &score},
		{Ticker: "BBB", Outcome: "failed", FailureStage: "provider_history"},
	}
	suppressWeeklyLeaderQualificationsOnProviderFailure(evaluations, 1, 2)
	if evaluations[0].Qualified || evaluations[0].Outcome != "not_qualified" ||
		evaluations[0].FailureStage != "cross_section_incomplete" {
		t.Fatalf("partial provider survivors remained promotable: %+v", evaluations[0])
	}
	if evaluations[1].Outcome != "failed" ||
		evaluations[1].FailureStage != "provider_history" {
		t.Fatalf("original provider failure was overwritten: %+v", evaluations[1])
	}
}

func TestWeeklyLeaderPromotionSelectsFirstNQualifiedRows(t *testing.T) {
	rank1, rank3, rank9, rank21 := 1, 3, 9, 21
	qualifications, err := weeklyLeaderQualifications(
		"run", "universe", "test", time.Now(),
		[]WeeklyLeaderEvaluation{
			{Ticker: "ZZZ", Outcome: "qualified", Qualified: true, Rank: &rank21},
			{Ticker: "AAA", Outcome: "not_qualified", Rank: &rank1},
			{Ticker: "CCC", Outcome: "qualified", Qualified: true, Rank: &rank9},
			{Ticker: "BBB", Outcome: "qualified", Qualified: true, Rank: &rank3},
		},
		config.WeeklyLeaderConfig{
			PromoteCandidates: true, MaxPromotionsPerRun: 2,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if qualifications[0].PromotionStatus != "not_requested" ||
		qualifications[1].PromotionStatus != "not_requested" ||
		qualifications[2].PromotionStatus != "pending" ||
		qualifications[3].PromotionStatus != "pending" {
		t.Fatalf("promotion statuses = %s/%s/%s/%s",
			qualifications[0].PromotionStatus,
			qualifications[1].PromotionStatus,
			qualifications[2].PromotionStatus,
			qualifications[3].PromotionStatus)
	}
}

func TestWeeklyLeaderFeatureDefinitions(t *testing.T) {
	asOf := time.Date(2026, time.July, 24, 20, 0, 0, 0, time.UTC)
	smooth := leaderTestBars(asOf, 260, 0.0015, 1_000_000)
	features, err := extractWeeklyLeaderFeatures(smooth)
	if err != nil {
		t.Fatal(err)
	}
	if features.High52WeekProximity <= 0 || features.High52WeekProximity > 1 {
		t.Fatalf("52-week proximity = %v", features.High52WeekProximity)
	}
	if features.ClosePrice != smooth[len(smooth)-1].Close {
		t.Fatalf("feature close = %v, want %v",
			features.ClosePrice, smooth[len(smooth)-1].Close)
	}
	if features.Return126 <= features.Return20 {
		t.Fatalf("independent horizons collapsed: 20=%v 126=%v",
			features.Return20, features.Return126)
	}
	if features.RegressionR2 < 0.99 || features.Efficiency60 < 0.99 {
		t.Fatalf("smooth trend quality = r2 %.4f efficiency %.4f",
			features.RegressionR2, features.Efficiency60)
	}
	if features.MedianDollarVolume20 <= 0 ||
		features.DollarVolumeStability20 <= 0 {
		t.Fatalf("liquidity features invalid: %+v", features)
	}

	gappy := append([]data.Candle(nil), smooth[len(smooth)-61:]...)
	index := 30
	gappy[index].Open = gappy[index-1].Close * 1.25
	gappy[index].High = math.Max(gappy[index].High, gappy[index].Open*1.001)
	dominance, largest, progress := gapDomination(gappy)
	if dominance <= 0 || largest <= 0 || progress <= 0 {
		t.Fatalf("gap evidence not detected: dominance=%v largest=%v progress=%v",
			dominance, largest, progress)
	}
	if penalty := 1 - 0.35*dominance; penalty >= 1 {
		t.Fatalf("gap penalty = %v, want below 1", penalty)
	}
}

func TestWeeklyLeaderRankingIsDeterministicAndKeepsHighAndRSSeparate(t *testing.T) {
	asOf := time.Date(2026, time.July, 24, 20, 0, 0, 0, time.UTC)
	benchmark := leaderTestBars(asOf, 260, 0.0003, 2_000_000)
	members := make([]store.SwingUniverseMember, 0, 10)
	bars := make(map[string][]data.Candle)
	taxonomyAt := asOf.Add(-24 * time.Hour)
	for i := 0; i < 10; i++ {
		ticker := string(rune('A'+i)) + "AA"
		capMM := 500.0 + float64(i)*2_000
		members = append(members, store.SwingUniverseMember{
			Ticker: ticker, Eligible: true, MarketCapMM: &capMM,
			Sector: "Tech", Industry: "Software", TaxonomyStatus: "known",
			TaxonomyAsOf: &taxonomyAt,
		})
		bars[ticker] = leaderTestBars(asOf, 260, 0.0005+float64(i)*0.0002, 500_000+int64(i)*20_000)
	}
	cfg := leaderTestConfig()
	cfg.IndustryMinPeers = 5
	cfg.SelectionPercentile = 0.80
	cfg.MinCompositeScore = 0.10
	first := EvaluateWeeklyLeaders(
		members, bars, benchmark, asOf, asOf, asOf.Add(15*time.Minute), cfg,
	)
	second := EvaluateWeeklyLeaders(
		members, bars, benchmark, asOf, asOf, asOf.Add(15*time.Minute), cfg,
	)
	for i := range first {
		if first[i].Ticker != second[i].Ticker ||
			*first[i].Rank != *second[i].Rank ||
			*first[i].CompositeScore != *second[i].CompositeScore {
			t.Fatalf("non-deterministic evaluation at %d: %+v / %+v", i, first[i], second[i])
		}
		if first[i].Features == nil {
			t.Fatalf("%s features missing", first[i].Ticker)
		}
		if first[i].Features.High52WeekProximity == first[i].Features.MarketRS60 {
			t.Fatalf("%s high proximity and market RS were conflated", first[i].Ticker)
		}
		if first[i].Features.IndustryRS60 == nil {
			t.Fatalf("%s industry RS missing despite sufficient peers", first[i].Ticker)
		}
	}
	qualified := 0
	for _, evaluation := range first {
		if evaluation.Qualified {
			qualified++
		}
	}
	if qualified == 0 || qualified >= len(first) {
		t.Fatalf("qualified = %d, want a selective non-empty leader set", qualified)
	}

	futureTaxonomyMembers := append([]store.SwingUniverseMember(nil), members...)
	futureTaxonomy := asOf.Add(time.Hour)
	futureTaxonomyMembers[0].TaxonomyAsOf = &futureTaxonomy
	withoutFutureTaxonomy := EvaluateWeeklyLeaders(
		futureTaxonomyMembers, bars, benchmark,
		asOf, asOf, asOf.Add(15*time.Minute), cfg,
	)
	if withoutFutureTaxonomy[0].Features.IndustryRS60 != nil {
		t.Fatal("taxonomy first effective after the decision cutoff leaked into industry RS")
	}
}

func TestWeeklyLeaderScoreRespondsToBenchmarkReturn(t *testing.T) {
	asOf := time.Date(2026, time.July, 24, 20, 0, 0, 0, time.UTC)
	members := []store.SwingUniverseMember{
		{Ticker: "AAA", Eligible: true},
		{Ticker: "BBB", Eligible: true},
	}
	bars := map[string][]data.Candle{
		"AAA": leaderTestBars(asOf, 260, 0.0012, 1_000_000),
		"BBB": leaderTestBars(asOf, 260, 0.0008, 1_000_000),
	}
	cfg := leaderTestConfig()
	cfg.SelectionPercentile = 0.49
	weakBenchmark := EvaluateWeeklyLeaders(
		members, bars, leaderTestBars(asOf, 260, 0.0001, 2_000_000),
		asOf, asOf, asOf.Add(15*time.Minute), cfg,
	)
	strongBenchmark := EvaluateWeeklyLeaders(
		members, bars, leaderTestBars(asOf, 260, 0.0015, 2_000_000),
		asOf, asOf, asOf.Add(15*time.Minute), cfg,
	)

	weakAAA := weeklyLeaderEvaluationByTicker(t, weakBenchmark, "AAA")
	strongAAA := weeklyLeaderEvaluationByTicker(t, strongBenchmark, "AAA")
	if weakAAA.Components.MarketRS <= strongAAA.Components.MarketRS {
		t.Fatalf("market RS did not respond to benchmark: weak %.6f, strong %.6f",
			weakAAA.Components.MarketRS, strongAAA.Components.MarketRS)
	}
	if *weakAAA.CompositeScore <= *strongAAA.CompositeScore {
		t.Fatalf("composite score did not respond to benchmark: weak %.6f, strong %.6f",
			*weakAAA.CompositeScore, *strongAAA.CompositeScore)
	}
	if weakAAA.Features.MarketRS60 <= strongAAA.Features.MarketRS60 {
		t.Fatalf("raw market RS did not respond to benchmark: weak %.6f, strong %.6f",
			weakAAA.Features.MarketRS60, strongAAA.Features.MarketRS60)
	}

	cfg.MinCompositeScore = (*weakAAA.CompositeScore + *strongAAA.CompositeScore) / 2
	weakGate := weeklyLeaderEvaluationByTicker(t, EvaluateWeeklyLeaders(
		members, bars, leaderTestBars(asOf, 260, 0.0001, 2_000_000),
		asOf, asOf, asOf.Add(15*time.Minute), cfg,
	), "AAA")
	strongGate := weeklyLeaderEvaluationByTicker(t, EvaluateWeeklyLeaders(
		members, bars, leaderTestBars(asOf, 260, 0.0015, 2_000_000),
		asOf, asOf, asOf.Add(15*time.Minute), cfg,
	), "AAA")
	if !weakGate.Qualified || strongGate.Qualified {
		t.Fatalf("benchmark-relative score did not change qualification: weak=%v strong=%v",
			weakGate.Qualified, strongGate.Qualified)
	}
}

func weeklyLeaderEvaluationByTicker(
	t *testing.T, evaluations []WeeklyLeaderEvaluation, ticker string,
) WeeklyLeaderEvaluation {
	t.Helper()
	for _, evaluation := range evaluations {
		if evaluation.Ticker == ticker {
			if evaluation.Features == nil || evaluation.Components == nil ||
				evaluation.CompositeScore == nil {
				t.Fatalf("%s evaluation is incomplete: %+v", ticker, evaluation)
			}
			return evaluation
		}
	}
	t.Fatalf("%s evaluation not found", ticker)
	return WeeklyLeaderEvaluation{}
}

func TestPercentilesUseMidrankForTies(t *testing.T) {
	rows := []weeklyLeaderRaw{
		{member: store.SwingUniverseMember{Ticker: "BBB"}, capBand: "large", volBand: "low"},
		{member: store.SwingUniverseMember{Ticker: "AAA"}, capBand: "large", volBand: "low"},
	}
	got := percentileForRows(rows, func(weeklyLeaderRaw) (float64, bool) {
		return 42, true
	})
	if got["AAA"] != 0.5 || got["BBB"] != 0.5 {
		t.Fatalf("tie percentiles = %v, want both 0.5", got)
	}
}

func TestFragileOneGapSmallCapDoesNotOutrankSmoothLeadership(t *testing.T) {
	asOf := time.Date(2026, time.July, 24, 20, 0, 0, 0, time.UTC)
	benchmark := leaderTestBars(asOf, 260, 0.0003, 2_000_000)
	bars := make(map[string][]data.Candle)
	members := make([]store.SwingUniverseMember, 0, 10)
	taxonomyAt := asOf.Add(-24 * time.Hour)
	for i := 0; i < 9; i++ {
		ticker := string(rune('A'+i)) + "SM"
		capMM := 12_000.0 + float64(i)*1_000
		members = append(members, store.SwingUniverseMember{
			Ticker: ticker, Eligible: true, MarketCapMM: &capMM,
			Sector: "Tech", Industry: "Software", TaxonomyStatus: "known",
			TaxonomyAsOf: &taxonomyAt,
		})
		bars[ticker] = leaderTestBars(
			asOf, 260, 0.0008+float64(i)*0.00008, 1_000_000,
		)
	}
	smallCap := 300.0
	members = append(members, store.SwingUniverseMember{
		Ticker: "ZGP", Eligible: true, MarketCapMM: &smallCap,
		Sector: "Tech", Industry: "Software", TaxonomyStatus: "known",
		TaxonomyAsOf: &taxonomyAt,
	})
	gappy := leaderTestBars(asOf, 260, 0.0005, 100_000)
	last := len(gappy) - 1
	gappy[last].Open = gappy[last-1].Close * 1.60
	gappy[last].Close = gappy[last].Open * 1.002
	gappy[last].High = gappy[last].Close * 1.002
	gappy[last].Low = gappy[last].Open * 0.998
	bars["ZGP"] = gappy

	cfg := leaderTestConfig()
	cfg.IndustryMinPeers = 5
	cfg.SelectionPercentile = 0.80
	cfg.MinCompositeScore = 0.10
	evaluations := EvaluateWeeklyLeaders(
		members, bars, benchmark, asOf, asOf, asOf.Add(15*time.Minute), cfg,
	)
	var gapScore float64
	bestSmooth := 0.0
	for _, evaluation := range evaluations {
		if evaluation.Ticker == "ZGP" {
			gapScore = *evaluation.CompositeScore
			if evaluation.Features.GapDomination60 < 0.50 {
				t.Fatalf("fragile gap domination = %.3f, want >= 0.50",
					evaluation.Features.GapDomination60)
			}
			continue
		}
		bestSmooth = math.Max(bestSmooth, *evaluation.CompositeScore)
	}
	if gapScore >= bestSmooth {
		t.Fatalf("one-gap small cap score %.4f outranked smooth leader %.4f",
			gapScore, bestSmooth)
	}
}

func leaderTestBars(
	asOf time.Time, count int, logSlope float64, volume int64,
) []data.Candle {
	out := make([]data.Candle, 0, count)
	start := asOf.AddDate(0, 0, -(count - 1))
	for i := 0; i < count; i++ {
		closePrice := 50 * math.Exp(logSlope*float64(i))
		open := closePrice * 0.999
		out = append(out, data.Candle{
			Ticker: "TEST", Timeframe: data.Timeframe1Day,
			Timestamp: start.AddDate(0, 0, i),
			Open:      open, High: closePrice * 1.002, Low: open * 0.998,
			Close: closePrice, Volume: volume + int64(i%5)*100,
		})
	}
	return out
}

func leaderTestConfig() config.WeeklyLeaderConfig {
	return config.WeeklyLeaderConfig{
		MinHistorySessions: 253, IndustryMinPeers: 2,
		SelectionPercentile: 0.50, MinCompositeScore: 0.01,
		MinFeatureCoverage: 0.90,
	}
}
