package scanner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func (s *MarketScanner) weeklyLeaderCandidates(ctx context.Context) ([]SwingCandidate, error) {
	if !s.Cfg.WeeklyLeader.Enabled || !s.Cfg.WeeklyLeader.PromoteCandidates ||
		!store.IsConnected() {
		return nil, nil
	}
	claimToken, err := newWeeklyLeaderPromotionClaimToken()
	if err != nil {
		return nil, err
	}
	claimedAt := s.now()
	claimLease := time.Duration(s.Cfg.WeeklyLeader.RunLeaseMinutes) * time.Minute
	rows, err := store.ClaimWeeklyLeaderCandidatesAsOf(
		ctx, expectedWeeklyLeaderPromotionSession(
			claimedAt, s.Cfg.WeeklyLeader.AfterCloseDelayMinutes,
		),
		claimedAt, claimedAt, claimLease, claimToken,
	)
	if err != nil {
		return nil, err
	}
	out := make([]SwingCandidate, 0, len(rows))
	for _, row := range rows {
		member := true
		out = append(out, SwingCandidate{
			Ticker:                 row.Ticker,
			Price:                  row.Price,
			Return1M:               row.Return20 * 100,
			Return3M:               row.Return60 * 100,
			Return6M:               row.Return126 * 100,
			DollarVolume:           row.MedianDollarVolume20,
			MarketCapMM:            row.MarketCapMM,
			Sources:                []string{"weekly_leader"},
			SourceScores:           map[string]float64{"weekly_leader": row.Score},
			UniverseMember:         &member,
			Sector:                 row.Sector,
			Industry:               row.Industry,
			TaxonomyStatus:         row.TaxonomyStatus,
			TaxonomyAsOf:           row.TaxonomyAsOf,
			TaxonomyProvider:       row.TaxonomyProvider,
			TaxonomyEvidence:       append(json.RawMessage(nil), row.TaxonomyEvidence...),
			WeeklyLeaderRunID:      row.RunID,
			WeeklyLeaderClaimToken: row.PromotionClaimToken,
			UniverseVersionID:      row.UniverseVersionID,
			UniverseAsOf:           row.UniverseAsOf,
			WeeklyLeaderEvidence: fmt.Sprintf(
				"rank %d, composite score %.3f across the full eligible universe",
				row.Rank, row.Score,
			),
		})
	}
	return out, nil
}

func expectedWeeklyLeaderPromotionSession(
	now time.Time, afterCloseDelayMinutes int,
) time.Time {
	delay := time.Duration(afterCloseDelayMinutes) * time.Minute
	if delay < 0 {
		delay = 0
	}
	return engine.MostRecentCompletedMarketSession(now, delay).Date
}

func mergeWeeklyLeaderCandidates(
	existing, leaders []SwingCandidate,
) []SwingCandidate {
	byTicker := make(map[string]int, len(existing)+len(leaders))
	out := append([]SwingCandidate(nil), existing...)
	for i := range out {
		byTicker[out[i].Ticker] = i
	}
	for _, leader := range leaders {
		index, found := byTicker[leader.Ticker]
		if !found {
			byTicker[leader.Ticker] = len(out)
			out = append(out, leader)
			continue
		}
		candidate := &out[index]
		fallback := cloneSwingCandidate(*candidate)
		fallback.weeklyLeaderFallback = nil
		candidate.weeklyLeaderFallback = &fallback
		candidate.Sources = mergeSources(candidate.Sources, leader.Sources)
		if candidate.SourceScores == nil {
			candidate.SourceScores = make(map[string]float64)
		}
		for source, score := range leader.SourceScores {
			candidate.SourceScores[source] = score
		}
		candidate.Return1M, candidate.Return3M, candidate.Return6M =
			leader.Return1M, leader.Return3M, leader.Return6M
		candidate.DollarVolume = leader.DollarVolume
		if leader.MarketCapMM > 0 {
			candidate.MarketCapMM = leader.MarketCapMM
		}
		candidate.UniverseMember = leader.UniverseMember
		candidate.Sector, candidate.Industry = leader.Sector, leader.Industry
		candidate.TaxonomyStatus = leader.TaxonomyStatus
		candidate.TaxonomyAsOf = nil
		if leader.TaxonomyAsOf != nil {
			asOf := *leader.TaxonomyAsOf
			candidate.TaxonomyAsOf = &asOf
		}
		candidate.TaxonomyProvider = leader.TaxonomyProvider
		candidate.TaxonomyEvidence = append(
			json.RawMessage(nil), leader.TaxonomyEvidence...,
		)
		candidate.WeeklyLeaderRunID = leader.WeeklyLeaderRunID
		candidate.WeeklyLeaderClaimToken = leader.WeeklyLeaderClaimToken
		candidate.UniverseVersionID = leader.UniverseVersionID
		candidate.UniverseAsOf = leader.UniverseAsOf
		candidate.WeeklyLeaderEvidence = leader.WeeklyLeaderEvidence
	}
	return out
}

func ordinaryCandidateAfterWeeklyClaimLoss(
	candidate SwingCandidate,
) (SwingCandidate, bool) {
	if candidate.weeklyLeaderFallback == nil {
		return SwingCandidate{}, false
	}
	fallback := cloneSwingCandidate(*candidate.weeklyLeaderFallback)
	fallback.News = append([]SwingNewsItem(nil), candidate.News...)
	fallback.PullbackQualified = candidate.PullbackQualified
	fallback.PullbackQualificationKnown = candidate.PullbackQualificationKnown
	filteredSources := fallback.Sources[:0]
	for _, source := range fallback.Sources {
		if source != "weekly_leader" {
			filteredSources = append(filteredSources, source)
		}
	}
	fallback.Sources = filteredSources
	delete(fallback.SourceScores, "weekly_leader")
	fallback.WeeklyLeaderRunID = ""
	fallback.WeeklyLeaderClaimToken = ""
	fallback.WeeklyLeaderEvidence = ""
	fallback.UniverseVersionID = ""
	fallback.UniverseAsOf = time.Time{}
	fallback.weeklyLeaderFallback = nil
	return fallback, len(fallback.Sources) > 0
}

func cloneSwingCandidate(candidate SwingCandidate) SwingCandidate {
	clone := candidate
	clone.Sources = append([]string(nil), candidate.Sources...)
	clone.News = append([]SwingNewsItem(nil), candidate.News...)
	clone.PoliticalDisclosureClaimReportIDs = append(
		[]string(nil), candidate.PoliticalDisclosureClaimReportIDs...,
	)
	clone.PoliticalDisclosureFacts = append(
		json.RawMessage(nil), candidate.PoliticalDisclosureFacts...,
	)
	clone.TaxonomyEvidence = append(json.RawMessage(nil), candidate.TaxonomyEvidence...)
	if candidate.SourceScores != nil {
		clone.SourceScores = make(map[string]float64, len(candidate.SourceScores))
		for source, score := range candidate.SourceScores {
			clone.SourceScores[source] = score
		}
	}
	if candidate.UniverseMember != nil {
		member := *candidate.UniverseMember
		clone.UniverseMember = &member
	}
	if candidate.TaxonomyAsOf != nil {
		asOf := *candidate.TaxonomyAsOf
		clone.TaxonomyAsOf = &asOf
	}
	return clone
}

func (s *MarketScanner) markWeeklyLeaderCandidates(
	ctx context.Context,
	before, after []SwingCandidate,
	status, scannerRunID, reason string,
) {
	kept := make(map[string]struct{}, len(after))
	for _, candidate := range after {
		kept[candidate.Ticker] = struct{}{}
	}
	for _, candidate := range before {
		if _, exists := kept[candidate.Ticker]; exists {
			continue
		}
		s.markWeeklyLeaderCandidate(ctx, candidate, status, scannerRunID, reason)
	}
}

func (s *MarketScanner) markWeeklyLeaderCandidate(
	ctx context.Context,
	candidate SwingCandidate,
	status, scannerRunID, reason string,
) {
	if candidate.WeeklyLeaderRunID == "" || !store.IsConnected() {
		return
	}
	if err := store.MarkWeeklyLeaderPromotion(
		ctx, candidate.WeeklyLeaderRunID, candidate.Ticker,
		candidate.WeeklyLeaderClaimToken, status,
		scannerRunID, reason, s.now(),
	); err != nil {
		slog.Warn("scanner: mark weekly leader promotion",
			"ticker", candidate.Ticker, "status", status, "err", err)
	}
}

func (s *MarketScanner) releaseWeeklyLeaderClaims(
	ctx context.Context, candidates []SwingCandidate,
) {
	if len(candidates) == 0 || !store.IsConnected() {
		return
	}
	claimToken := candidates[0].WeeklyLeaderClaimToken
	if claimToken == "" {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := store.ReleaseWeeklyLeaderPromotionClaims(
		releaseCtx, claimToken, "scanner cycle ended before a terminal outcome",
	); err != nil {
		slog.Warn("scanner: release weekly leader promotion claims", "err", err)
	}
}

func newWeeklyLeaderPromotionClaimToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate weekly leader promotion claim token: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
}
