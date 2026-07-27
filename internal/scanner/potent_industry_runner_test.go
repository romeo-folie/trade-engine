package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type fakePotentIndustryProvider struct {
	result    data.DailyBarsBatchResult
	err       error
	calls     int
	symbols   []string
	from, to  time.Time
	batchSize int
}

func (f *fakePotentIndustryProvider) FetchDailyBars(
	_ context.Context, symbols []string, from, to time.Time, batchSize int,
) (data.DailyBarsBatchResult, error) {
	f.calls++
	f.symbols = append([]string(nil), symbols...)
	f.from, f.to, f.batchSize = from, to, batchSize
	return f.result, f.err
}

type fakePotentIndustryRepository struct {
	universe          *store.SwingUniverseVersion
	universeErr       error
	members           []store.SwingUniverseMember
	membersErr        error
	claim             *store.IndustryRotationRun
	claimed           bool
	claimErr          error
	universeAsOf      time.Time
	universeMaxAge    time.Duration
	claimInput        store.IndustryRotationRun
	claimLease        time.Duration
	failID            string
	failAttempt       int
	failCause         error
	completeID        string
	completeAttempt   int
	completeGroups    []store.IndustryRotationGroup
	completeMembers   []store.IndustryRotationMember
	benchmarkEvidence json.RawMessage
}

func (f *fakePotentIndustryRepository) GetUsableUniverse(
	_ context.Context, asOf time.Time, maxAge time.Duration,
) (*store.SwingUniverseVersion, error) {
	f.universeAsOf, f.universeMaxAge = asOf, maxAge
	return f.universe, f.universeErr
}

func (f *fakePotentIndustryRepository) ListEligibleMembers(
	_ context.Context, _ string,
) ([]store.SwingUniverseMember, error) {
	return append([]store.SwingUniverseMember(nil), f.members...), f.membersErr
}

func (f *fakePotentIndustryRepository) ClaimRun(
	_ context.Context, input store.IndustryRotationRun, lease time.Duration,
) (*store.IndustryRotationRun, bool, error) {
	f.claimInput, f.claimLease = input, lease
	return f.claim, f.claimed, f.claimErr
}

func (f *fakePotentIndustryRepository) FailRun(
	_ context.Context, id string, attempt int, _ time.Time, cause error,
) error {
	f.failID, f.failAttempt, f.failCause = id, attempt, cause
	return nil
}

func (f *fakePotentIndustryRepository) CompleteRun(
	_ context.Context, id string, attempt int, _ time.Time,
	groups []store.IndustryRotationGroup,
	members []store.IndustryRotationMember,
	evidence json.RawMessage,
) error {
	f.completeID, f.completeAttempt = id, attempt
	f.completeGroups = append([]store.IndustryRotationGroup(nil), groups...)
	f.completeMembers = append([]store.IndustryRotationMember(nil), members...)
	f.benchmarkEvidence = append(json.RawMessage(nil), evidence...)
	return nil
}

func TestPotentIndustryRunnerCompletesObservationAtExactBoundary(t *testing.T) {
	target, _, decisionAt := potentTestTimes()
	session := potentTestSession(target)
	cfg := potentRunnerConfig()
	cfg.PromoteCandidates = true // PR A remains observation-only even if preconfigured.
	members := potentTestMembers(
		decisionAt, "Software", "Technology",
		[]float64{2000, 1500, 1000, 800, 600},
	)
	members[1].Industry = " software "
	bars := potentRunnerHistory(target, members, "SPY", "XLK")
	provider := &fakePotentIndustryProvider{result: data.DailyBarsBatchResult{
		Bars: bars, Failures: map[string]error{},
	}}
	universeAsOf := decisionAt.Add(-time.Hour)
	repository := &fakePotentIndustryRepository{
		universe: &store.SwingUniverseVersion{
			ID: "universe-1", Provider: "fixture", AsOf: universeAsOf,
		},
		members: members,
		claim: &store.IndustryRotationRun{
			ID: "rotation-1", Status: "running", AttemptCount: 1,
		},
		claimed: true,
	}
	now := decisionAt.Add(time.Minute)
	runner := PotentIndustryRunner{
		Provider: provider, Cfg: cfg, Repository: repository,
		Now: func() time.Time { return now },
	}
	if err := runner.RunSession(context.Background(), session, decisionAt); err != nil {
		t.Fatal(err)
	}
	if !repository.universeAsOf.Equal(decisionAt) ||
		repository.universeMaxAge != 36*time.Hour {
		t.Fatalf("universe cutoff = %v/%v", repository.universeAsOf,
			repository.universeMaxAge)
	}
	if !repository.claimInput.AsOf.Equal(session.Close) ||
		!repository.claimInput.DecisionAt.Equal(decisionAt) ||
		repository.claimInput.UniverseVersionID != "universe-1" ||
		repository.claimInput.FeatureVersion != potentIndustryFeatureVersion ||
		repository.claimInput.MarketBenchmark != "SPY" ||
		repository.claimLease != time.Hour {
		t.Fatalf("run claim lost immutable boundary: %+v lease=%v",
			repository.claimInput, repository.claimLease)
	}
	if provider.calls != 1 || !provider.to.Equal(session.Close) ||
		!provider.from.Equal(session.Close.AddDate(0, 0, -420)) ||
		provider.batchSize != 200 {
		t.Fatalf("provider boundary = calls %d from %v to %v batch %d",
			provider.calls, provider.from, provider.to, provider.batchSize)
	}
	for _, symbol := range []string{"AIE", "BIE", "CIE", "DIE", "EIE", "SPY", "XLK"} {
		if !slices.Contains(provider.symbols, symbol) {
			t.Fatalf("provider symbols omitted %s: %v", symbol, provider.symbols)
		}
	}
	if len(provider.symbols) != 7 {
		t.Fatalf("runner fetched unused configured benchmarks: %v", provider.symbols)
	}
	if repository.completeID != "rotation-1" || repository.completeAttempt != 1 ||
		len(repository.completeGroups) != 1 || len(repository.completeMembers) != 5 {
		t.Fatalf("completion = %s/%d groups=%d members=%d",
			repository.completeID, repository.completeAttempt,
			len(repository.completeGroups), len(repository.completeMembers))
	}
	group := repository.completeGroups[0]
	if group.Outcome != "qualified" || group.GroupType != "industry" ||
		group.GroupKey != "Software" || group.Benchmark != "XLK" ||
		group.AdvanceDecline == nil || group.UpVolumeShare == nil ||
		group.PersistenceSessions != 5 || len(group.GroupEvidence) == 0 ||
		len(group.Components) == 0 {
		t.Fatalf("group evidence incomplete: %+v", group)
	}
	selected := 0
	for _, member := range repository.completeMembers {
		if member.GroupKey != "Software" {
			t.Fatalf("normalized member label lost canonical group FK: %+v", member)
		}
		if member.PromotionStatus != "not_requested" {
			t.Fatalf("observation PR requested promotion: %+v", member)
		}
		if member.Outcome == "qualified" {
			selected++
			if member.ClosePrice == nil || member.MemberScore == nil ||
				member.MemberRank == nil || member.StockVsIndustryRS == nil ||
				member.StockVsMarketRS == nil || len(member.SourceEvidence) == 0 {
				t.Fatalf("selected member evidence incomplete: %+v", member)
			}
		}
	}
	if selected == 0 {
		t.Fatal("fixture produced no selected members")
	}
	var evidence struct {
		MarketBenchmark string `json:"market_benchmark"`
		Benchmarks      map[string]struct {
			Status string `json:"status"`
			Hash   string `json:"bar_set_hash"`
		} `json:"benchmarks"`
	}
	if err := json.Unmarshal(repository.benchmarkEvidence, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.MarketBenchmark != "SPY" ||
		evidence.Benchmarks["SPY"].Status != "complete" ||
		evidence.Benchmarks["SPY"].Hash == "" ||
		evidence.Benchmarks["XLK"].Status != "complete" {
		t.Fatalf("benchmark lineage incomplete: %s", repository.benchmarkEvidence)
	}
}

func TestPotentIndustryRunnerMarketFailureFailsClaimedRun(t *testing.T) {
	target, _, decisionAt := potentTestTimes()
	session := potentTestSession(target)
	cfg := potentRunnerConfig()
	members := potentTestMembers(
		decisionAt, "Software", "Technology",
		[]float64{2000, 1500, 1000, 800, 600},
	)
	providerFailure := errors.New("SPY provider unavailable")
	provider := &fakePotentIndustryProvider{result: data.DailyBarsBatchResult{
		Bars: map[string][]data.Candle{}, Failures: map[string]error{"SPY": providerFailure},
	}}
	repository := &fakePotentIndustryRepository{
		universe: &store.SwingUniverseVersion{ID: "universe-1", Provider: "fixture"},
		members:  members,
		claim: &store.IndustryRotationRun{
			ID: "rotation-1", Status: "running", AttemptCount: 2,
		},
		claimed: true,
	}
	runner := PotentIndustryRunner{
		Provider: provider, Cfg: cfg, Repository: repository,
		Now: func() time.Time { return decisionAt },
	}
	err := runner.RunSession(context.Background(), session, decisionAt)
	if err == nil || !strings.Contains(err.Error(), "market benchmark SPY") {
		t.Fatalf("market failure = %v", err)
	}
	if repository.failID != "rotation-1" || repository.failAttempt != 2 ||
		!errors.Is(repository.failCause, providerFailure) ||
		repository.completeID != "" {
		t.Fatalf("claimed failure not recorded: id=%s attempt=%d cause=%v complete=%s",
			repository.failID, repository.failAttempt,
			repository.failCause, repository.completeID)
	}
}

func TestPotentIndustryStoreRowsRetainsUnknownTaxonomyDenominator(t *testing.T) {
	target, asOf, decisionAt := potentTestTimes()
	member := potentTestMembers(
		decisionAt, "", store.UnknownTaxonomy, []float64{1000},
	)[0]
	member.TaxonomyStatus = "unknown"
	market := potentTestBars(
		"SPY", target, 100, constantPotentReturns(253, .0001), 1,
	)
	evaluation := EvaluatePotentIndustries(
		[]store.SwingUniverseMember{member}, nil, market, nil,
		target, asOf, decisionAt, potentTestPolicy(),
	)
	universe := &store.SwingUniverseVersion{
		ID: "universe-1", Provider: "fixture", AsOf: asOf,
	}
	groups, members, err := potentIndustryStoreRows(
		"run-1", universe, []store.SwingUniverseMember{member}, asOf, evaluation,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].GroupKey != store.UnknownTaxonomy ||
		groups[0].EligibleMemberCount != 1 || groups[0].FailureStage != "taxonomy" {
		t.Fatalf("unknown taxonomy group persistence = %+v", groups)
	}
	if len(members) != 1 || members[0].GroupKey != store.UnknownTaxonomy ||
		members[0].Ticker != member.Ticker ||
		members[0].FailureStage != "taxonomy" {
		t.Fatalf("unknown taxonomy member persistence = %+v", members)
	}
}

func TestPotentIndustryRunnerAttributesPartialProviderFailures(t *testing.T) {
	target, _, decisionAt := potentTestTimes()
	session := potentTestSession(target)
	cfg := potentRunnerConfig()
	technology := potentTestMembers(
		decisionAt, "Software", "Technology",
		[]float64{2000, 1500, 1000, 800, 600},
	)
	energy := potentTestMembers(
		decisionAt, "Oil & Gas", "Energy",
		[]float64{2000, 1500, 1000, 800, 600},
	)
	for i := range energy {
		energy[i].Ticker = "X" + energy[i].Ticker
	}
	members := append(technology, energy...)
	bars := potentRunnerHistory(target, members, "SPY", "XLK")
	// Providers may return partial bars and a symbol failure together. The
	// failure must remove those bars from the scoring cohort.
	memberFailure := errors.New("member history timeout")
	benchmarkFailure := errors.New("sector ETF unavailable")
	provider := &fakePotentIndustryProvider{result: data.DailyBarsBatchResult{
		Bars: bars,
		Failures: map[string]error{
			technology[0].Ticker: memberFailure,
			"XLE":                benchmarkFailure,
		},
	}}
	repository := &fakePotentIndustryRepository{
		universe: &store.SwingUniverseVersion{ID: "universe-1", Provider: "fixture"},
		members:  members,
		claim: &store.IndustryRotationRun{
			ID: "rotation-1", Status: "running", AttemptCount: 1,
		},
		claimed: true,
	}
	runner := PotentIndustryRunner{
		Provider: provider, Cfg: cfg, Repository: repository,
		Now: func() time.Time { return decisionAt },
	}
	if err := runner.RunSession(context.Background(), session, decisionAt); err != nil {
		t.Fatal(err)
	}
	groupByIndustry := make(map[string]store.IndustryRotationGroup)
	for _, group := range repository.completeGroups {
		groupByIndustry[group.Industry] = group
	}
	if got := groupByIndustry["Oil & Gas"]; got.FailureStage != "provider_benchmark" ||
		got.Outcome != "failed" || !strings.Contains(got.FailureReason, benchmarkFailure.Error()) {
		t.Fatalf("benchmark failure attribution = %+v", got)
	}
	if got := groupByIndustry["Software"]; got.EvaluatedMemberCount != 4 ||
		got.MemberCoverage != .8 {
		t.Fatalf("failed member's partial bars influenced cohort: %+v", got)
	}
	var failedMember *store.IndustryRotationMember
	for i := range repository.completeMembers {
		if repository.completeMembers[i].Ticker == technology[0].Ticker {
			failedMember = &repository.completeMembers[i]
			break
		}
	}
	if failedMember == nil || failedMember.FailureStage != "provider_history" ||
		failedMember.Outcome != "failed" ||
		!strings.Contains(failedMember.FailureReason, memberFailure.Error()) {
		t.Fatalf("member failure attribution = %+v", failedMember)
	}
}

func TestPotentIndustryRunnerCompletedClaimSkipsProvider(t *testing.T) {
	_, _, decisionAt := potentTestTimes()
	session := potentTestSession(decisionAt)
	provider := &fakePotentIndustryProvider{}
	repository := &fakePotentIndustryRepository{
		universe: &store.SwingUniverseVersion{ID: "universe-1", Provider: "fixture"},
		claim: &store.IndustryRotationRun{
			ID: "rotation-1", Status: "completed", AttemptCount: 1,
		},
		claimed: false,
	}
	runner := PotentIndustryRunner{
		Provider: provider, Cfg: potentRunnerConfig(), Repository: repository,
	}
	if err := runner.RunSession(context.Background(), session, decisionAt); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 0 || repository.completeID != "" {
		t.Fatalf("canonical completed run repeated work: calls=%d complete=%s",
			provider.calls, repository.completeID)
	}
}

func TestPotentIndustryRunnerDefaultOffAndActiveLeaseDoNoProviderWork(t *testing.T) {
	var disabled PotentIndustryRunner
	if err := disabled.RunSession(
		context.Background(), engine.USMarketSession{}, time.Time{},
	); err != nil {
		t.Fatalf("default-off runner = %v", err)
	}

	_, _, decisionAt := potentTestTimes()
	session := potentTestSession(decisionAt)
	provider := &fakePotentIndustryProvider{}
	retryAt := decisionAt.Add(45 * time.Minute)
	repository := &fakePotentIndustryRepository{
		universe: &store.SwingUniverseVersion{ID: "universe-1", Provider: "fixture"},
		claim: &store.IndustryRotationRun{
			ID: "rotation-1", Status: "running", AttemptCount: 1,
			LeaseUntil: retryAt,
		},
		claimed: false,
	}
	runner := PotentIndustryRunner{
		Provider: provider, Cfg: potentRunnerConfig(), Repository: repository,
	}
	err := runner.RunSession(context.Background(), session, decisionAt)
	var active *potentIndustryRunLeaseActiveError
	if !errors.As(err, &active) || !active.retryAt.Equal(retryAt) {
		t.Fatalf("active claim = %v, want retry at %v", err, retryAt)
	}
	if provider.calls != 0 {
		t.Fatalf("active lease fetched provider history %d times", provider.calls)
	}
}

func TestPotentIndustryPolicyMapsEveryScoringControl(t *testing.T) {
	cfg := potentRunnerConfig()
	got := potentIndustryPolicy(cfg)
	if got.MinHistorySessions != cfg.MinHistorySessions ||
		got.TaxonomyMaxAge != time.Duration(cfg.TaxonomyMaxAgeHours)*time.Hour ||
		got.MinGroupMembers != cfg.MinGroupMembers ||
		got.MinGroupCoverage != cfg.MinGroupCoverage ||
		got.MinFreshTaxonomyCoverage != cfg.MinFreshTaxonomyCoverage ||
		got.MinPotencyScore != cfg.MinPotencyScore ||
		got.MinMemberScore != cfg.MinMemberScore ||
		got.SelectionPercentile != cfg.SelectionPercentile ||
		got.MinAdvanceShare != cfg.MinAdvanceShare ||
		got.MinMedianReturn5 != cfg.MinMedianReturn5D ||
		got.MinPersistenceSessions != cfg.MinPersistenceSessions ||
		got.MaxCapEqualDivergence != cfg.MaxCapEqualDivergence ||
		got.MaxMembersPerGroup != cfg.MaxMembersPerGroup ||
		got.SectorBenchmarks["Technology"] != "XLK" {
		t.Fatalf("config-to-policy mapping incomplete: %+v", got)
	}
}

func potentRunnerConfig() config.PotentIndustryConfig {
	cfg := config.Defaults().Scanner.PotentIndustry
	cfg.Enabled = true
	cfg.MinPotencyScore = .55
	cfg.SelectionPercentile = .50
	cfg.MinMemberScore = .40
	cfg.MinAdvanceShare = .55
	return cfg
}

func potentTestSession(target time.Time) engine.USMarketSession {
	return engine.MostRecentCompletedMarketSession(
		time.Date(
			target.Year(), target.Month(), target.Day(), 17, 0, 0, 0,
			target.Location(),
		),
		15*time.Minute,
	)
}

func potentRunnerHistory(
	target time.Time,
	members []store.SwingUniverseMember,
	marketSymbol, sectorSymbol string,
) map[string][]data.Candle {
	bars := map[string][]data.Candle{
		marketSymbol: potentTestBars(
			marketSymbol, target, 100, constantPotentReturns(253, .0001), 1,
		),
		sectorSymbol: potentTestBars(
			sectorSymbol, target, 100, constantPotentReturns(253, .0002), 1,
		),
	}
	for i, member := range members {
		bars[member.Ticker] = potentTestBars(
			member.Ticker, target, 30+float64(i),
			constantPotentReturns(253, .0015+.00015*float64(i)), 1.5,
		)
	}
	return bars
}
