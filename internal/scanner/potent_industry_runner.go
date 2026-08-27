package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type potentIndustryRepository interface {
	GetUsableUniverse(context.Context, time.Time, time.Duration) (*store.SwingUniverseVersion, error)
	ListEligibleMembers(context.Context, string) ([]store.SwingUniverseMember, error)
	ClaimRun(context.Context, store.IndustryRotationRun, time.Duration) (*store.IndustryRotationRun, bool, error)
	FailRun(context.Context, string, int, time.Time, error) error
	CompleteRun(
		context.Context, string, int, time.Time,
		[]store.IndustryRotationGroup, []store.IndustryRotationMember, json.RawMessage,
	) error
}

type potentIndustryStoreRepository struct{}

func (potentIndustryStoreRepository) GetUsableUniverse(
	ctx context.Context, asOf time.Time, maxAge time.Duration,
) (*store.SwingUniverseVersion, error) {
	return store.GetUsableSwingUniverseVersion(ctx, asOf, maxAge)
}

func (potentIndustryStoreRepository) ListEligibleMembers(
	ctx context.Context, versionID string,
) ([]store.SwingUniverseMember, error) {
	return store.ListEligibleSwingUniverseMembers(ctx, versionID)
}

func (potentIndustryStoreRepository) ClaimRun(
	ctx context.Context, run store.IndustryRotationRun, lease time.Duration,
) (*store.IndustryRotationRun, bool, error) {
	return store.ClaimIndustryRotationRun(ctx, run, lease)
}

func (potentIndustryStoreRepository) FailRun(
	ctx context.Context, id string, attempt int, at time.Time, cause error,
) error {
	return store.FailIndustryRotationRun(ctx, id, attempt, at, cause)
}

func (potentIndustryStoreRepository) CompleteRun(
	ctx context.Context, id string, attempt int, at time.Time,
	groups []store.IndustryRotationGroup,
	members []store.IndustryRotationMember,
	benchmarkEvidence json.RawMessage,
) error {
	return store.CompleteIndustryRotationRun(
		ctx, id, attempt, at, groups, members, benchmarkEvidence,
	)
}

// PotentIndustryRunner observes coordinated industry rotation across the
// immutable point-in-time universe. It never authorizes a trade; when explicitly
// configured, its highest-ranked members become pending scanner candidates.
type PotentIndustryRunner struct {
	Provider      data.BulkDailyBarsProvider
	Cfg           config.PotentIndustryConfig
	Now           func() time.Time
	Repository    potentIndustryRepository
	universe      *store.SwingUniverseVersion
	exactUniverse bool
}

func (r *PotentIndustryRunner) Name() string { return "potent_industry" }

func (r *PotentIndustryRunner) RunSessionWithUniverse(
	ctx context.Context, session engine.USMarketSession,
	universe *store.SwingUniverseVersion, decisionAt time.Time,
) error {
	copy := *r
	copy.universe = universe
	copy.exactUniverse = true
	return copy.RunSession(ctx, session, decisionAt)
}

func (r *PotentIndustryRunner) RunSession(
	ctx context.Context, session engine.USMarketSession, decisionAt time.Time,
) (runErr error) {
	if !r.Cfg.Enabled {
		return nil
	}
	if r.Provider == nil {
		return fmt.Errorf("potent industry daily-bars provider is required")
	}
	if decisionAt.Before(session.Close) {
		return fmt.Errorf("potent industry decision time precedes session close")
	}
	repository := r.repository()
	startedAt := r.now().UTC()
	maxAge := time.Duration(r.Cfg.UniverseMaxAgeHours) * time.Hour
	if maxAge <= 0 {
		maxAge = 36 * time.Hour
	}
	universe := r.universe
	providedUniverse := r.exactUniverse
	var err error
	if providedUniverse && universe == nil {
		return fmt.Errorf("potent industry requires an approved universe from the session pipeline")
	}
	if !providedUniverse {
		universe, err = repository.GetUsableUniverse(ctx, decisionAt, maxAge)
		if err != nil {
			return fmt.Errorf("load potent industry universe: %w", err)
		}
	}
	if universe == nil {
		return fmt.Errorf("no completed swing universe available at %s within %s",
			decisionAt.Format(time.RFC3339), maxAge)
	}
	if providedUniverse && (universe.Status != "completed" || universe.QualityStatus != "approved" ||
		universe.CompletedAt == nil || universe.CompletedAt.After(decisionAt)) {
		return fmt.Errorf("potent industry requires an approved universe available by decision time")
	}
	if providedUniverse && (universe.SessionDate == nil ||
		universe.SessionDate.Format("2006-01-02") != session.Date.Format("2006-01-02")) {
		return fmt.Errorf("potent industry universe does not belong to target session")
	}
	members, err := repository.ListEligibleMembers(ctx, universe.ID)
	if err != nil {
		return fmt.Errorf("list potent industry universe members: %w", err)
	}
	configSnapshot, configHash, err := config.PotentIndustryConfigEvidence(
		r.Cfg, potentIndustryFeatureVersion,
	)
	if err != nil {
		return err
	}
	marketBenchmark := strings.ToUpper(strings.TrimSpace(r.Cfg.MarketBenchmark))
	if marketBenchmark == "" {
		marketBenchmark = "SPY"
	}
	lease := time.Duration(r.Cfg.RunLeaseMinutes) * time.Minute
	if lease <= 0 {
		lease = time.Hour
	}
	claimed, claimedCurrent, err := repository.ClaimRun(
		ctx,
		store.IndustryRotationRun{
			SessionDate: session.Date, AsOf: session.Close, DecisionAt: decisionAt,
			UniverseVersionID: universe.ID, StartedAt: startedAt,
			ConfigHash: configHash, ConfigSnapshot: configSnapshot,
			FeatureVersion:  potentIndustryFeatureVersion,
			MarketBenchmark: marketBenchmark, HistoryProvider: universe.Provider,
		},
		lease,
	)
	if err != nil {
		return err
	}
	if !claimedCurrent {
		if claimed != nil && claimed.Status == "completed" {
			slog.Info("potent industry: session already completed",
				"session_date", session.Date.Format("2006-01-02"),
				"universe_version_id", universe.ID)
			return nil
		}
		if claimed != nil && claimed.Status == "running" {
			return &potentIndustryRunLeaseActiveError{
				sessionDate: session.Date, retryAt: claimed.LeaseUntil,
			}
		}
		status := "missing"
		if claimed != nil {
			status = claimed.Status
		}
		return fmt.Errorf("potent industry run claim declined with unexpected status %s", status)
	}
	defer func() {
		if runErr == nil {
			return
		}
		failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := repository.FailRun(
			failCtx, claimed.ID, claimed.AttemptCount, r.now().UTC(), runErr,
		); err != nil {
			slog.Error("potent industry: failed to persist run failure",
				"run_id", claimed.ID, "err", err)
		}
	}()

	benchmarkSymbols := potentIndustryResolvedBenchmarkSymbols(
		members, marketBenchmark, r.Cfg, decisionAt,
	)
	symbols := potentIndustryHistorySymbols(members, benchmarkSymbols)
	historyDays := r.Cfg.HistoryCalendarDays
	if historyDays <= 0 {
		historyDays = 420
	}
	batchSize := r.Cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 200
	}
	history, err := r.Provider.FetchDailyBars(
		ctx, symbols, session.Close.AddDate(0, 0, -historyDays),
		session.Close, batchSize,
	)
	if err != nil {
		return fmt.Errorf("fetch potent industry daily bars: %w", err)
	}
	marketBars := potentHistoryBars(history.Bars, marketBenchmark)
	if failure, ok := potentHistoryFailure(history.Failures, marketBenchmark); ok {
		return fmt.Errorf("potent industry market benchmark %s history: %w",
			marketBenchmark, failure)
	}
	minHistory := r.Cfg.MinHistorySessions
	if minHistory <= 0 {
		minHistory = 253
	}
	completedMarket, err := potentAlignedBenchmark(
		marketBars, session.Date, session.Close, minHistory,
	)
	if err != nil {
		return fmt.Errorf("potent industry market benchmark %s: %w",
			marketBenchmark, err)
	}

	scoringBars := potentIndustryHistoryWithoutFailures(history.Bars, history.Failures)
	benchmarkHistories := make(map[string][]data.Candle)
	for _, symbol := range benchmarkSymbols {
		benchmarkHistories[symbol] = potentHistoryBars(scoringBars, symbol)
	}
	evaluation := EvaluatePotentIndustries(
		members, scoringBars, marketBars, benchmarkHistories,
		session.Date, session.Close, decisionAt, potentIndustryPolicy(r.Cfg),
	)
	potentIndustryApplyProviderFailures(&evaluation, history.Failures)
	groups, memberRows, err := potentIndustryStoreRows(
		claimed.ID, universe, members, session.Close, evaluation,
	)
	if err != nil {
		return err
	}
	markPotentIndustryPromotions(groups, memberRows, r.Cfg)
	benchmarkEvidence, err := potentIndustryBenchmarkEvidence(
		completedMarket, marketBenchmark, benchmarkHistories, history.Failures,
		session.Date, session.Close, minHistory,
	)
	if err != nil {
		return err
	}
	if err := repository.CompleteRun(
		ctx, claimed.ID, claimed.AttemptCount, r.now().UTC(),
		groups, memberRows, benchmarkEvidence,
	); err != nil {
		return err
	}
	potentGroups, selectedMembers := 0, 0
	for _, group := range groups {
		if group.Outcome == "qualified" {
			potentGroups++
		}
	}
	for _, member := range memberRows {
		if member.Outcome == "qualified" {
			selectedMembers++
		}
	}
	slog.Info("potent industry: session completed",
		"run_id", claimed.ID,
		"session_date", session.Date.Format("2006-01-02"),
		"groups", len(groups), "potent_groups", potentGroups,
		"members", len(memberRows), "selected_members", selectedMembers,
	)
	return nil
}

func markPotentIndustryPromotions(
	groups []store.IndustryRotationGroup,
	members []store.IndustryRotationMember,
	cfg config.PotentIndustryConfig,
) {
	if !cfg.PromoteCandidates {
		return
	}
	limit := cfg.MaxPromotionsPerRun
	if limit <= 0 {
		limit = maxPromotionAttemptsPerSource
	}
	if limit > maxPromotionAttemptsPerSource {
		limit = maxPromotionAttemptsPerSource
	}
	type rankedMember struct {
		index       int
		groupRank   int
		memberRank  int
		memberScore float64
		ticker      string
	}
	groupRanks := make(map[string]int, len(groups))
	for _, group := range groups {
		if group.Outcome == "qualified" && group.Rank != nil {
			groupRanks[potentPromotionGroupKey(group.GroupType, group.GroupKey)] = *group.Rank
		}
	}
	ranked := make([]rankedMember, 0, len(members))
	for i, member := range members {
		groupRank, groupQualified := groupRanks[potentPromotionGroupKey(
			member.GroupType, member.GroupKey,
		)]
		if !groupQualified || member.Outcome != "qualified" ||
			member.MemberRank == nil || member.MemberScore == nil {
			continue
		}
		ranked = append(ranked, rankedMember{
			index: i, groupRank: groupRank, memberRank: *member.MemberRank,
			memberScore: *member.MemberScore, ticker: member.Ticker,
		})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].groupRank != ranked[j].groupRank {
			return ranked[i].groupRank < ranked[j].groupRank
		}
		if ranked[i].memberRank != ranked[j].memberRank {
			return ranked[i].memberRank < ranked[j].memberRank
		}
		if ranked[i].memberScore != ranked[j].memberScore {
			return ranked[i].memberScore > ranked[j].memberScore
		}
		return ranked[i].ticker < ranked[j].ticker
	})
	selectedTickers := make(map[string]struct{}, limit)
	for _, candidate := range ranked {
		if len(selectedTickers) >= limit {
			break
		}
		if _, exists := selectedTickers[candidate.ticker]; exists {
			continue
		}
		members[candidate.index].PromotionStatus = "pending"
		selectedTickers[candidate.ticker] = struct{}{}
	}
}

func potentPromotionGroupKey(groupType, groupKey string) string {
	return strings.ToLower(strings.TrimSpace(groupType)) + "\x00" +
		normalizePotentLabel(groupKey)
}

func potentIndustryPolicy(cfg config.PotentIndustryConfig) PotentIndustryScoringPolicy {
	return PotentIndustryScoringPolicy{
		MinHistorySessions:       cfg.MinHistorySessions,
		TaxonomyMaxAge:           time.Duration(cfg.TaxonomyMaxAgeHours) * time.Hour,
		MinGroupMembers:          cfg.MinGroupMembers,
		MinGroupCoverage:         cfg.MinGroupCoverage,
		MinFreshTaxonomyCoverage: cfg.MinFreshTaxonomyCoverage,
		MinPotencyScore:          cfg.MinPotencyScore,
		SelectionPercentile:      cfg.SelectionPercentile,
		MinAdvanceShare:          cfg.MinAdvanceShare,
		MinMedianReturn5:         cfg.MinMedianReturn5D,
		MinPersistenceSessions:   cfg.MinPersistenceSessions,
		MaxCapEqualDivergence:    cfg.MaxCapEqualDivergence,
		MaxMembersPerGroup:       cfg.MaxMembersPerGroup,
		MinMemberScore:           cfg.MinMemberScore,
		SectorBenchmarks:         cfg.SectorBenchmarks,
		IndustryBenchmarks:       cfg.IndustryBenchmarkOverrides,
	}
}

func potentIndustryHistorySymbols(
	members []store.SwingUniverseMember,
	benchmarkSymbols []string,
) []string {
	symbols := make([]string, 0, len(members)+len(benchmarkSymbols))
	for _, member := range members {
		symbols = append(symbols, strings.ToUpper(strings.TrimSpace(member.Ticker)))
	}
	symbols = append(symbols, benchmarkSymbols...)
	sort.Strings(symbols)
	return deduplicateStrings(symbols)
}

func potentIndustryResolvedBenchmarkSymbols(
	members []store.SwingUniverseMember,
	marketBenchmark string,
	cfg config.PotentIndustryConfig,
	decisionAt time.Time,
) []string {
	symbols := []string{strings.ToUpper(strings.TrimSpace(marketBenchmark))}
	groups := make(map[string][]store.SwingUniverseMember)
	for _, member := range members {
		industry := strings.TrimSpace(member.Industry)
		if !member.Eligible || industry == "" ||
			strings.EqualFold(industry, store.UnknownTaxonomy) {
			continue
		}
		key := normalizePotentLabel(industry)
		groups[key] = append(groups[key], member)
	}
	policy := potentIndustryPolicy(cfg)
	for _, group := range groups {
		fresh := potentFreshTaxonomyMembers(
			group, decisionAt, policy.TaxonomyMaxAge,
		)
		if len(fresh) == 0 {
			continue
		}
		sector, _ := potentGroupSector(fresh)
		if symbol := resolvePotentBenchmark(group[0].Industry, sector, policy); symbol != "" {
			symbols = append(symbols, symbol)
		}
	}
	sort.Strings(symbols)
	return deduplicateStrings(symbols)
}

func potentIndustryHistoryWithoutFailures(
	bars map[string][]data.Candle, failures map[string]error,
) map[string][]data.Candle {
	out := make(map[string][]data.Candle, len(bars))
	for symbol, history := range bars {
		if _, failed := potentHistoryFailure(failures, symbol); failed {
			continue
		}
		out[symbol] = history
	}
	return out
}

func potentHistoryBars(values map[string][]data.Candle, symbol string) []data.Candle {
	if bars, ok := values[symbol]; ok {
		return bars
	}
	for key, bars := range values {
		if strings.EqualFold(strings.TrimSpace(key), symbol) {
			return bars
		}
	}
	return nil
}

func potentHistoryFailure(values map[string]error, symbol string) (error, bool) {
	if failure, ok := values[symbol]; ok {
		return failure, true
	}
	for key, failure := range values {
		if strings.EqualFold(strings.TrimSpace(key), symbol) {
			return failure, true
		}
	}
	return nil, false
}

func potentIndustryApplyProviderFailures(
	evaluation *PotentIndustryEvaluation, failures map[string]error,
) {
	for i := range evaluation.Members {
		if failure, ok := potentHistoryFailure(failures, evaluation.Members[i].Ticker); ok {
			evaluation.Members[i].Outcome = "failed"
			evaluation.Members[i].Qualified = false
			evaluation.Members[i].FailureStage = "provider_history"
			evaluation.Members[i].FailureReason = failure.Error()
			evaluation.Members[i].Rank = nil
		}
	}
	for i := range evaluation.Groups {
		group := &evaluation.Groups[i]
		if failure, ok := potentHistoryFailure(failures, group.Benchmark); ok {
			group.Outcome = "failed"
			group.Qualified = false
			group.FailureStage = "provider_benchmark"
			group.FailureReason = failure.Error()
		}
	}
}

func potentIndustryStoreRows(
	runID string,
	universe *store.SwingUniverseVersion,
	universeMembers []store.SwingUniverseMember,
	asOf time.Time,
	evaluation PotentIndustryEvaluation,
) ([]store.IndustryRotationGroup, []store.IndustryRotationMember, error) {
	groups := make([]store.IndustryRotationGroup, 0, len(evaluation.Groups))
	groupKeys := make(map[string]string, len(evaluation.Groups))
	for _, item := range evaluation.Groups {
		row := store.IndustryRotationGroup{
			RunID: runID, GroupType: "industry", GroupKey: item.Industry,
			Sector: item.Sector, Industry: item.Industry, Benchmark: item.Benchmark,
			Outcome: item.Outcome, Archetype: item.Archetype,
			FailureStage:  item.FailureStage,
			FailureReason: item.FailureReason, PotencyScore: item.PotencyScore,
			ScorePercentile: item.ScorePercentile, Rank: item.Rank,
		}
		if item.Features != nil {
			features := item.Features
			row.EligibleMemberCount = features.MemberCount
			row.EvaluatedMemberCount = features.ValidMemberCount
			row.FreshTaxonomyCount = features.FreshTaxonomyCount
			row.MemberCoverage = features.Coverage
			row.FreshTaxonomyCoverage = features.FreshTaxonomyCoverage
			row.EqualWeightReturn1 = floatPtr(features.EqualReturn1)
			row.EqualWeightReturn5 = floatPtr(features.EqualReturn5)
			row.EqualWeightReturn20 = floatPtr(features.EqualReturn20)
			row.CapWeightReturn1 = floatPtr(features.CapReturn1)
			row.CapWeightReturn5 = floatPtr(features.CapReturn5)
			row.CapWeightReturn20 = floatPtr(features.CapReturn20)
			row.MedianMemberReturn1 = floatPtr(features.MedianReturn1)
			row.MedianMemberReturn5 = floatPtr(features.MedianReturn5)
			row.MedianMemberReturn20 = floatPtr(features.MedianReturn20)
			row.MarketRelativeReturn1 = floatPtr(features.MarketExcess1)
			row.MarketRelativeReturn5 = floatPtr(features.MarketExcess5)
			row.MarketRelativeReturn20 = floatPtr(features.MarketExcess20)
			row.SectorRelativeReturn1 = floatPtr(features.SectorExcess1)
			row.SectorRelativeReturn5 = floatPtr(features.SectorExcess5)
			row.SectorRelativeReturn20 = floatPtr(features.SectorExcess20)
			row.AdvanceShare = floatPtr(features.AdvanceShare)
			row.AdvanceDecline = floatPtr(features.AdvanceDecline)
			row.UpVolumeShare = floatPtr(features.UpVolumeShare)
			row.PctAboveEMA20 = floatPtr(features.AboveEMA20Share)
			row.PctAboveEMA50 = floatPtr(features.AboveEMA50Share)
			row.PctAt20DayHigh = floatPtr(features.AtHigh20Share)
			row.PctAt52WeekHigh = floatPtr(features.AtHigh252Share)
			row.MedianRVOL20 = floatPtr(features.MedianRVOL20)
			row.MedianDollarVolumeExpansion = floatPtr(features.MedianDollarExpand20)
			row.BreadthAcceleration = floatPtr(features.BreadthAcceleration)
			row.CapEqualDivergence = floatPtr(features.CapEqualDivergence)
			row.PersistenceSessions = features.PersistenceSessions
			row.FeatureCoverage = floatPtr(features.Coverage)
			evidence, err := json.Marshal(struct {
				FeatureVersion string                       `json:"feature_version"`
				Archetype      string                       `json:"archetype"`
				Features       *PotentIndustryGroupFeatures `json:"features"`
			}{potentIndustryFeatureVersion, item.Archetype, features})
			if err != nil {
				return nil, nil, fmt.Errorf(
					"marshal %s potent industry evidence: %w", item.Industry, err,
				)
			}
			row.GroupEvidence = evidence
		}
		if item.Components != nil {
			encoded, err := json.Marshal(item.Components)
			if err != nil {
				return nil, nil, fmt.Errorf(
					"marshal %s potent industry components: %w", item.Industry, err,
				)
			}
			row.Components = encoded
		}
		groups = append(groups, row)
		groupKeys[normalizePotentLabel(item.Industry)] = item.Industry
	}

	universeByTicker := make(map[string]store.SwingUniverseMember, len(universeMembers))
	for _, member := range universeMembers {
		universeByTicker[member.Ticker] = member
	}
	members := make([]store.IndustryRotationMember, 0, len(evaluation.Members))
	for _, item := range evaluation.Members {
		canonicalGroupKey, exists := groupKeys[normalizePotentLabel(item.Industry)]
		if !exists {
			continue
		}
		universeMember := universeByTicker[item.Ticker]
		row := store.IndustryRotationMember{
			RunID: runID, UniverseVersionID: universe.ID,
			GroupType: "industry", GroupKey: canonicalGroupKey, Ticker: item.Ticker,
			Outcome: item.Outcome, FailureStage: item.FailureStage,
			FailureReason: item.FailureReason, HistoryFirstAt: item.HistoryFirstAt,
			HistoryLastAt: item.HistoryLastAt, HistorySessions: item.HistorySessions,
			BarSetHash: item.BarSetHash, MemberScore: item.MemberScore,
			MemberRank: item.Rank, PromotionStatus: "not_requested",
		}
		if item.Features != nil {
			features := item.Features
			row.ClosePrice = floatPtr(features.ClosePrice)
			row.Return1, row.Return5, row.Return20 =
				floatPtr(features.Return1), floatPtr(features.Return5),
				floatPtr(features.Return20)
			row.StockVsIndustryRS = floatPtr(potentWeightedRelative(
				features.IndustryExcess1, features.IndustryExcess5,
				features.IndustryExcess20,
			))
			row.StockVsMarketRS = floatPtr(potentWeightedRelative(
				features.MarketExcess1, features.MarketExcess5,
				features.MarketExcess20,
			))
			row.FeatureCoverage = floatPtr(1)
		}
		if item.Components != nil {
			row.LiquidityScore = floatPtr(item.Components.Liquidity)
			row.TrendQualityScore = floatPtr(item.Components.TrendQuality)
			encoded, err := json.Marshal(item.Components)
			if err != nil {
				return nil, nil, fmt.Errorf(
					"marshal %s potent industry member components: %w", item.Ticker, err,
				)
			}
			row.Components = encoded
		}
		sourceEvidence, err := json.Marshal(map[string]any{
			"source": "potent_industry", "provider": universe.Provider,
			"timeframe": string(data.Timeframe1Day), "adjustment": "split",
			"as_of":               asOf.UTC().Format(time.RFC3339Nano),
			"universe_version_id": universe.ID, "universe_as_of": universe.AsOf,
			"bar_set_hash": item.BarSetHash, "market_cap_mm": universeMember.MarketCapMM,
			"taxonomy_status":   universeMember.TaxonomyStatus,
			"taxonomy_as_of":    universeMember.TaxonomyAsOf,
			"taxonomy_provider": universeMember.TaxonomyProvider,
			"taxonomy_evidence": universeMember.TaxonomyEvidence,
		})
		if err != nil {
			return nil, nil, fmt.Errorf(
				"marshal %s potent industry source evidence: %w", item.Ticker, err,
			)
		}
		row.SourceEvidence = sourceEvidence
		members = append(members, row)
	}
	return groups, members, nil
}

func potentWeightedRelative(one, five, twenty float64) float64 {
	return .15*one + .35*five + .50*twenty
}

func potentIndustryBenchmarkEvidence(
	completedMarket []data.Candle,
	marketSymbol string,
	benchmarkHistories map[string][]data.Candle,
	failures map[string]error,
	target, asOf time.Time,
	minHistory int,
) (json.RawMessage, error) {
	type benchmarkObservation struct {
		Status     string `json:"status"`
		BarSetHash string `json:"bar_set_hash,omitempty"`
		Failure    string `json:"failure,omitempty"`
	}
	observations := map[string]benchmarkObservation{
		marketSymbol: {
			Status: "complete", BarSetHash: weeklyLeaderBarSetHash(completedMarket),
		},
	}
	for symbol, bars := range benchmarkHistories {
		if failure, ok := potentHistoryFailure(failures, symbol); ok {
			observations[symbol] = benchmarkObservation{
				Status: "failed", Failure: failure.Error(),
			}
			continue
		}
		aligned, err := potentAlignedMemberBars(
			bars, completedMarket, target, asOf, minHistory,
		)
		if err != nil {
			observations[symbol] = benchmarkObservation{
				Status: "failed", Failure: err.Error(),
			}
			continue
		}
		observations[symbol] = benchmarkObservation{
			Status: "complete", BarSetHash: weeklyLeaderBarSetHash(aligned),
		}
	}
	return json.Marshal(struct {
		FeatureVersion  string                          `json:"feature_version"`
		MarketBenchmark string                          `json:"market_benchmark"`
		Benchmarks      map[string]benchmarkObservation `json:"benchmarks"`
	}{
		FeatureVersion:  potentIndustryFeatureVersion,
		MarketBenchmark: marketSymbol, Benchmarks: observations,
	})
}

func (r *PotentIndustryRunner) repository() potentIndustryRepository {
	if r.Repository != nil {
		return r.Repository
	}
	return potentIndustryStoreRepository{}
}

func (r *PotentIndustryRunner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
