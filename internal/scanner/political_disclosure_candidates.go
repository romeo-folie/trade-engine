package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
)

func (s *MarketScanner) politicalDisclosureCandidates(
	ctx context.Context,
) ([]SwingCandidate, error) {
	cfg := s.Cfg.PoliticalDisclosures
	if !cfg.Enabled || !cfg.PromoteCandidates || !store.IsConnected() {
		return nil, nil
	}
	token, err := newWeeklyLeaderPromotionClaimToken()
	if err != nil {
		return nil, err
	}
	now := s.now()
	rows, err := store.ClaimPoliticalDisclosureCandidates(
		ctx, now.AddDate(0, 0, -s.Cfg.PoliticalDisclosures.TTLDays), now,
		time.Duration(cfg.ClaimLeaseMinutes)*time.Minute,
		cfg.MaxCandidatesPerRun, token,
	)
	if err != nil {
		return nil, err
	}
	byTicker := make(map[string][]store.PoliticalDisclosureCandidate)
	for _, row := range rows {
		byTicker[row.Ticker] = append(byTicker[row.Ticker], row)
	}
	tickers := make([]string, 0, len(byTicker))
	for ticker := range byTicker {
		tickers = append(tickers, ticker)
	}
	sort.Strings(tickers)
	out := make([]SwingCandidate, 0, len(tickers))
	for _, ticker := range tickers {
		items := byTicker[ticker]
		reportIDs := make([]string, 0, len(items))
		parts := make([]string, 0, len(items))
		var transactionParts []string
		transactionCount := 0
		purchaseCount := 0
		latest := time.Time{}
		for _, item := range items {
			reportIDs = append(reportIDs, item.ReportID)
			transactionCount += item.TransactionCount
			if item.LatestTransaction.After(latest) {
				latest = item.LatestTransaction
			}
			var transactions []store.PoliticalDisclosureTransaction
			if err := json.Unmarshal(item.Evidence, &transactions); err == nil {
				for _, transaction := range transactions {
					if transaction.TransactionType == "purchase" {
						purchaseCount++
					}
					if len(transactionParts) < 8 {
						transactionParts = append(transactionParts, fmt.Sprintf(
							"%s %s %s (%s)", transaction.TransactionType,
							transaction.TransactionDate.Format("2006-01-02"),
							transaction.AmountText, transaction.AssetName,
						))
					}
				}
			}
			sourceLabel := item.Source
			if sourceLabel == "house" {
				sourceLabel = "House"
			} else if sourceLabel == "senate" {
				sourceLabel = "Senate"
			}
			parts = append(parts, fmt.Sprintf(
				"%s filing by %s on %s (%d transaction(s), source: %s)",
				sourceLabel, item.FilerName,
				item.FilingDate.Format("2006-01-02"), item.TransactionCount,
				item.DocumentURL,
			))
		}
		disclosureScore := scorePoliticalDisclosure(
			now, cfg.TTLDays, latest, transactionCount, purchaseCount, len(items),
		)
		out = append(out, SwingCandidate{
			Ticker: ticker, Sources: []string{"political_disclosure"},
			SourceScores:                      map[string]float64{"political_disclosure": disclosureScore},
			PoliticalDisclosureClaimToken:     token,
			PoliticalDisclosureClaimReportIDs: reportIDs,
			PoliticalDisclosureEvidence: disclosureEvidence(
				transactionCount, parts, transactionParts,
			),
			PoliticalDisclosureScore: disclosureScore,
		})
	}
	return s.hydratePoliticalDisclosureMarketData(ctx, out), nil
}

func scorePoliticalDisclosure(
	now time.Time, ttlDays int, latest time.Time,
	transactionCount, purchaseCount, filingCount int,
) float64 {
	recency := 0.0
	if !latest.IsZero() {
		recency = 1 - math.Min(1, math.Max(0,
			now.Sub(latest).Hours()/(24*float64(maxInt(1, ttlDays))),
		))
	}
	purchaseSignal := 0.0
	if transactionCount > 0 {
		purchaseSignal = float64(purchaseCount) / float64(transactionCount)
	}
	corroboration := math.Min(1, float64(filingCount)/3)
	return 0.45*recency + 0.35*purchaseSignal + 0.20*corroboration
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func disclosureEvidence(count int, filings, transactions []string) string {
	evidence := fmt.Sprintf("%d disclosed transaction(s): %s", count, strings.Join(filings, "; "))
	if len(transactions) > 0 {
		evidence += ". Transactions: " + strings.Join(transactions, "; ")
	}
	return evidence
}

func mergePoliticalDisclosureCandidates(
	existing, disclosures []SwingCandidate,
) []SwingCandidate {
	byTicker := make(map[string]int, len(existing)+len(disclosures))
	out := append([]SwingCandidate(nil), existing...)
	for index := range out {
		byTicker[out[index].Ticker] = index
	}
	for _, source := range disclosures {
		index, found := byTicker[source.Ticker]
		if !found {
			byTicker[source.Ticker] = len(out)
			out = append(out, source)
			continue
		}
		candidate := &out[index]
		fallback := cloneSwingCandidate(*candidate)
		fallback.politicalDisclosureFallback = nil
		candidate.politicalDisclosureFallback = &fallback
		attachPoliticalFallback(candidate.weeklyLeaderFallback, source)
		attachPoliticalFallback(candidate.potentIndustryFallback, source)
		candidate.Sources = mergeSources(candidate.Sources, source.Sources)
		if candidate.SourceScores == nil {
			candidate.SourceScores = make(map[string]float64)
		}
		candidate.SourceScores["political_disclosure"] = source.PoliticalDisclosureScore
		candidate.PoliticalDisclosureClaimToken = source.PoliticalDisclosureClaimToken
		candidate.PoliticalDisclosureClaimReportIDs = append(
			[]string(nil), source.PoliticalDisclosureClaimReportIDs...,
		)
		candidate.PoliticalDisclosureEvidence = source.PoliticalDisclosureEvidence
		candidate.PoliticalDisclosureScore = source.PoliticalDisclosureScore
	}
	return out
}

func attachPoliticalFallback(fallback *SwingCandidate, source SwingCandidate) {
	if fallback == nil {
		return
	}
	copy := cloneSwingCandidate(*fallback)
	copy.Sources = mergeSources(copy.Sources, source.Sources)
	if copy.SourceScores == nil {
		copy.SourceScores = make(map[string]float64)
	}
	copy.SourceScores["political_disclosure"] = source.PoliticalDisclosureScore
	copy.PoliticalDisclosureClaimToken = source.PoliticalDisclosureClaimToken
	copy.PoliticalDisclosureClaimReportIDs = append(
		[]string(nil), source.PoliticalDisclosureClaimReportIDs...,
	)
	copy.PoliticalDisclosureEvidence = source.PoliticalDisclosureEvidence
	copy.PoliticalDisclosureScore = source.PoliticalDisclosureScore
	fallback.politicalDisclosureFallback = &copy
}

// hydratePoliticalDisclosureMarketData prevents a filing-derived symbol from
// leaving this source without the tradeability and liquidity evidence required
// by the normal scanner. Provider failures release claims for a later attempt;
// definitive asset/snapshot failures are terminally recorded so malformed
// symbols do not churn forever.
func (s *MarketScanner) hydratePoliticalDisclosureMarketData(
	ctx context.Context, candidates []SwingCandidate,
) []SwingCandidate {
	political := make([]string, 0)
	for _, candidate := range candidates {
		if candidate.PoliticalDisclosureClaimToken != "" {
			political = append(political, candidate.Ticker)
		}
	}
	if len(political) == 0 {
		return candidates
	}
	if s.StockAssets == nil || s.StockSnapshots == nil {
		slog.Warn("scanner: political disclosure market-data providers unavailable")
		s.releasePoliticalDisclosureClaims(ctx, candidates)
		return nil
	}

	assets, err := s.StockAssets.FetchStockAssets(ctx)
	if err != nil {
		slog.Warn("scanner: validate political disclosure assets", "err", err)
		s.releasePoliticalDisclosureClaims(ctx, candidates)
		return nil
	}
	tradable := make(map[string]struct{}, len(assets))
	for _, asset := range assets {
		if strings.EqualFold(asset.Status, "active") && asset.Tradable &&
			strings.EqualFold(asset.AssetClass, "us_equity") {
			tradable[strings.ToUpper(strings.TrimSpace(asset.Symbol))] = struct{}{}
		}
	}
	snapshots, err := s.StockSnapshots.FetchStockSnapshots(ctx, political, 100)
	if err != nil {
		slog.Warn("scanner: hydrate political disclosure snapshots", "err", err)
		s.releasePoliticalDisclosureClaims(ctx, candidates)
		return nil
	}

	out := make([]SwingCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.PoliticalDisclosureClaimToken == "" {
			out = append(out, candidate)
			continue
		}
		if _, ok := tradable[candidate.Ticker]; !ok {
			s.rejectPoliticalDisclosureMarketData(ctx, candidate,
				"political disclosure symbol is not an active, tradeable US equity")
			continue
		}
		snapshot, ok := snapshots.Snapshots[candidate.Ticker]
		if !ok {
			if failure := snapshots.Failures[candidate.Ticker]; failure != nil {
				s.releasePoliticalDisclosureMarketData(ctx, candidate,
					fmt.Sprintf("political disclosure snapshot unavailable: %v", failure))
				continue
			}
			s.rejectPoliticalDisclosureMarketData(ctx, candidate,
				"political disclosure symbol has no current market snapshot")
			continue
		}
		hydratePoliticalSnapshot(&candidate, snapshot)
		if candidate.Price <= 0 || candidate.Volume <= 0 ||
			candidate.DollarVolume < pullbackMinDollarVolume {
			s.rejectPoliticalDisclosureMarketData(ctx, candidate, fmt.Sprintf(
				"political disclosure market evidence failed liquidity gate: price=%.4f volume=%d dollar_volume=%.2f minimum=%.2f",
				candidate.Price, candidate.Volume, candidate.DollarVolume,
				pullbackMinDollarVolume,
			))
			continue
		}
		out = append(out, candidate)
	}
	return out
}

func hydratePoliticalSnapshot(candidate *SwingCandidate, snapshot data.StockSnapshotEvidence) {
	price := snapshot.LatestTradePrice
	if price <= 0 && snapshot.MinuteBar != nil {
		price = snapshot.MinuteBar.Close
	}
	if price <= 0 && snapshot.DailyBar != nil {
		price = snapshot.DailyBar.Close
	}
	volume := int64(0)
	if snapshot.DailyBar != nil {
		volume = snapshot.DailyBar.Volume
	}
	if volume <= 0 && snapshot.PreviousDailyBar != nil {
		volume = snapshot.PreviousDailyBar.Volume
	}
	if price > 0 {
		candidate.Price = price
	}
	if volume > 0 {
		candidate.Volume = volume
	}
	if candidate.Price > 0 && candidate.Volume > 0 {
		candidate.DollarVolume = candidate.Price * float64(candidate.Volume)
	}
	if snapshot.PreviousDailyBar != nil && snapshot.PreviousDailyBar.Close > 0 &&
		candidate.Price > 0 {
		candidate.ChangePct = (candidate.Price/snapshot.PreviousDailyBar.Close - 1) * 100
	}
}

func (s *MarketScanner) rejectPoliticalDisclosureMarketData(
	ctx context.Context, candidate SwingCandidate, reason string,
) {
	s.markPoliticalDisclosureCandidate(ctx, candidate, "failed", "", reason)
	slog.Warn("scanner: political disclosure candidate rejected",
		"ticker", candidate.Ticker, "reason", reason)
}

func (s *MarketScanner) releasePoliticalDisclosureMarketData(
	ctx context.Context, candidate SwingCandidate, reason string,
) {
	s.markPoliticalDisclosureCandidate(ctx, candidate, "pending", "", reason)
	slog.Warn("scanner: political disclosure candidate released",
		"ticker", candidate.Ticker, "reason", reason)
}

func (s *MarketScanner) markPoliticalDisclosureCandidate(
	ctx context.Context, candidate SwingCandidate,
	status, scannerRunID, reason string,
) {
	if candidate.PoliticalDisclosureClaimToken == "" || !store.IsConnected() {
		return
	}
	if err := store.MarkPoliticalDisclosurePromotion(
		ctx, candidate.Ticker, candidate.PoliticalDisclosureClaimToken,
		status, scannerRunID, reason, s.now(),
	); err != nil {
		slog.Warn("scanner: mark political disclosure promotion",
			"ticker", candidate.Ticker, "status", status, "err", err)
	}
}

func (s *MarketScanner) releasePoliticalDisclosureClaims(
	ctx context.Context, candidates []SwingCandidate,
) {
	if len(candidates) == 0 || !store.IsConnected() {
		return
	}
	token := candidates[0].PoliticalDisclosureClaimToken
	if token == "" {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := store.ReleasePoliticalDisclosurePromotionClaims(
		releaseCtx, token, "scanner cycle ended before a terminal outcome",
	); err != nil {
		slog.Warn("scanner: release political disclosure promotion claims", "err", err)
	}
}
