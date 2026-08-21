package scanner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

// scannerSourceLeases converts source-owned observation identity into durable
// watchlist residency leases. The identity deliberately excludes the scanner
// run and LLM score: retries and a different model opinion cannot renew a
// ticker. New immutable source evidence or materially different market/news
// observation can.
func (s *MarketScanner) scannerSourceLeases(
	candidate SwingCandidate, discovery *tradeprofile.DiscoveryContext, now time.Time,
) []store.WatchlistSourceLease {
	if discovery == nil {
		return nil
	}
	leases := make([]store.WatchlistSourceLease, 0, len(discovery.Sources))
	for _, source := range discovery.Sources {
		evidence := append(json.RawMessage(nil), discovery.EvidenceBySource[source]...)
		fingerprintEvidence := evidence
		if source == "news_sympathy" {
			// Rendered sympathy prose contains relative article age. Its immutable
			// news identity below is the lease fingerprint instead.
			fingerprintEvidence = json.RawMessage(`{}`)
		}
		identity := scannerSourceIdentity(candidate, source)
		fingerprintInput := map[string]any{
			"source": source, "identity": identity, "evidence": json.RawMessage(fingerprintEvidence),
		}
		encoded, err := json.Marshal(fingerprintInput)
		if err != nil {
			continue
		}
		digest := sha256.Sum256(encoded)
		observedAt := candidate.UniverseAsOf
		if observedAt.IsZero() {
			observedAt = now
		}
		leases = append(leases, store.WatchlistSourceLease{
			Source: source, EvidenceFingerprint: hex.EncodeToString(digest[:]),
			QualifiedAt: now, ObservedAt: observedAt, EffectiveAt: observedAt,
			ExpiresAt: now.AddDate(0, 0, s.sourceLeaseTTLDays(source)), Evidence: evidence,
		})
	}
	return leases
}

func (s *MarketScanner) sourceLeaseTTLDays(source string) int {
	switch source {
	case "weekly_leader":
		if s.Cfg.WeeklyLeader.TTLDays > 0 {
			return s.Cfg.WeeklyLeader.TTLDays
		}
	case "potent_industry":
		if s.Cfg.PotentIndustry.TTLDays > 0 {
			return s.Cfg.PotentIndustry.TTLDays
		}
	case "political_disclosure":
		return s.Cfg.PoliticalDisclosures.WatchlistLeaseWindowDays()
	}
	return s.ttlDays()
}

func maxSourceLeaseExpiry(leases []store.WatchlistSourceLease, fallback time.Time) time.Time {
	expiresAt := fallback
	for _, lease := range leases {
		if lease.ExpiresAt.After(expiresAt) {
			expiresAt = lease.ExpiresAt
		}
	}
	return expiresAt
}

func scannerSourceIdentity(candidate SwingCandidate, source string) map[string]any {
	identity := map[string]any{"ticker": strings.ToUpper(strings.TrimSpace(candidate.Ticker))}
	switch source {
	case "weekly_leader":
		identity["run_id"] = candidate.WeeklyLeaderRunID
	case "potent_industry":
		identity["run_id"] = candidate.PotentIndustryRunID
		identity["group_type"] = candidate.PotentIndustryGroupType
		identity["group_key"] = candidate.PotentIndustryGroupKey
	case "premarket_episodic":
		identity["run_id"] = candidate.PremarketEpisodicRunID
	case "political_disclosure":
		reportIDs := append([]string(nil), candidate.PoliticalDisclosureClaimReportIDs...)
		sort.Strings(reportIDs)
		identity["report_ids"] = reportIDs
		identity["facts"] = json.RawMessage(candidate.PoliticalDisclosureFacts)
	case "graph_neighbor":
		identity["relationship"] = sourceGraphContext(candidate, source)
	case "news_sympathy":
		identity["seed"] = candidate.SympathySeed
		identity["relationship"] = candidate.SympathyRelationship
		if len(candidate.News) > 0 {
			// Sympathy candidates place their immutable seed trigger first; any
			// subsequent entries are optional neighbor enrichment and must not
			// renew the source lease.
			identity["trigger"] = scannerNewsIdentity(candidate.News[:1])
		}
	case "mover", "active":
		// Preserve the source's raw screener observation. Weekly/industry and
		// premarket merges legitimately replace fields on the shared candidate
		// for scoring, but they must not renew an independent mover/activity
		// lease without new mover/activity evidence.
		identity["market"] = ordinarySourceMarketObservation(candidate, source)
		identity["news"] = scannerNewsIdentity(candidate.News)
	default:
		// Other sources without an immutable upstream ID use the bounded market
		// observation, excluding wall-clock scan time so exact retries are inert.
		identity["market"] = scannerMarketObservation(candidate)
		identity["news"] = scannerNewsIdentity(candidate.News)
	}
	return identity
}

func sourceGraphContext(candidate SwingCandidate, source string) string {
	if candidate.sourceGraphContexts != nil && candidate.sourceGraphContexts[source] != "" {
		return candidate.sourceGraphContexts[source]
	}
	return candidate.GraphContext
}

func moverMarketObservation(price, changePct float64, volume int64) map[string]any {
	return map[string]any{
		"price": price, "change_pct": changePct, "premarket_volume": volume,
	}
}

func activeMarketObservation(price float64, volume, tradeCount int64) map[string]any {
	return map[string]any{
		"price": price, "volume": volume, "trade_count": tradeCount,
	}
}

func ordinarySourceMarketObservation(candidate SwingCandidate, source string) map[string]any {
	if observation := candidate.sourceMarketObservations[source]; observation != nil {
		copy := make(map[string]any, len(observation))
		for key, value := range observation {
			copy[key] = value
		}
		return copy
	}
	// Candidates reconstructed by a legacy caller still receive a bounded,
	// source-specific identity rather than fields supplied by another source.
	if source == "mover" {
		return moverMarketObservation(candidate.Price, candidate.ChangePct, candidate.Volume)
	}
	return activeMarketObservation(candidate.Price, candidate.Volume, candidate.TradeCount)
}

func scannerNewsIdentity(news []SwingNewsItem) []map[string]any {
	news = canonicalScannerNews(news)
	values := make([]map[string]any, 0, len(news))
	for _, item := range news {
		values = append(values, map[string]any{
			"headline": item.Headline, "sentiment": item.Sentiment,
			"published_at": item.PublishedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return values
}

// canonicalScannerNews makes both persisted evidence and source fingerprints
// insensitive to upstream news ordering.
func canonicalScannerNews(news []SwingNewsItem) []SwingNewsItem {
	canonical := append([]SwingNewsItem(nil), news...)
	sort.Slice(canonical, func(i, j int) bool {
		if !canonical[i].PublishedAt.Equal(canonical[j].PublishedAt) {
			return canonical[i].PublishedAt.Before(canonical[j].PublishedAt)
		}
		if canonical[i].Headline != canonical[j].Headline {
			return canonical[i].Headline < canonical[j].Headline
		}
		return canonical[i].Sentiment < canonical[j].Sentiment
	})
	return canonical
}
