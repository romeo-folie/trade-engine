package scanner

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

type scanRecorder struct {
	run                   store.ScannerRun
	candidates            map[string]*store.ScannerCandidateSnapshot
	universeEnabled       bool
	universeTTLHours      int
	resolveUniverseByID   func(context.Context, string, time.Time) (*store.SwingUniverseVersion, error)
	resolveUsableUniverse func(context.Context, time.Time, time.Duration) (*store.SwingUniverseVersion, error)
}

func newScanRecorder(s *MarketScanner, startedAt time.Time) *scanRecorder {
	minMomentum := s.PullbackMinMomentumReturnPct
	if minMomentum <= 0 {
		minMomentum = 30
	}
	cap := s.Cfg.CandidateCap
	if cap <= 0 {
		cap = 10
	}
	return &scanRecorder{
		run: store.ScannerRun{
			StartedAt:                    startedAt,
			PullbackUniverseEnabled:      s.PullbackUniverseEnabled,
			PullbackMinMomentumReturnPct: minMomentum,
			PullbackMinMarketCapMM:       pullbackMinMarketCapMM,
			PullbackMinDollarVolume:      pullbackMinDollarVolume,
			CandidateCap:                 cap,
			MinConviction:                s.Cfg.MinConviction,
			TTLDays:                      s.ttlDays(),
			UniverseVersionID:            s.UniverseVersionID,
			UniverseAsOf:                 s.UniverseAsOf,
			RegimeReference:              s.RegimeReference,
		},
		candidates:            make(map[string]*store.ScannerCandidateSnapshot),
		universeEnabled:       s.Cfg.Universe.Enabled,
		universeTTLHours:      s.Cfg.Universe.SnapshotTTLHours,
		resolveUniverseByID:   store.GetCompletedSwingUniverseVersionByIDAsOf,
		resolveUsableUniverse: store.GetUsableSwingUniverseVersion,
	}
}

func (r *scanRecorder) begin(ctx context.Context) {
	if !store.IsConnected() {
		return
	}
	if err := r.resolveUniverseLineage(ctx); err != nil {
		slog.Warn("scanner: resolve universe snapshot", "err", err)
	}
	id, err := store.BeginScannerRun(ctx, r.run)
	if err != nil {
		slog.Warn("scanner: begin universe snapshot", "err", err)
		return
	}
	r.run.ID = id
}

func (r *scanRecorder) resolveUniverseLineage(ctx context.Context) error {
	if r.run.StartedAt.IsZero() {
		r.run.StartedAt = time.Now()
	}
	if r.run.UniverseVersionID != "" {
		requestedVersionID := r.run.UniverseVersionID
		resolver := r.resolveUniverseByID
		if resolver == nil {
			resolver = store.GetCompletedSwingUniverseVersionByIDAsOf
		}
		version, err := resolver(ctx, r.run.UniverseVersionID, r.run.StartedAt)
		if err != nil {
			r.clearUniverseLineage()
			return err
		}
		if version == nil || version.ID != requestedVersionID {
			r.clearUniverseLineage()
			return nil
		}
		r.setUniverseLineage(version)
		return nil
	}

	if r.run.UniverseVersionID == "" {
		ttl := time.Duration(0)
		// A zero TTL intentionally means no age bound. newScanRecorder only
		// enables this lookup when the operator has opted into the universe.
		if r.universeEnabled {
			ttl = time.Duration(r.universeTTLHours) * time.Hour
			resolver := r.resolveUsableUniverse
			if resolver == nil {
				resolver = store.GetUsableSwingUniverseVersion
			}
			version, err := resolver(ctx, r.run.StartedAt, ttl)
			if err != nil {
				r.clearUniverseLineage()
				return err
			} else if version != nil {
				r.setUniverseLineage(version)
			}
		}
	}
	return nil
}

func (r *scanRecorder) setUniverseLineage(version *store.SwingUniverseVersion) {
	r.run.UniverseVersionID = version.ID
	asOf := version.AsOf
	r.run.UniverseAsOf = &asOf
}

func (r *scanRecorder) clearUniverseLineage() {
	r.run.UniverseVersionID = ""
	r.run.UniverseAsOf = nil
}

func (r *scanRecorder) register(candidates []SwingCandidate) {
	for _, c := range candidates {
		snapshot, ok := r.candidates[c.Ticker]
		if !ok {
			snapshot = &store.ScannerCandidateSnapshot{
				ScannerRunID:       r.run.ID,
				Ticker:             c.Ticker,
				QualificationStage: "not_evaluated",
				FinalStage:         "sourced",
			}
			r.candidates[c.Ticker] = snapshot
		}
		snapshot.Sources = mergeSources(snapshot.Sources, c.Sources)
		if snapshot.SourceScores == nil {
			snapshot.SourceScores = make(map[string]float64)
		}
		for source, score := range c.SourceScores {
			snapshot.SourceScores[source] = score
		}
		if c.GraphContext != "" {
			snapshot.GraphContext = c.GraphContext
		}
		if c.WeeklyLeaderEvidence != "" {
			snapshot.WeeklyLeaderEvidence = c.WeeklyLeaderEvidence
		}
		if c.PotentIndustryEvidence != "" {
			snapshot.PotentIndustryEvidence = c.PotentIndustryEvidence
		}
		snapshot.Price = c.Price
		snapshot.ChangePct = c.ChangePct
		snapshot.Volume = c.Volume
		snapshot.TradeCount = c.TradeCount
		snapshot.DollarVolume = c.DollarVolume
		snapshot.MarketCapMM = c.MarketCapMM
		snapshot.Return1M = c.Return1M
		snapshot.Return3M = c.Return3M
		snapshot.Return6M = c.Return6M
		if c.UniverseMember != nil {
			member := *c.UniverseMember
			snapshot.UniverseMember = &member
		}
		if c.Sector != "" {
			snapshot.Sector = c.Sector
		}
		if c.Industry != "" {
			snapshot.Industry = c.Industry
		}
		if c.TaxonomyStatus != "" {
			snapshot.TaxonomyStatus = c.TaxonomyStatus
		}
		if c.TaxonomyAsOf != nil {
			snapshot.TaxonomyAsOf = c.TaxonomyAsOf
		}
		if c.TaxonomyProvider != "" {
			snapshot.TaxonomyProvider = c.TaxonomyProvider
		}
		if len(c.TaxonomyEvidence) > 0 {
			snapshot.TaxonomyEvidence = append(snapshot.TaxonomyEvidence[:0], c.TaxonomyEvidence...)
		}
		if c.RemovalStage != "" {
			snapshot.RemovalStage = c.RemovalStage
		}
		if c.FailureStage != "" {
			snapshot.FailureStage = c.FailureStage
		}
		if c.FailureReason != "" {
			snapshot.FailureReason = c.FailureReason
		}
	}
	r.run.RawCandidateCount = len(r.candidates)
}

func mergeSources(existing, incoming []string) []string {
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	merged := make([]string, 0, len(existing)+len(incoming))
	for _, sources := range [][]string{existing, incoming} {
		for _, source := range sources {
			if source == "" {
				continue
			}
			if _, ok := seen[source]; ok {
				continue
			}
			seen[source] = struct{}{}
			merged = append(merged, source)
		}
	}
	return merged
}

func (r *scanRecorder) qualification(result pullbackQualification) {
	for ticker, outcome := range result.outcomes {
		snapshot := r.candidates[ticker]
		if snapshot == nil {
			continue
		}
		snapshot.QualificationKnown = outcome.known
		snapshot.PullbackQualified = outcome.qualified
		snapshot.QualificationStage = outcome.stage
		snapshot.QualificationReason = outcome.reason
		snapshot.MomentumRank = outcome.rank
		snapshot.Price = outcome.candidate.Price
		snapshot.Volume = outcome.candidate.Volume
		snapshot.DollarVolume = outcome.candidate.DollarVolume
		snapshot.MarketCapMM = outcome.candidate.MarketCapMM
		snapshot.Return1M = outcome.candidate.Return1M
		snapshot.Return3M = outcome.candidate.Return3M
		snapshot.Return6M = outcome.candidate.Return6M
		if outcome.candidate.Return1M != 0 || outcome.candidate.Return3M != 0 ||
			outcome.candidate.Return6M != 0 {
			snapshot.MomentumComposite = momentumComposite(outcome.candidate)
		}
	}
}

func (r *scanRecorder) markRemoved(before, after []SwingCandidate, stage string) {
	kept := make(map[string]struct{}, len(after))
	for _, c := range after {
		kept[c.Ticker] = struct{}{}
	}
	for _, c := range before {
		if _, ok := kept[c.Ticker]; ok {
			continue
		}
		snapshot := r.candidates[c.Ticker]
		if snapshot == nil {
			continue
		}
		snapshot.FinalStage = stage
		snapshot.RemovalStage = stage
		switch stage {
		case "active_watchlist":
			snapshot.ActiveWatchlistDuplicate = true
		case "requalification_unavailable":
			snapshot.QuarantineRequired = true
		}
	}
}

func (r *scanRecorder) setQuarantine(required map[string]struct{}) {
	for ticker := range required {
		if snapshot := r.candidates[ticker]; snapshot != nil {
			snapshot.QuarantineRequired = true
		}
	}
}

func (r *scanRecorder) setEligible(candidates []SwingCandidate) {
	for _, c := range candidates {
		if snapshot := r.candidates[c.Ticker]; snapshot != nil {
			snapshot.EligibleForScoring = true
			snapshot.FinalStage = "scoring"
		}
	}
	r.run.EvaluatedCandidateCount = len(candidates)
}

func (r *scanRecorder) scored(c SwingCandidate, score SwingScoreResult, err error, minConviction float64) {
	snapshot := r.candidates[c.Ticker]
	if snapshot == nil {
		return
	}
	if err != nil {
		snapshot.LLMError = err.Error()
		snapshot.FailureStage = "scoring"
		snapshot.FailureReason = err.Error()
		snapshot.FinalStage = "scoring_error"
		return
	}
	r.run.ScoredCandidateCount++
	snapshot.LLMAction = score.Action
	snapshot.LLMConviction = score.Conviction
	snapshot.LLMSetupType = score.SetupType
	snapshot.LLMStrategy = score.Strategy
	snapshot.LLMTimeframes = append([]string(nil), score.Timeframes...)
	snapshot.LLMReasoning = score.Reasoning
	snapshot.LLMRisks = append([]string(nil), score.Risks...)
	snapshot.PassedMinConviction = score.Action == "add" && score.Conviction >= minConviction
	if snapshot.PassedMinConviction {
		snapshot.FinalStage = "passed_conviction"
	} else if score.Action != "add" {
		snapshot.FinalStage = "llm_skipped"
	} else {
		snapshot.FinalStage = "below_conviction"
	}
}

func (r *scanRecorder) fallbackScored(
	c SwingCandidate, score SwingScoreResult, err error, minConviction float64,
) {
	snapshot := r.candidates[c.Ticker]
	if snapshot == nil {
		return
	}
	snapshot.Sources = append([]string(nil), c.Sources...)
	snapshot.SourceScores = make(map[string]float64, len(c.SourceScores))
	for source, sourceScore := range c.SourceScores {
		snapshot.SourceScores[source] = sourceScore
	}
	snapshot.GraphContext = c.GraphContext
	snapshot.WeeklyLeaderEvidence = c.WeeklyLeaderEvidence
	snapshot.PotentIndustryEvidence = c.PotentIndustryEvidence
	snapshot.Price = c.Price
	snapshot.ChangePct = c.ChangePct
	snapshot.Volume = c.Volume
	snapshot.TradeCount = c.TradeCount
	snapshot.DollarVolume = c.DollarVolume
	snapshot.MarketCapMM = c.MarketCapMM
	snapshot.Return1M = c.Return1M
	snapshot.Return3M = c.Return3M
	snapshot.Return6M = c.Return6M
	snapshot.UniverseMember = nil
	if c.UniverseMember != nil {
		member := *c.UniverseMember
		snapshot.UniverseMember = &member
	}
	snapshot.Sector = c.Sector
	snapshot.Industry = c.Industry
	snapshot.TaxonomyStatus = c.TaxonomyStatus
	snapshot.TaxonomyAsOf = c.TaxonomyAsOf
	snapshot.TaxonomyProvider = c.TaxonomyProvider
	snapshot.TaxonomyEvidence = append(
		snapshot.TaxonomyEvidence[:0], c.TaxonomyEvidence...,
	)
	snapshot.LLMError = ""
	snapshot.FailureStage = ""
	snapshot.FailureReason = ""
	snapshot.LLMAction = ""
	snapshot.LLMConviction = 0
	snapshot.LLMSetupType = ""
	snapshot.LLMStrategy = ""
	snapshot.LLMTimeframes = nil
	snapshot.LLMReasoning = ""
	snapshot.LLMRisks = nil
	snapshot.PassedMinConviction = false
	if err != nil {
		snapshot.LLMError = err.Error()
		snapshot.FailureStage = "scoring"
		snapshot.FailureReason = err.Error()
		snapshot.FinalStage = "scoring_error"
		return
	}
	snapshot.LLMAction = score.Action
	snapshot.LLMConviction = score.Conviction
	snapshot.LLMSetupType = score.SetupType
	snapshot.LLMStrategy = score.Strategy
	snapshot.LLMTimeframes = append([]string(nil), score.Timeframes...)
	snapshot.LLMReasoning = score.Reasoning
	snapshot.LLMRisks = append([]string(nil), score.Risks...)
	snapshot.PassedMinConviction =
		score.Action == "add" && score.Conviction >= minConviction
	if snapshot.PassedMinConviction {
		snapshot.FinalStage = "passed_conviction"
	} else if score.Action != "add" {
		snapshot.FinalStage = "llm_skipped"
	} else {
		snapshot.FinalStage = "below_conviction"
	}
}

func (r *scanRecorder) selected(selected []scoredCandidate) {
	selectedTickers := make(map[string]struct{}, len(selected))
	for _, sc := range selected {
		selectedTickers[sc.candidate.Ticker] = struct{}{}
		if snapshot := r.candidates[sc.candidate.Ticker]; snapshot != nil {
			snapshot.CapSelected = true
			snapshot.FinalStage = "selected"
		}
	}
	for _, snapshot := range r.candidates {
		if snapshot.PassedMinConviction {
			if _, ok := selectedTickers[snapshot.Ticker]; !ok {
				snapshot.FinalStage = "candidate_cap"
			}
		}
	}
}

func (r *scanRecorder) watchlistResult(
	c SwingCandidate, score SwingScoreResult, strategies []string, err error,
) {
	snapshot := r.candidates[c.Ticker]
	if snapshot == nil {
		return
	}
	snapshot.Strategies = append([]string(nil), strategies...)
	if err != nil {
		snapshot.WatchlistError = err.Error()
		snapshot.FailureStage = "watchlist"
		snapshot.FailureReason = err.Error()
		snapshot.FinalStage = "watchlist_error"
		return
	}
	snapshot.WatchlistAdded = true
	snapshot.FinalStage = "watchlist_added"
	r.run.AddedCandidateCount++
}

func (r *scanRecorder) finish(scanErr error) {
	if r.run.ID == "" {
		return
	}
	r.run.CompletedAt = time.Now()
	r.run.Status = "completed"
	if scanErr != nil {
		r.run.Status = "failed"
		r.run.Error = scanErr.Error()
	}
	candidates := make([]store.ScannerCandidateSnapshot, 0, len(r.candidates))
	for _, c := range r.candidates {
		candidates = append(candidates, *c)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Ticker < candidates[j].Ticker
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.FinishScannerRun(ctx, r.run, candidates); err != nil {
		slog.Warn("scanner: finish universe snapshot", "scan_id", r.run.ID, "err", err)
	}
}
