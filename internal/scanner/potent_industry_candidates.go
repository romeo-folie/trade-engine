package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func (s *MarketScanner) potentIndustryCandidates(
	ctx context.Context,
) ([]SwingCandidate, error) {
	if !s.Cfg.PotentIndustry.Enabled ||
		!s.Cfg.PotentIndustry.PromoteCandidates || !store.IsConnected() {
		return nil, nil
	}
	claimToken, err := newWeeklyLeaderPromotionClaimToken()
	if err != nil {
		return nil, fmt.Errorf("generate potent industry promotion claim token: %w", err)
	}
	claimedAt := s.now()
	lease := time.Duration(s.Cfg.PotentIndustry.RunLeaseMinutes) * time.Minute
	rows, err := store.ClaimIndustryRotationCandidatesAsOf(
		ctx,
		expectedPotentIndustryPromotionSession(
			claimedAt, s.Cfg.PotentIndustry.AfterCloseDelayMinutes,
		),
		claimedAt, claimedAt, lease, claimToken,
	)
	if err != nil {
		return nil, err
	}
	out := make([]SwingCandidate, 0, len(rows))
	for _, row := range rows {
		member := true
		out = append(out, SwingCandidate{
			Ticker:                   row.Ticker,
			Price:                    row.Price,
			Return1M:                 row.Return20 * 100,
			DollarVolume:             row.DollarVolume,
			MarketCapMM:              row.MarketCapMM,
			Sources:                  []string{"potent_industry"},
			SourceScores:             map[string]float64{"potent_industry": row.MemberScore},
			UniverseMember:           &member,
			Sector:                   row.Sector,
			Industry:                 row.Industry,
			TaxonomyStatus:           row.TaxonomyStatus,
			TaxonomyAsOf:             cloneTimePointer(row.TaxonomyAsOf),
			TaxonomyProvider:         row.TaxonomyProvider,
			TaxonomyEvidence:         append(json.RawMessage(nil), row.TaxonomyEvidence...),
			PotentIndustryRunID:      row.RunID,
			PotentIndustryGroupType:  row.GroupType,
			PotentIndustryGroupKey:   row.GroupKey,
			PotentIndustryClaimToken: row.PromotionClaimToken,
			PotentIndustryEvidence: fmt.Sprintf(
				"%s %s (group rank %d, potency %.3f); member rank %d, score %.3f; 20d stock-vs-industry RS %+.1f%%",
				row.GroupKey, row.GroupArchetype, row.GroupRank, row.GroupScore,
				row.MemberRank, row.MemberScore, row.StockVsIndustryRS*100,
			),
			UniverseVersionID: row.UniverseVersionID,
			UniverseAsOf:      row.UniverseAsOf,
		})
	}
	return out, nil
}

func expectedPotentIndustryPromotionSession(
	now time.Time, afterCloseDelayMinutes int,
) time.Time {
	delay := time.Duration(afterCloseDelayMinutes) * time.Minute
	if delay < 0 {
		delay = 0
	}
	return engine.MostRecentCompletedMarketSession(now, delay).Date
}

func mergePotentIndustryCandidates(
	existing, industryCandidates []SwingCandidate,
) []SwingCandidate {
	byTicker := make(map[string]int, len(existing)+len(industryCandidates))
	out := append([]SwingCandidate(nil), existing...)
	for i := range out {
		byTicker[out[i].Ticker] = i
	}
	for _, industry := range industryCandidates {
		index, found := byTicker[industry.Ticker]
		if !found {
			byTicker[industry.Ticker] = len(out)
			out = append(out, industry)
			continue
		}
		candidate := &out[index]
		fallback := cloneSwingCandidate(*candidate)
		fallback.potentIndustryFallback = nil
		candidate.potentIndustryFallback = &fallback
		source := cloneSwingCandidate(industry)
		source.weeklyLeaderFallback = nil
		source.potentIndustryFallback = nil
		source.potentIndustrySource = nil
		candidate.potentIndustrySource = &source
		candidate.Sources = mergeSources(candidate.Sources, industry.Sources)
		if candidate.SourceScores == nil {
			candidate.SourceScores = make(map[string]float64)
		}
		candidate.SourceScores["potent_industry"] =
			industry.SourceScores["potent_industry"]
		if candidate.Price == 0 {
			candidate.Price = industry.Price
		}
		if candidate.Return1M == 0 {
			candidate.Return1M = industry.Return1M
		}
		if candidate.DollarVolume == 0 {
			candidate.DollarVolume = industry.DollarVolume
		}
		if candidate.MarketCapMM == 0 {
			candidate.MarketCapMM = industry.MarketCapMM
		}
		candidate.UniverseMember = industry.UniverseMember
		candidate.Sector, candidate.Industry = industry.Sector, industry.Industry
		candidate.TaxonomyStatus = industry.TaxonomyStatus
		candidate.TaxonomyAsOf = cloneTimePointer(industry.TaxonomyAsOf)
		candidate.TaxonomyProvider = industry.TaxonomyProvider
		candidate.TaxonomyEvidence = append(
			json.RawMessage(nil), industry.TaxonomyEvidence...,
		)
		candidate.PotentIndustryRunID = industry.PotentIndustryRunID
		candidate.PotentIndustryGroupType = industry.PotentIndustryGroupType
		candidate.PotentIndustryGroupKey = industry.PotentIndustryGroupKey
		candidate.PotentIndustryClaimToken = industry.PotentIndustryClaimToken
		candidate.PotentIndustryEvidence = industry.PotentIndustryEvidence
		candidate.UniverseVersionID = industry.UniverseVersionID
		candidate.UniverseAsOf = industry.UniverseAsOf
	}
	return out
}

func candidateAfterPromotionClaimLoss(
	candidate SwingCandidate, source string,
) (SwingCandidate, bool) {
	// Premarket evidence is the freshest dynamic state for a merged name.
	// The older weekly/industry fallback snapshots predate that merge, so
	// rebuilding a partially attributed candidate from them could silently
	// replace point-in-time price/liquidity evidence. Fail closed and release
	// the remaining claims at cycle end instead.
	if candidate.PremarketEpisodicRunID != "" && source != "political_disclosure" {
		return SwingCandidate{}, false
	}
	switch source {
	case "potent_industry":
		if candidate.potentIndustryFallback == nil {
			return SwingCandidate{}, false
		}
		fallback := cloneSwingCandidate(*candidate.potentIndustryFallback)
		copyDynamicCandidateState(&fallback, candidate)
		return fallback, len(fallback.Sources) > 0
	case "weekly_leader":
		if hasSource(candidate.Sources, "potent_industry") {
			if candidate.potentIndustrySource == nil {
				return SwingCandidate{}, false
			}
			fallback := cloneSwingCandidate(candidate)
			copyDynamicCandidateState(&fallback, candidate)
			fallback.Sources = withoutSource(fallback.Sources, "weekly_leader")
			delete(fallback.SourceScores, "weekly_leader")
			industry := candidate.potentIndustrySource
			fallback.Price = industry.Price
			fallback.Return1M = industry.Return1M
			fallback.Return3M = industry.Return3M
			fallback.Return6M = industry.Return6M
			fallback.DollarVolume = industry.DollarVolume
			fallback.MarketCapMM = industry.MarketCapMM
			fallback.UniverseMember = industry.UniverseMember
			fallback.Sector = industry.Sector
			fallback.Industry = industry.Industry
			fallback.TaxonomyStatus = industry.TaxonomyStatus
			fallback.TaxonomyAsOf = cloneTimePointer(industry.TaxonomyAsOf)
			fallback.TaxonomyProvider = industry.TaxonomyProvider
			fallback.TaxonomyEvidence = append(
				json.RawMessage(nil), industry.TaxonomyEvidence...,
			)
			fallback.WeeklyLeaderRunID = ""
			fallback.WeeklyLeaderClaimToken = ""
			fallback.WeeklyLeaderEvidence = ""
			fallback.weeklyLeaderFallback = nil
			fallback.potentIndustrySource = nil
			fallback.potentIndustryFallback = nil
			if candidate.potentIndustryFallback != nil {
				ordinary, ordinaryOK := ordinaryCandidateAfterWeeklyClaimLoss(
					*candidate.potentIndustryFallback,
				)
				if ordinaryOK {
					fallback.potentIndustryFallback = &ordinary
				}
			}
			return fallback, len(fallback.Sources) > 0
		}
		return ordinaryCandidateAfterWeeklyClaimLoss(candidate)
	case "political_disclosure":
		if candidate.politicalDisclosureFallback == nil {
			return SwingCandidate{}, false
		}
		fallback := cloneSwingCandidate(*candidate.politicalDisclosureFallback)
		copyDynamicCandidateState(&fallback, candidate)
		fallback.Sources = withoutSource(fallback.Sources, "political_disclosure")
		delete(fallback.SourceScores, "political_disclosure")
		fallback.PoliticalDisclosureClaimToken = ""
		fallback.PoliticalDisclosureClaimReportIDs = nil
		fallback.PoliticalDisclosureEvidence = ""
		fallback.PoliticalDisclosureScore = 0
		fallback.politicalDisclosureFallback = nil
		return fallback, len(fallback.Sources) > 0
	default:
		return SwingCandidate{}, false
	}
}

func copyDynamicCandidateState(target *SwingCandidate, source SwingCandidate) {
	target.News = append([]SwingNewsItem(nil), source.News...)
	target.PullbackQualified = source.PullbackQualified
	target.PullbackQualificationKnown = source.PullbackQualificationKnown
}

func withoutSource(sources []string, remove string) []string {
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		if source != remove {
			out = append(out, source)
		}
	}
	return out
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func (s *MarketScanner) markPotentIndustryCandidate(
	ctx context.Context,
	candidate SwingCandidate,
	status, scannerRunID, reason string,
) {
	if candidate.PotentIndustryRunID == "" || !store.IsConnected() {
		return
	}
	if err := store.MarkIndustryRotationPromotion(
		ctx, candidate.PotentIndustryRunID, candidate.PotentIndustryGroupType,
		candidate.PotentIndustryGroupKey, candidate.Ticker,
		candidate.PotentIndustryClaimToken, status, scannerRunID, reason, s.now(),
	); err != nil {
		slog.Warn("scanner: mark potent industry promotion",
			"ticker", candidate.Ticker, "status", status, "err", err)
	}
}

func (s *MarketScanner) markDiscoveryCandidate(
	ctx context.Context,
	candidate SwingCandidate,
	status, scannerRunID, reason string,
) {
	s.markWeeklyLeaderCandidate(ctx, candidate, status, scannerRunID, reason)
	s.markPotentIndustryCandidate(ctx, candidate, status, scannerRunID, reason)
	s.markPremarketEpisodicCandidate(ctx, candidate, status, scannerRunID, reason)
	s.markPoliticalDisclosureCandidate(ctx, candidate, status, scannerRunID, reason)
}

func (s *MarketScanner) markDiscoveryCandidates(
	ctx context.Context,
	before, after []SwingCandidate,
	status, scannerRunID, reason string,
) {
	kept := make(map[string]struct{}, len(after))
	for _, candidate := range after {
		kept[candidate.Ticker] = struct{}{}
	}
	for _, candidate := range before {
		if _, exists := kept[candidate.Ticker]; !exists {
			s.markDiscoveryCandidate(ctx, candidate, status, scannerRunID, reason)
		}
	}
}

func (s *MarketScanner) releasePotentIndustryClaims(
	ctx context.Context, candidates []SwingCandidate,
) {
	if len(candidates) == 0 || !store.IsConnected() {
		return
	}
	token := candidates[0].PotentIndustryClaimToken
	if token == "" {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := store.ReleaseIndustryRotationPromotionClaims(
		releaseCtx, token, "scanner cycle ended before a terminal outcome",
	); err != nil {
		slog.Warn("scanner: release potent industry promotion claims", "err", err)
	}
}

func scannerSourcePromotionClaims(
	candidate SwingCandidate,
) store.ScannerSourcePromotionClaims {
	var claims store.ScannerSourcePromotionClaims
	if candidate.WeeklyLeaderRunID != "" {
		claims.WeeklyLeader = &store.WeeklyLeaderPromotionClaim{
			RunID: candidate.WeeklyLeaderRunID, Ticker: candidate.Ticker,
			Token: candidate.WeeklyLeaderClaimToken,
		}
	}
	if candidate.PotentIndustryRunID != "" {
		claims.PotentIndustry = &store.PotentIndustryPromotionClaim{
			RunID:     candidate.PotentIndustryRunID,
			GroupType: candidate.PotentIndustryGroupType,
			GroupKey:  candidate.PotentIndustryGroupKey,
			Ticker:    candidate.Ticker, Token: candidate.PotentIndustryClaimToken,
		}
	}
	if candidate.PremarketEpisodicRunID != "" {
		claims.PremarketEpisodic = &store.PremarketEpisodicPromotionClaim{
			RunID: candidate.PremarketEpisodicRunID, Ticker: candidate.Ticker,
			Token: candidate.PremarketEpisodicClaimToken,
		}
	}
	if candidate.PoliticalDisclosureClaimToken != "" {
		claims.PoliticalDisclosure = &store.PoliticalDisclosurePromotionClaim{
			Ticker: candidate.Ticker, Token: candidate.PoliticalDisclosureClaimToken,
			ReportIDs: append([]string(nil), candidate.PoliticalDisclosureClaimReportIDs...),
		}
	}
	return claims
}

func (s *MarketScanner) promoteDiscoveryCandidate(
	ctx context.Context, item store.WatchlistItem, candidate SwingCandidate,
	lease *store.ScannerCycleLease,
) error {
	claims := scannerSourcePromotionClaims(candidate)
	if claims.WeeklyLeader == nil && claims.PotentIndustry == nil &&
		claims.PremarketEpisodic == nil && claims.PoliticalDisclosure == nil {
		if lease != nil {
			return store.AddWatchlistItemWithScannerLease(ctx, item, *lease)
		}
		return store.AddWatchlistItem(ctx, item)
	}
	if lease != nil {
		return store.PromoteScannerSourcesToWatchlistWithScannerLease(
			ctx, item, claims, "passed conviction and was added to the watchlist", s.now(), *lease,
		)
	}
	return store.PromoteScannerSourcesToWatchlist(
		ctx, item, claims, "passed conviction and was added to the watchlist", s.now(),
	)
}

func (s *MarketScanner) promoteDiscoveryFallback(
	ctx context.Context, item store.WatchlistItem, candidate SwingCandidate,
	lease *store.ScannerCycleLease,
) error {
	claims := scannerSourcePromotionClaims(candidate)
	if claims.WeeklyLeader == nil && claims.PotentIndustry == nil &&
		claims.PremarketEpisodic == nil && claims.PoliticalDisclosure == nil {
		if lease != nil {
			return store.AddWatchlistItemUnlessActiveWithScannerLease(ctx, item, *lease)
		}
		return store.AddWatchlistItemUnlessActive(ctx, item)
	}
	if lease != nil {
		return store.PromoteScannerSourcesToWatchlistUnlessActiveWithScannerLease(
			ctx, item, claims,
			"remaining source passed conviction after another source claim was lost",
			s.now(), *lease,
		)
	}
	return store.PromoteScannerSourcesToWatchlistUnlessActive(
		ctx, item, claims,
		"remaining source passed conviction after another source claim was lost",
		s.now(),
	)
}
