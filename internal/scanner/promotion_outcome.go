package scanner

import (
	"fmt"
	"sort"

	"github.com/romeo-folie/trade-engine/internal/events"
)

func scannerPromotionSources(candidate SwingCandidate) []events.ScannerPromotionSourcePayload {
	sources := append([]string(nil), candidate.Sources...)
	sort.Strings(sources)
	payloads := make([]events.ScannerPromotionSourcePayload, 0, len(sources))
	for _, source := range sources {
		payloads = append(payloads, events.ScannerPromotionSourcePayload{
			Source: source, Rank: candidate.promotionSourceRanks[source],
			Evidence: scannerPromotionEvidence(candidate, source),
		})
	}
	return payloads
}

func scannerPromotionEvidence(candidate SwingCandidate, source string) string {
	switch source {
	case "weekly_leader":
		return candidate.WeeklyLeaderEvidence
	case "potent_industry":
		return candidate.PotentIndustryEvidence
	case "premarket_episodic":
		return candidate.PremarketEpisodicEvidence
	case "political_disclosure":
		return candidate.PoliticalDisclosureEvidence
	case "graph_neighbor", "news_sympathy":
		return sourceGraphContext(candidate, source)
	default:
		return fmt.Sprintf("deterministic source score %.4g", candidate.SourceScores[source])
	}
}

func (s *MarketScanner) publishPromotionOutcome(
	runID string, candidate SwingCandidate, outcome, bucket, reasonCode, reason string,
) {
	if s.Bus == nil {
		return
	}
	s.Bus.Publish(events.ScannerPromotionOutcome, events.ScannerPromotionOutcomePayload{
		RunID: runID, Ticker: candidate.Ticker, Outcome: outcome,
		Sources: scannerPromotionSources(candidate), QueueBucket: bucket,
		ReasonCode: reasonCode, Reason: reason,
	})
}
