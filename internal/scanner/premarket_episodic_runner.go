package scanner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type premarketRunLeaseActiveError struct {
	retryAt time.Time
}

func (e *premarketRunLeaseActiveError) Error() string {
	return fmt.Sprintf("premarket checkpoint is leased until %s", e.retryAt.Format(time.RFC3339))
}

type PremarketEpisodicRunner struct {
	Provider data.PremarketDataProvider
	News     data.NewsProvider
	Cfg      config.PremarketEpisodicConfig
	Now      func() time.Time
}

func (r *PremarketEpisodicRunner) RunCheckpoint(
	ctx context.Context, session engine.USMarketSession,
	checkpoint, hardCutoff time.Time,
) (runErr error) {
	if !r.Cfg.Enabled {
		return nil
	}
	if r.Provider == nil {
		return errors.New("premarket data provider is required")
	}
	if !checkpoint.Before(hardCutoff) || !checkpoint.Before(session.Open) {
		return errors.New("premarket checkpoint must precede cutoff and market open")
	}
	startedAt := r.now().UTC()
	if !startedAt.Before(hardCutoff) {
		return errors.New("premarket checkpoint skipped after hard cutoff")
	}
	universeSession := engine.MostRecentCompletedMarketSession(checkpoint, 0)
	universe, err := store.GetLatestApprovedSwingUniverseForSessionAsOf(
		ctx, universeSession.Date, checkpoint,
	)
	if err != nil {
		return fmt.Errorf("load premarket universe: %w", err)
	}
	if universe == nil {
		cause := fmt.Errorf("no approved swing universe for prior session %s available at %s",
			universeSession.Date.Format("2006-01-02"), checkpoint.Format(time.RFC3339))
		if persistErr := store.RecordScannerDependencyBlock(
			ctx, "premarket_episodic", session.Date, checkpoint, "swing_universe", cause,
		); persistErr != nil {
			return errors.Join(cause, persistErr)
		}
		return cause
	}
	if err := store.ResolveScannerDependencyBlock(
		ctx, "premarket_episodic", session.Date, checkpoint, "swing_universe",
	); err != nil {
		return err
	}
	members, err := store.ListEligibleSwingUniverseMembers(ctx, universe.ID)
	if err != nil {
		return fmt.Errorf("list premarket universe: %w", err)
	}
	configSnapshot, configHash, err := premarketConfigEvidence(r.Cfg)
	if err != nil {
		return err
	}
	lease := time.Duration(r.Cfg.RunLeaseMinutes) * time.Minute
	claimed, ok, err := store.ClaimPremarketEpisodicRun(
		ctx,
		store.PremarketEpisodicRun{
			SessionDate: session.Date, CheckpointAt: checkpoint,
			HardCutoffAt: hardCutoff, UniverseVersionID: universe.ID,
			StartedAt: startedAt, ConfigHash: configHash,
			ConfigSnapshot:     configSnapshot,
			FeatureVersion:     premarketEpisodicFeatureVersion,
			MarketDataProvider: "alpaca", Feed: "sip",
			EntitlementStatus: "available",
		},
		lease,
	)
	if err != nil {
		return err
	}
	if !ok {
		if claimed.Status == "completed" {
			slog.Info("premarket episodic: checkpoint already completed",
				"checkpoint", checkpoint)
			return nil
		}
		if claimed.Status == "running" {
			return &premarketRunLeaseActiveError{retryAt: claimed.LeaseUntil}
		}
		return fmt.Errorf("premarket run claim declined with status %s", claimed.Status)
	}
	defer func() {
		if runErr == nil {
			return
		}
		failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		entitlement := errors.Is(runErr, data.ErrPremarketEntitlement)
		if err := store.FailPremarketEpisodicRun(
			failCtx, claimed.ID, claimed.AttemptCount, r.now().UTC(),
			entitlement, runErr,
		); err != nil {
			slog.Error("premarket episodic: persist failure", "run_id", claimed.ID, "err", err)
		}
	}()

	deadlineCtx, cancel := context.WithDeadline(ctx, hardCutoff)
	defer cancel()
	symbols := make([]string, 0, len(members))
	memberByTicker := make(map[string]store.SwingUniverseMember, len(members))
	for _, member := range members {
		symbols = append(symbols, member.Ticker)
		memberByTicker[member.Ticker] = member
	}
	batchSize := r.Cfg.BatchSize
	snapshots, err := r.Provider.FetchPremarketSnapshots(
		deadlineCtx, symbols, batchSize, "sip",
	)
	if err != nil {
		return fmt.Errorf("fetch consolidated premarket snapshots: %w", err)
	}
	if len(snapshots.Failures) > 0 {
		return fmt.Errorf(
			"consolidated premarket snapshot coverage incomplete: %d of %d symbols failed",
			len(snapshots.Failures), len(symbols),
		)
	}
	shortlist := premarketShortlist(snapshots.Snapshots, memberByTicker, r.Cfg)
	if len(shortlist) > r.Cfg.MaxCandidatesPerRun {
		shortlist = shortlist[:r.Cfg.MaxCandidatesPerRun]
	}
	evidence, err := r.Provider.FetchPremarketEvidence(
		deadlineCtx, shortlist, session.Date, checkpoint,
		r.Cfg.HistorySessions, batchSize, "sip",
	)
	if err != nil {
		return fmt.Errorf("fetch consolidated premarket evidence: %w", err)
	}
	evaluations := make([]PremarketEpisodicEvaluation, 0, len(shortlist))
	rawByTicker := make(map[string]json.RawMessage, len(shortlist))
	for _, ticker := range shortlist {
		member := memberByTicker[ticker]
		marketCap := 0.0
		if member.MarketCapMM != nil {
			marketCap = *member.MarketCapMM
		}
		catalyst := r.catalyst(deadlineCtx, ticker, checkpoint)
		item, found := evidence.Evidence[ticker]
		if failure, failed := evidence.Failures[ticker]; failed || !found {
			reason := "provider omitted detailed premarket evidence"
			if failed {
				reason = failure.Error()
			}
			evaluations = append(evaluations, PremarketEpisodicEvaluation{
				Ticker: ticker, Outcome: "failed", RemovalStage: "provider_evidence",
				FailureReason: reason, Catalyst: catalyst,
				Features: PremarketEpisodicFeatures{
					MarketCapMM: marketCap, FloatStatus: "unavailable",
				},
			})
			continue
		}
		rawByTicker[ticker], _ = json.Marshal(struct {
			Provider     string                 `json:"provider"`
			CheckpointAt time.Time              `json:"checkpoint_at"`
			Evidence     data.PremarketEvidence `json:"evidence"`
		}{"alpaca", checkpoint, item})
		evaluations = append(evaluations, EvaluatePremarketEpisodic(
			ticker, item, marketCap, checkpoint, catalyst, r.Cfg,
		))
	}
	if incomplete := premarketEvidenceFailureCount(shortlist, evidence); incomplete > 0 {
		reason := fmt.Sprintf(
			"detailed premarket coverage incomplete: %d of %d shortlisted symbols failed",
			incomplete, len(shortlist),
		)
		for i := range evaluations {
			if evaluations[i].Qualified {
				evaluations[i].Qualified = false
				evaluations[i].Outcome = "not_qualified"
				evaluations[i].RemovalStage = "cross_section_incomplete"
				evaluations[i].FailureReason = reason
			}
		}
	}
	rankPremarketEvaluations(evaluations)
	rows, err := r.storeCandidates(
		claimed.ID, universe.ID, evaluations, rawByTicker,
	)
	if err != nil {
		return err
	}
	completedAt := r.now().UTC()
	if err := store.CompletePremarketEpisodicRun(
		deadlineCtx, claimed.ID, claimed.AttemptCount, completedAt,
		len(members), len(shortlist), rows,
	); err != nil {
		return err
	}
	slog.Info("premarket episodic: checkpoint completed",
		"run_id", claimed.ID, "session_date", session.Date.Format("2006-01-02"),
		"checkpoint", checkpoint, "shortlisted", len(shortlist),
		"qualified", countPremarketQualified(evaluations),
		"promote_candidates", r.Cfg.PromoteCandidates,
	)
	return nil
}

func premarketEvidenceFailureCount(
	shortlist []string, result data.PremarketBatchResult,
) int {
	count := 0
	for _, ticker := range shortlist {
		if _, failed := result.Failures[ticker]; failed {
			count++
			continue
		}
		if _, found := result.Evidence[ticker]; !found {
			count++
		}
	}
	return count
}

func premarketShortlist(
	snapshots map[string]data.StockSnapshotEvidence,
	members map[string]store.SwingUniverseMember,
	cfg config.PremarketEpisodicConfig,
) []string {
	type candidate struct {
		ticker string
		gap    float64
	}
	var candidates []candidate
	for ticker, snapshot := range snapshots {
		member, eligible := members[ticker]
		if !eligible || snapshot.PreviousDailyBar == nil {
			continue
		}
		price := snapshot.LatestTradePrice
		if price <= 0 && snapshot.MinuteBar != nil {
			price = snapshot.MinuteBar.Close
		}
		if price <= 0 || snapshot.PreviousDailyBar.Close <= 0 {
			continue
		}
		marketCap := 0.0
		if member.MarketCapMM != nil {
			marketCap = *member.MarketCapMM
		}
		gap := (price/snapshot.PreviousDailyBar.Close - 1) * 100
		if math.Abs(gap) >= cfg.MinGapPct && price >= cfg.MinPrice &&
			marketCap >= cfg.MinMarketCapMM {
			candidates = append(candidates, candidate{ticker: ticker, gap: gap})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := math.Abs(candidates[i].gap), math.Abs(candidates[j].gap)
		if left != right {
			return left > right
		}
		return candidates[i].ticker < candidates[j].ticker
	})
	out := make([]string, len(candidates))
	for i := range candidates {
		out[i] = candidates[i].ticker
	}
	return out
}

func (r *PremarketEpisodicRunner) catalyst(
	ctx context.Context, ticker string, checkpoint time.Time,
) PremarketCatalyst {
	if r.News == nil {
		return PremarketCatalyst{Status: "provider_unavailable"}
	}
	windowStart := checkpoint.Add(-time.Duration(r.Cfg.NewsLookbackHours) * time.Hour)
	items, err := r.News.FetchNews(ctx, ticker, windowStart)
	if err != nil {
		return PremarketCatalyst{Status: "provider_unavailable"}
	}
	var latest *data.NewsItem
	for i := range items {
		if items[i].PublishedAt.Before(windowStart) ||
			items[i].PublishedAt.After(checkpoint) {
			continue
		}
		if latest == nil || items[i].PublishedAt.After(latest.PublishedAt) {
			latest = &items[i]
		}
	}
	if latest == nil {
		return PremarketCatalyst{Status: "confirmed_absent"}
	}
	return PremarketCatalyst{
		Status: "confirmed", ExternalID: latest.ExternalID,
		Headline: latest.Headline, Source: latest.Source, URL: latest.URL,
		PublishedAt: latest.PublishedAt,
	}
}

func rankPremarketEvaluations(evaluations []PremarketEpisodicEvaluation) {
	sort.SliceStable(evaluations, func(i, j int) bool {
		if evaluations[i].Qualified != evaluations[j].Qualified {
			return evaluations[i].Qualified
		}
		if evaluations[i].Score != evaluations[j].Score {
			return evaluations[i].Score > evaluations[j].Score
		}
		return evaluations[i].Ticker < evaluations[j].Ticker
	})
}

func (r *PremarketEpisodicRunner) storeCandidates(
	runID, universeID string,
	evaluations []PremarketEpisodicEvaluation,
	rawByTicker map[string]json.RawMessage,
) ([]store.PremarketEpisodicCandidate, error) {
	thresholds, err := json.Marshal(r.Cfg)
	if err != nil {
		return nil, err
	}
	rows := make([]store.PremarketEpisodicCandidate, 0, len(evaluations))
	qualifiedRank := 0
	for _, evaluation := range evaluations {
		var rank *int
		promotionStatus := "not_requested"
		if evaluation.Qualified {
			qualifiedRank++
			rank = intPointer(qualifiedRank)
			if r.Cfg.PromoteCandidates &&
				qualifiedRank <= minPromotionAttemptLimit(r.Cfg.MaxPromotionsPerRun) {
				promotionStatus = "pending"
			}
		}
		catalyst, err := json.Marshal(evaluation.Catalyst)
		if err != nil {
			return nil, err
		}
		f := evaluation.Features
		row := store.PremarketEpisodicCandidate{
			RunID: runID, UniverseVersionID: universeID, Ticker: evaluation.Ticker,
			Outcome: evaluation.Outcome, RemovalStage: evaluation.RemovalStage,
			FailureReason:     evaluation.FailureReason,
			CatalystStatus:    evaluation.Catalyst.Status,
			CatalystReference: catalyst, RawEvidence: rawByTicker[evaluation.Ticker],
			Thresholds: thresholds, PromotionStatus: promotionStatus,
			FloatStatus: "unavailable", Rank: rank,
		}
		if row.CatalystStatus == "" {
			row.CatalystStatus = "provider_unavailable"
		}
		if evaluation.Outcome != "failed" {
			row.Score = floatPointer(evaluation.Score)
			row.GapPct = floatPointer(f.GapPct)
			row.PremarketVolume = int64Pointer(f.PremarketVolume)
			row.TimeAdjustedRVOL = floatPointer(f.TimeAdjustedRVOL)
			row.DollarVolume = floatPointer(f.DollarVolume)
			row.TradeCount = int64Pointer(f.TradeCount)
			row.SpreadBPS = floatPointer(f.SpreadBPS)
			row.QuoteAgeSeconds = floatPointer(f.QuoteAgeSeconds)
			row.BarCoverage = floatPointer(f.BarCoverage)
			row.PremarketHigh = floatPointer(f.PremarketHigh)
			row.PremarketLow = floatPointer(f.PremarketLow)
			row.PremarketLocation = floatPointer(f.PremarketLocation)
			row.LastPrice = floatPointer(f.LastPrice)
			row.PreviousClose = floatPointer(f.PreviousClose)
			row.MarketCapMM = floatPointer(f.MarketCapMM)
			row.MarketCapRisk = f.MarketCapRisk
			row.ADR14Pct = floatPointer(f.ADR14Pct)
			row.NATR14Pct = floatPointer(f.NATR14Pct)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func premarketConfigEvidence(
	cfg config.PremarketEpisodicConfig,
) (json.RawMessage, string, error) {
	snapshot, err := json.Marshal(struct {
		FeatureVersion string                         `json:"feature_version"`
		Config         config.PremarketEpisodicConfig `json:"config"`
	}{premarketEpisodicFeatureVersion, cfg})
	if err != nil {
		return nil, "", err
	}
	compatibility := cfg
	compatibility.Enabled = false
	compatibility.Checkpoints = nil
	compatibility.RunLeaseMinutes = 0
	compatibility.BatchSize = 0
	compatibility.UniverseMaxAgeHours = 0
	encoded, err := json.Marshal(struct {
		FeatureVersion string                         `json:"feature_version"`
		Config         config.PremarketEpisodicConfig `json:"config"`
	}{premarketEpisodicFeatureVersion, compatibility})
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(encoded)
	return snapshot, hex.EncodeToString(sum[:]), nil
}

func countPremarketQualified(evaluations []PremarketEpisodicEvaluation) int {
	count := 0
	for _, evaluation := range evaluations {
		if evaluation.Qualified {
			count++
		}
	}
	return count
}

func intPointer(value int) *int           { return &value }
func int64Pointer(value int64) *int64     { return &value }
func floatPointer(value float64) *float64 { return &value }

func (r *PremarketEpisodicRunner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
