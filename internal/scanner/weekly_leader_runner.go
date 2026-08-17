package scanner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// WeeklyLeaderRunner evaluates the complete eligible point-in-time universe.
// It has no execution dependency: qualified rows become optional inputs to the
// existing scanner/LLM/watchlist path only.
type WeeklyLeaderRunner struct {
	Provider      data.BulkDailyBarsProvider
	Cfg           config.WeeklyLeaderConfig
	Now           func() time.Time
	universe      *store.SwingUniverseVersion
	exactUniverse bool
}

func (r *WeeklyLeaderRunner) Name() string { return "weekly_leader" }

func (r *WeeklyLeaderRunner) RunSessionWithUniverse(
	ctx context.Context, session engine.USMarketSession,
	universe *store.SwingUniverseVersion, decisionAt time.Time,
) error {
	copy := *r
	copy.universe = universe
	copy.exactUniverse = true
	return copy.RunSession(ctx, session, decisionAt)
}

func (r *WeeklyLeaderRunner) RunSession(
	ctx context.Context, session engine.USMarketSession, decisionAt time.Time,
) (runErr error) {
	if !r.Cfg.Enabled {
		return nil
	}
	if r.Provider == nil {
		return fmt.Errorf("weekly leader daily-bars provider is required")
	}
	if decisionAt.Before(session.Close) {
		return fmt.Errorf("weekly leader decision time precedes session close")
	}
	startedAt := r.now().UTC()
	maxAge := time.Duration(r.Cfg.UniverseMaxAgeHours) * time.Hour
	if maxAge <= 0 {
		maxAge = 36 * time.Hour
	}
	universe := r.universe
	providedUniverse := r.exactUniverse
	var err error
	if providedUniverse && universe == nil {
		return fmt.Errorf("weekly leader requires an approved universe from the session pipeline")
	}
	if !providedUniverse {
		universe, err = store.GetUsableSwingUniverseVersion(ctx, decisionAt, maxAge)
		if err != nil {
			return fmt.Errorf("load weekly leader universe: %w", err)
		}
	}
	if universe == nil {
		return fmt.Errorf("no completed swing universe available at %s within %s",
			decisionAt.Format(time.RFC3339), maxAge)
	}
	if providedUniverse && (universe.Status != "completed" || universe.QualityStatus != "approved" ||
		universe.CompletedAt == nil || universe.CompletedAt.After(decisionAt)) {
		return fmt.Errorf("weekly leader requires an approved universe available by decision time")
	}
	if providedUniverse && (universe.SessionDate == nil ||
		universe.SessionDate.Format("2006-01-02") != session.Date.Format("2006-01-02")) {
		return fmt.Errorf("weekly leader universe does not belong to target session")
	}
	members, err := store.ListEligibleSwingUniverseMembers(ctx, universe.ID)
	if err != nil {
		return fmt.Errorf("list weekly leader universe members: %w", err)
	}
	snapshot, configHash, err := weeklyLeaderConfigEvidence(r.Cfg)
	if err != nil {
		return err
	}
	benchmark := strings.ToUpper(strings.TrimSpace(r.Cfg.Benchmark))
	if benchmark == "" {
		benchmark = "SPY"
	}
	lease := time.Duration(r.Cfg.RunLeaseMinutes) * time.Minute
	if lease <= 0 {
		lease = time.Hour
	}
	claimed, ok, err := store.ClaimWeeklyLeaderRun(ctx, store.WeeklyLeaderRun{
		SessionDate:       session.Date,
		AsOf:              session.Close,
		DecisionAt:        decisionAt,
		UniverseVersionID: universe.ID,
		StartedAt:         startedAt,
		ConfigHash:        configHash,
		ConfigSnapshot:    snapshot,
		FeatureVersion:    weeklyLeaderFeatureVersion,
		Benchmark:         benchmark,
		HistoryProvider:   universe.Provider,
	}, lease)
	if err != nil {
		return err
	}
	if !ok {
		if claimed != nil && claimed.Status == "completed" {
			slog.Info("weekly leader: session already completed",
				"session_date", session.Date.Format("2006-01-02"),
				"universe_version_id", universe.ID)
			return nil
		}
		if claimed != nil && claimed.Status == "running" {
			return &weeklyLeaderRunLeaseActiveError{
				sessionDate: session.Date,
				retryAt:     claimed.LeaseUntil,
			}
		}
		status := "missing"
		if claimed != nil {
			status = claimed.Status
		}
		return fmt.Errorf("weekly leader run claim declined with unexpected status %s", status)
	}
	defer func() {
		if runErr == nil {
			return
		}
		failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := store.FailWeeklyLeaderRun(
			failCtx, claimed.ID, claimed.AttemptCount, r.now().UTC(), runErr,
		); err != nil {
			slog.Error("weekly leader: failed to persist run failure",
				"run_id", claimed.ID, "err", err)
		}
	}()

	symbols := make([]string, 0, len(members)+1)
	for _, member := range members {
		symbols = append(symbols, member.Ticker)
	}
	symbols = append(symbols, benchmark)
	sort.Strings(symbols)
	symbols = deduplicateStrings(symbols)
	historyDays := r.Cfg.HistoryCalendarDays
	if historyDays <= 0 {
		historyDays = 420
	}
	batchSize := r.Cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 200
	}
	history, err := r.Provider.FetchDailyBars(
		ctx, symbols, session.Close.AddDate(0, 0, -historyDays), session.Close, batchSize,
	)
	if err != nil {
		return fmt.Errorf("fetch weekly leader daily bars: %w", err)
	}
	if failure, exists := history.Failures[benchmark]; exists {
		return fmt.Errorf("weekly leader benchmark %s history: %w", benchmark, failure)
	}
	// A missing target-session benchmark bar means the provider has not
	// finalized the supposed session (or the exchange closed exceptionally).
	// Fail the attempt instead of publishing an empty run and false removals.
	completedBenchmark, err := completedDailyBars(
		history.Bars[benchmark], session.Date, session.Close,
	)
	if err != nil {
		return fmt.Errorf("weekly leader benchmark %s target session: %w", benchmark, err)
	}
	benchmarkBarSetHash := weeklyLeaderBarSetHash(completedBenchmark)
	evaluations := EvaluateWeeklyLeaders(
		members, history.Bars, history.Bars[benchmark],
		session.Date, session.Close, decisionAt, r.Cfg,
	)
	for i := range evaluations {
		if failure, exists := history.Failures[evaluations[i].Ticker]; exists {
			evaluations[i].Outcome = "failed"
			evaluations[i].Qualified = false
			evaluations[i].FailureStage = "provider_history"
			evaluations[i].FailureReason = failure.Error()
		}
	}
	incompleteCoverageCount := weeklyLeaderIncompleteCoverageCount(evaluations)
	coverage := weeklyLeaderEvaluationCoverage(incompleteCoverageCount, len(members))
	minCoverage := r.Cfg.MinCrossSectionCoverage
	if minCoverage <= 0 || minCoverage > 1 {
		minCoverage = .99
	}
	if incompleteCoverageCount > 0 && coverage < minCoverage {
		suppressWeeklyLeaderQualificationsOnIncompleteCoverage(
			evaluations, incompleteCoverageCount, len(members), coverage, minCoverage,
		)
	}
	qualifications, err := weeklyLeaderQualifications(
		claimed.ID, universe.ID, universe.Provider, session.Close,
		evaluations, r.Cfg,
	)
	if err != nil {
		return err
	}
	if err := store.CompleteWeeklyLeaderRun(
		ctx, claimed.ID, claimed.AttemptCount, r.now().UTC(), qualifications,
		benchmarkBarSetHash,
	); err != nil {
		return err
	}
	qualified := 0
	for _, evaluation := range evaluations {
		if evaluation.Qualified {
			qualified++
		}
	}
	slog.Info("weekly leader: session completed",
		"run_id", claimed.ID,
		"session_date", session.Date.Format("2006-01-02"),
		"eligible", len(members),
		"qualified", qualified,
		"breadth", fraction(qualified, len(members)),
	)
	return nil
}

func weeklyLeaderIncompleteCoverageCount(evaluations []WeeklyLeaderEvaluation) int {
	failed := 0
	for _, evaluation := range evaluations {
		switch evaluation.FailureStage {
		case "provider_history", "bar_validation":
			failed++
		}
	}
	return failed
}

func weeklyLeaderEvaluationCoverage(failed, eligible int) float64 {
	if eligible <= 0 {
		return 0
	}
	if failed <= 0 {
		return 1
	}
	return float64(eligible-failed) / float64(eligible)
}

func suppressWeeklyLeaderQualificationsOnIncompleteCoverage(
	evaluations []WeeklyLeaderEvaluation, failed, eligible int, coverage, minimum float64,
) {
	reason := fmt.Sprintf(
		"cross-sectional provider coverage %.4f below required %.4f: %d of %d eligible symbols failed",
		coverage, minimum, failed, eligible,
	)
	for i := range evaluations {
		if !evaluations[i].Qualified {
			continue
		}
		evaluations[i].Qualified = false
		evaluations[i].Outcome = "not_qualified"
		evaluations[i].FailureStage = "cross_section_incomplete"
		evaluations[i].FailureReason = reason
	}
}

func weeklyLeaderQualifications(
	runID, universeVersionID, provider string,
	asOf time.Time,
	evaluations []WeeklyLeaderEvaluation,
	cfg config.WeeklyLeaderConfig,
) ([]store.WeeklyLeaderQualification, error) {
	out := make([]store.WeeklyLeaderQualification, 0, len(evaluations))
	maxPromotions := cfg.MaxPromotionsPerRun
	if maxPromotions <= 0 {
		maxPromotions = 20
	}
	promotionIndexes := make(map[int]struct{})
	if cfg.PromoteCandidates {
		type rankedEvaluation struct {
			index  int
			rank   int
			ticker string
		}
		ranked := make([]rankedEvaluation, 0, len(evaluations))
		for i, evaluation := range evaluations {
			if evaluation.Qualified && evaluation.Outcome == "qualified" &&
				evaluation.Rank != nil {
				ranked = append(ranked, rankedEvaluation{
					index: i, rank: *evaluation.Rank, ticker: evaluation.Ticker,
				})
			}
		}
		sort.Slice(ranked, func(i, j int) bool {
			if ranked[i].rank != ranked[j].rank {
				return ranked[i].rank < ranked[j].rank
			}
			return ranked[i].ticker < ranked[j].ticker
		})
		for i := 0; i < len(ranked) && i < maxPromotions; i++ {
			promotionIndexes[ranked[i].index] = struct{}{}
		}
	}
	for i, evaluation := range evaluations {
		q := store.WeeklyLeaderQualification{
			RunID: runID, UniverseVersionID: universeVersionID,
			Ticker: evaluation.Ticker, Outcome: evaluation.Outcome,
			FailureStage: evaluation.FailureStage, FailureReason: evaluation.FailureReason,
			HistoryFirstAt: evaluation.HistoryFirstAt, HistoryLastAt: evaluation.HistoryLastAt,
			HistorySessions: evaluation.HistorySessions, BarSetHash: evaluation.BarSetHash,
			Sector:   evaluation.Sector,
			Industry: evaluation.Industry, MarketCapBand: evaluation.MarketCapBand,
			VolatilityBand:  evaluation.VolatilityBand,
			CompositeScore:  evaluation.CompositeScore,
			ScorePercentile: evaluation.Percentile, Rank: evaluation.Rank,
			PromotionStatus: "not_requested",
		}
		q.SourceEvidence, _ = json.Marshal(map[string]any{
			"provider":     provider,
			"timeframe":    string(data.Timeframe1Day),
			"adjustment":   "split",
			"feed":         "iex",
			"as_of":        asOf.UTC().Format(time.RFC3339Nano),
			"bar_set_hash": evaluation.BarSetHash,
		})
		if _, promote := promotionIndexes[i]; promote {
			q.PromotionStatus = "pending"
		}
		if evaluation.Features != nil {
			f := evaluation.Features
			q.ClosePrice = floatPtr(f.ClosePrice)
			q.Return5, q.Return20, q.Return60, q.Return126 =
				floatPtr(f.Return5), floatPtr(f.Return20), floatPtr(f.Return60), floatPtr(f.Return126)
			q.High52WeekProximity = floatPtr(f.High52WeekProximity)
			marketRS := mean(f.MarketRS20, f.MarketRS60, f.MarketRS126)
			q.MarketRelativeStrength = floatPtr(marketRS)
			q.IndustryRelativeStrength = f.IndustryRS60
			q.RSAcceleration = floatPtr(f.RSAcceleration)
			q.RegressionAnnualizedSlope = floatPtr(f.RegressionAnnualizedSlope)
			q.RegressionR2 = floatPtr(f.RegressionR2)
			q.Efficiency60 = floatPtr(f.Efficiency60)
			q.MaxDrawdown126 = floatPtr(f.MaxDrawdown126)
			q.NATR14 = floatPtr(f.NATR14)
			q.VolatilityAdjustedReturn = floatPtr(f.VolAdjustedReturn60)
			q.MedianDollarVolume20 = floatPtr(f.MedianDollarVolume20)
			q.DollarVolumeStability20 = floatPtr(f.DollarVolumeStability20)
			q.NormalizedOBV20 = floatPtr(f.NormalizedOBV20)
			q.CMF20 = floatPtr(f.CMF20)
			q.GapDomination60 = floatPtr(f.GapDomination60)
			encoded, err := json.Marshal(f)
			if err != nil {
				return nil, fmt.Errorf("marshal %s weekly leader features: %w", q.Ticker, err)
			}
			q.Features = encoded
		}
		if evaluation.Components != nil {
			q.FeatureCoverage = floatPtr(evaluation.Components.FeatureCoverage)
			encoded, err := json.Marshal(evaluation.Components)
			if err != nil {
				return nil, fmt.Errorf("marshal %s weekly leader components: %w", q.Ticker, err)
			}
			q.Components = encoded
		}
		out = append(out, q)
	}
	return out, nil
}

func weeklyLeaderConfigEvidence(
	cfg config.WeeklyLeaderConfig,
) (json.RawMessage, string, error) {
	evidence := struct {
		FeatureVersion string                    `json:"feature_version"`
		Config         config.WeeklyLeaderConfig `json:"config"`
	}{FeatureVersion: weeklyLeaderFeatureVersion, Config: cfg}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return nil, "", fmt.Errorf("marshal weekly leader config: %w", err)
	}
	// Operational switches and timer/lease/batch settings do not change the
	// evaluation output. Keeping them out of the compatibility hash preserves
	// transition history across observation/promotion and deployment changes.
	compatibilityConfig := struct {
		FeatureVersion          string  `json:"feature_version"`
		HistoryCalendarDays     int     `json:"history_calendar_days"`
		MinHistorySessions      int     `json:"min_history_sessions"`
		Benchmark               string  `json:"benchmark"`
		IndustryMinPeers        int     `json:"industry_min_peers"`
		SelectionPercentile     float64 `json:"selection_percentile"`
		MinCompositeScore       float64 `json:"min_composite_score"`
		MinFeatureCoverage      float64 `json:"min_feature_coverage"`
		MinCrossSectionCoverage float64 `json:"min_cross_section_coverage"`
	}{
		FeatureVersion:          weeklyLeaderFeatureVersion,
		HistoryCalendarDays:     cfg.HistoryCalendarDays,
		MinHistorySessions:      cfg.MinHistorySessions,
		Benchmark:               strings.ToUpper(strings.TrimSpace(cfg.Benchmark)),
		IndustryMinPeers:        cfg.IndustryMinPeers,
		SelectionPercentile:     cfg.SelectionPercentile,
		MinCompositeScore:       cfg.MinCompositeScore,
		MinFeatureCoverage:      cfg.MinFeatureCoverage,
		MinCrossSectionCoverage: cfg.MinCrossSectionCoverage,
	}
	compatibilityEvidence, err := json.Marshal(compatibilityConfig)
	if err != nil {
		return nil, "", fmt.Errorf("marshal weekly leader compatibility config: %w", err)
	}
	sum := sha256.Sum256(compatibilityEvidence)
	return encoded, hex.EncodeToString(sum[:]), nil
}

func deduplicateStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func (r *WeeklyLeaderRunner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func fraction(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}
