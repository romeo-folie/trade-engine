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

func (s *MarketScanner) premarketEpisodicCandidates(
	ctx context.Context,
) ([]SwingCandidate, error) {
	cfg := s.Cfg.PremarketEpisodic
	if !cfg.Enabled || !cfg.PromoteCandidates || !store.IsConnected() {
		return nil, nil
	}
	now := s.now()
	session, ok := engine.MarketSessionForDate(now)
	if !ok || now.Before(session.Open) {
		return nil, nil
	}
	token, err := newWeeklyLeaderPromotionClaimToken()
	if err != nil {
		return nil, err
	}
	rows, err := store.ClaimPremarketEpisodicCandidatesAsOf(
		ctx, session.Date, now, now,
		time.Duration(cfg.RunLeaseMinutes)*time.Minute, token,
	)
	if err != nil {
		return nil, err
	}
	out := make([]SwingCandidate, 0, len(rows))
	for _, row := range rows {
		member := true
		catalyst := PremarketCatalyst{}
		_ = json.Unmarshal(row.CatalystReference, &catalyst)
		evidence := fmt.Sprintf(
			"gap %+.1f%%; %d shares, %.1fx same-clock RVOL, $%.0f premarket dollar volume; %d trades; %.1f bps spread; catalyst %s",
			derefFloat(row.GapPct), derefInt64(row.PremarketVolume),
			derefFloat(row.TimeAdjustedRVOL), derefFloat(row.DollarVolume),
			derefInt64(row.TradeCount), derefFloat(row.SpreadBPS),
			row.CatalystStatus,
		)
		if catalyst.Headline != "" {
			evidence += fmt.Sprintf(" (%s)", catalyst.Headline)
		}
		out = append(out, SwingCandidate{
			Ticker: row.Ticker, Price: derefFloat(row.LastPrice),
			ChangePct:    derefFloat(row.GapPct),
			Volume:       derefInt64(row.PremarketVolume),
			TradeCount:   derefInt64(row.TradeCount),
			DollarVolume: derefFloat(row.DollarVolume),
			MarketCapMM:  derefFloat(row.MarketCapMM),
			Sources:      []string{"premarket_episodic"},
			SourceScores: map[string]float64{
				"premarket_episodic": derefFloat(row.Score),
			},
			UniverseMember: &member,
			Sector:         row.Sector, Industry: row.Industry,
			TaxonomyStatus:              row.TaxonomyStatus,
			TaxonomyAsOf:                cloneTimePointer(row.TaxonomyAsOf),
			TaxonomyProvider:            row.TaxonomyProvider,
			TaxonomyEvidence:            append(json.RawMessage(nil), row.TaxonomyEvidence...),
			PremarketEpisodicRunID:      row.RunID,
			PremarketEpisodicClaimToken: row.PromotionClaimToken,
			PremarketEpisodicEvidence:   evidence,
			UniverseVersionID:           row.UniverseVersionID,
			UniverseAsOf:                row.UniverseAsOf,
		})
	}
	return out, nil
}

func mergePremarketEpisodicCandidates(
	existing, premarket []SwingCandidate,
) []SwingCandidate {
	byTicker := make(map[string]int, len(existing)+len(premarket))
	out := append([]SwingCandidate(nil), existing...)
	for i := range out {
		byTicker[out[i].Ticker] = i
	}
	for _, source := range premarket {
		index, found := byTicker[source.Ticker]
		if !found {
			byTicker[source.Ticker] = len(out)
			out = append(out, source)
			continue
		}
		candidate := &out[index]
		candidate.Sources = mergeSources(candidate.Sources, source.Sources)
		if candidate.SourceScores == nil {
			candidate.SourceScores = make(map[string]float64)
		}
		candidate.SourceScores["premarket_episodic"] =
			source.SourceScores["premarket_episodic"]
		// Premarket evidence is the freshest price/liquidity observation and
		// should be what the scorer sees for a multi-source name.
		candidate.Price = source.Price
		candidate.ChangePct = source.ChangePct
		candidate.Volume = source.Volume
		candidate.TradeCount = source.TradeCount
		candidate.DollarVolume = source.DollarVolume
		if source.MarketCapMM > 0 {
			candidate.MarketCapMM = source.MarketCapMM
		}
		candidate.UniverseMember = source.UniverseMember
		candidate.Sector, candidate.Industry = source.Sector, source.Industry
		candidate.TaxonomyStatus = source.TaxonomyStatus
		candidate.TaxonomyAsOf = cloneTimePointer(source.TaxonomyAsOf)
		candidate.TaxonomyProvider = source.TaxonomyProvider
		candidate.TaxonomyEvidence = append(
			json.RawMessage(nil), source.TaxonomyEvidence...,
		)
		candidate.PremarketEpisodicRunID = source.PremarketEpisodicRunID
		candidate.PremarketEpisodicClaimToken = source.PremarketEpisodicClaimToken
		candidate.PremarketEpisodicEvidence = source.PremarketEpisodicEvidence
		candidate.UniverseVersionID = source.UniverseVersionID
		candidate.UniverseAsOf = source.UniverseAsOf
	}
	return out
}

func (s *MarketScanner) markPremarketEpisodicCandidate(
	ctx context.Context, candidate SwingCandidate,
	status, scannerRunID, reason string,
) {
	if candidate.PremarketEpisodicRunID == "" || !store.IsConnected() {
		return
	}
	if err := store.MarkPremarketEpisodicPromotion(
		ctx, candidate.PremarketEpisodicRunID, candidate.Ticker,
		candidate.PremarketEpisodicClaimToken, status, scannerRunID,
		reason, s.now(),
	); err != nil {
		slog.Warn("scanner: mark premarket episodic promotion",
			"ticker", candidate.Ticker, "status", status, "err", err)
	}
}

func (s *MarketScanner) releasePremarketEpisodicClaims(
	ctx context.Context, candidates []SwingCandidate,
) {
	if len(candidates) == 0 || !store.IsConnected() {
		return
	}
	token := candidates[0].PremarketEpisodicClaimToken
	if token == "" {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := store.ReleasePremarketEpisodicPromotionClaims(
		releaseCtx, token, "scanner cycle ended before a terminal outcome",
	); err != nil {
		slog.Warn("scanner: release premarket episodic claims", "err", err)
	}
}

func derefFloat(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
