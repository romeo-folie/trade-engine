package scanner

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

// sortByResearchQueuePriority gives each bucket its own source-specific sort,
// then interleaves those rankings for the shared overflow lane. A rank of one
// from any bucket is therefore comparable to every other rank of one; raw
// scores stay strictly within their source-owned semantics.
func sortByResearchQueuePriority(items []scoredCandidate, cfg config.ResearchQueueConfig) {
	type rankedCandidate struct {
		candidate scoredCandidate
		bucket    string
		rank      int
	}
	byBucket := make(map[string][]scoredCandidate)
	for _, item := range items {
		bucket := researchQueueAdmission(item.candidate, cfg).Bucket
		byBucket[bucket] = append(byBucket[bucket], item)
	}
	ranked := make([]rankedCandidate, 0, len(items))
	for bucket, bucketItems := range byBucket {
		for index := range bucketItems {
			bucketItems[index].score.Conviction = deterministicBucketAdmissionScore(bucketItems[index].candidate, bucket)
		}
		sortByConviction(bucketItems)
		for index, item := range bucketItems {
			ranked = append(ranked, rankedCandidate{candidate: item, bucket: bucket, rank: index + 1})
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].rank != ranked[j].rank {
			return ranked[i].rank < ranked[j].rank
		}
		// Equal normalized ranks compete for overflow on candidate identity, not
		// bucket spelling. That keeps a source from winning every tie merely
		// because its bucket name happens to sort earlier.
		return ranked[i].candidate.candidate.Ticker < ranked[j].candidate.candidate.Ticker
	})
	for index, item := range ranked {
		items[index] = item.candidate
	}
}

func deterministicBucketAdmissionScore(candidate SwingCandidate, bucket string) float64 {
	if bucket == "political_disclosure" {
		return deterministicAdmissionScore(candidate)
	}
	if bucket == "market_activity" {
		return math.Max(candidate.SourceScores["mover"], candidate.SourceScores["active"])
	}
	source := map[string]string{
		"weekly_leader": "weekly_leader", "potent_industry": "potent_industry",
		"premarket_episodic": "premarket_episodic", "graph_neighbor": "graph_neighbor",
		"news_sympathy": "news_sympathy",
	}[bucket]
	if score := candidate.SourceScores[source]; !math.IsNaN(score) && !math.IsInf(score, 0) && score > 0 {
		return score
	}
	return math.Abs(candidate.ChangePct)
}

// requeueFallbackCandidate removes a lost source claim from the current slot
// and inserts its remaining-source candidate back into the unattempted queue.
// The fallback must not inherit the old source bucket's place in line.
func requeueFallbackCandidate(
	items []scoredCandidate, index int, fallback scoredCandidate, cfg config.ResearchQueueConfig,
) []scoredCandidate {
	requeued := append([]scoredCandidate{fallback}, items[index+1:]...)
	sortByResearchQueuePriority(requeued, cfg)
	return append(items[:index], requeued...)
}

// deterministicAdmissionScore ranks only within the source bucket selected
// below. It intentionally never leaves the scanner as confidence or sizing.
func deterministicAdmissionScore(candidate SwingCandidate) float64 {
	if hasSource(candidate.Sources, "political_disclosure") {
		if priority, ok := politicalDisclosureAdmissionPriority(candidate); ok {
			// Freshness is the leading political rank. Buyer breadth and a known
			// lower amount bound resolve otherwise equal filings; unknown ranges
			// remain fully eligible and simply do not receive that final boost.
			return float64(priority.latest.Unix())*1e6 +
				float64(priority.buyers)*1e3 + math.Log1p(float64(priority.minimumKnownAmount))
		}
	}
	best := 0.0
	for _, score := range candidate.SourceScores {
		if !math.IsNaN(score) && !math.IsInf(score, 0) && score > best {
			best = score
		}
	}
	if best > 0 {
		return best
	}
	if candidate.DollarVolume > 0 {
		return math.Log1p(candidate.DollarVolume)
	}
	return math.Abs(candidate.ChangePct)
}

type politicalAdmissionPriority struct {
	latest             time.Time
	filing             time.Time
	buyers             int
	minimumKnownAmount int64
}

func politicalDisclosureAdmissionPriority(candidate SwingCandidate) (politicalAdmissionPriority, bool) {
	var facts tradeprofile.PoliticalDisclosureEvidence
	if err := json.Unmarshal(candidate.PoliticalDisclosureFacts, &facts); err != nil {
		return politicalAdmissionPriority{}, false
	}
	legacy := facts.QualifyingFilerCount == 0
	latestDate := facts.LatestQualifyingPurchaseDate
	if legacy || latestDate == "" {
		latestDate = facts.LatestTransactionDate // backward-compatible evidence
	}
	latest, err := time.Parse("2006-01-02", latestDate)
	if err != nil || latest.IsZero() {
		return politicalAdmissionPriority{}, false
	}
	filingDate := facts.LatestQualifyingFilingDate
	if legacy {
		filingDate = facts.LatestFilingOrPublicationDate
	}
	filing, _ := time.Parse("2006-01-02", filingDate)
	buyers, minimumKnownAmount := facts.QualifyingBuyerCount, facts.QualifyingPurchaseMinimum
	if legacy {
		buyers, minimumKnownAmount = facts.DistinctBuyerCount, facts.PurchaseAmounts.MinimumTotal
	}
	return politicalAdmissionPriority{
		latest: latest, buyers: buyers,
		filing:             filing,
		minimumKnownAmount: minimumKnownAmount,
	}, true
}

func researchQueueAdmission(candidate SwingCandidate, cfg config.ResearchQueueConfig) *store.ResearchQueueAdmission {
	bucket := "market_activity"
	reservation := cfg.Reservations.MarketActivity
	for _, choice := range []struct {
		source, bucket string
		reservation    int
	}{
		{"political_disclosure", "political_disclosure", cfg.Reservations.PoliticalDisclosure},
		{"weekly_leader", "weekly_leader", cfg.Reservations.WeeklyLeader},
		{"potent_industry", "potent_industry", cfg.Reservations.PotentIndustry},
		{"premarket_episodic", "premarket_episodic", cfg.Reservations.PremarketEpisodic},
		{"graph_neighbor", "graph_neighbor", cfg.Reservations.GraphNeighbor},
		{"news_sympathy", "news_sympathy", cfg.Reservations.NewsSympathy},
	} {
		if hasSource(candidate.Sources, choice.source) {
			bucket, reservation = choice.bucket, choice.reservation
			break
		}
	}
	priorityFields := map[string]any{
		"bucket": bucket, "score": deterministicBucketAdmissionScore(candidate, bucket),
		"sources": candidate.Sources, "ticker": strings.ToUpper(candidate.Ticker),
	}
	if political, ok := politicalDisclosureAdmissionPriority(candidate); ok {
		priorityFields["political"] = map[string]any{
			"latest_qualifying_purchase_date": political.latest.Format("2006-01-02"),
			"qualifying_buyer_count":          political.buyers,
			"known_purchase_minimum":          political.minimumKnownAmount,
			"latest_qualifying_filing_date":   political.filing.Format("2006-01-02"),
		}
	}
	priority, _ := json.Marshal(priorityFields)
	return &store.ResearchQueueAdmission{
		Capacity: cfg.Capacity, Bucket: bucket, Reservation: reservation, Overflow: cfg.Overflow, Priority: priority,
	}
}
