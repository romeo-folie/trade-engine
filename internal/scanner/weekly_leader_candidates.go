package scanner

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func (s *MarketScanner) weeklyLeaderCandidates(ctx context.Context) ([]SwingCandidate, error) {
	if !s.Cfg.WeeklyLeader.Enabled || !s.Cfg.WeeklyLeader.PromoteCandidates ||
		!store.IsConnected() {
		return nil, nil
	}
	rows, err := store.ListPendingWeeklyLeaderCandidatesAsOf(ctx, s.now())
	if err != nil {
		return nil, err
	}
	out := make([]SwingCandidate, 0, len(rows))
	for _, row := range rows {
		member := true
		taxonomyStatus := "known"
		if row.Industry == "" || row.Industry == store.UnknownTaxonomy {
			taxonomyStatus = "unknown"
		}
		out = append(out, SwingCandidate{
			Ticker:            row.Ticker,
			Price:             row.Price,
			Return1M:          row.Return20 * 100,
			Return3M:          row.Return60 * 100,
			Return6M:          row.Return126 * 100,
			DollarVolume:      row.MedianDollarVolume20,
			MarketCapMM:       row.MarketCapMM,
			Sources:           []string{"weekly_leader"},
			SourceScores:      map[string]float64{"weekly_leader": row.Score},
			UniverseMember:    &member,
			Sector:            row.Sector,
			Industry:          row.Industry,
			TaxonomyStatus:    taxonomyStatus,
			WeeklyLeaderRunID: row.RunID,
			UniverseVersionID: row.UniverseVersionID,
			UniverseAsOf:      row.UniverseAsOf,
			GraphContext: fmt.Sprintf(
				"Weekly leader outcome: rank %d, composite score %.3f across the full eligible universe.",
				row.Rank, row.Score,
			),
		})
	}
	return out, nil
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
		candidate.UniverseMember = leader.UniverseMember
		candidate.Sector, candidate.Industry = leader.Sector, leader.Industry
		candidate.TaxonomyStatus = leader.TaxonomyStatus
		candidate.WeeklyLeaderRunID = leader.WeeklyLeaderRunID
		candidate.UniverseVersionID = leader.UniverseVersionID
		candidate.UniverseAsOf = leader.UniverseAsOf
		if candidate.GraphContext == "" {
			candidate.GraphContext = leader.GraphContext
		} else {
			candidate.GraphContext += " " + leader.GraphContext
		}
	}
	return out
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
		ctx, candidate.WeeklyLeaderRunID, candidate.Ticker, status,
		scannerRunID, reason, s.now(),
	); err != nil {
		slog.Warn("scanner: mark weekly leader promotion",
			"ticker", candidate.Ticker, "status", status, "err", err)
	}
}
