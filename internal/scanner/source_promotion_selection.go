package scanner

import (
	"math"
	"sort"
	"strings"
)

// maxPromotionAttemptsPerSource is a safety invariant rather than an
// operator-tunable queue setting. Sources may evaluate broad universes, but
// only their five best deterministic candidates can reach admission.
const maxPromotionAttemptsPerSource = 5

func minPromotionAttemptLimit(configured int) int {
	if configured <= 0 || configured > maxPromotionAttemptsPerSource {
		return maxPromotionAttemptsPerSource
	}
	return configured
}

type sourcePromotionExclusion struct {
	Ticker string
	Source string
	Rank   int
}

// selectSourcePromotionCandidates ranks every source independently, retains
// its first five distinct tickers, and merges only retained source facets back
// into candidates. It deliberately runs before the shared research queue,
// whose reservations and overflow policy remain unchanged.
func selectSourcePromotionCandidates(candidates []SwingCandidate) ([]SwingCandidate, []sourcePromotionExclusion) {
	type facet struct {
		candidate int
		source    string
		score     float64
		ticker    string
	}
	bySource := make(map[string][]facet)
	for index, candidate := range candidates {
		for _, source := range candidate.Sources {
			source = strings.TrimSpace(source)
			if source == "" {
				continue
			}
			bySource[source] = append(bySource[source], facet{
				candidate: index, source: source, score: sourcePromotionScore(candidate, source),
				ticker: normalizePromotionKey(candidate.Ticker),
			})
		}
	}
	selected := make([]map[string]int, len(candidates))
	var excluded []sourcePromotionExclusion
	for source, facets := range bySource {
		sort.SliceStable(facets, func(i, j int) bool {
			left, right := promotionScore(facets[i].score), promotionScore(facets[j].score)
			if left != right {
				return left > right
			}
			return facets[i].ticker < facets[j].ticker
		})
		seen := make(map[string]struct{}, len(facets))
		rank := 0
		for _, item := range facets {
			if _, duplicate := seen[item.ticker]; duplicate {
				continue
			}
			seen[item.ticker] = struct{}{}
			rank++
			if rank <= maxPromotionAttemptsPerSource {
				if selected[item.candidate] == nil {
					selected[item.candidate] = make(map[string]int)
				}
				selected[item.candidate][source] = rank
				continue
			}
			excluded = append(excluded, sourcePromotionExclusion{Ticker: candidates[item.candidate].Ticker, Source: source, Rank: rank})
		}
	}
	out := make([]SwingCandidate, 0, len(candidates))
	for index, candidate := range candidates {
		ranks := selected[index]
		if len(ranks) == 0 {
			continue
		}
		out = append(out, retainPromotionSources(candidate, ranks))
	}
	return out, excluded
}

func sourcePromotionScore(candidate SwingCandidate, source string) float64 {
	if source == "political_disclosure" {
		if priority, ok := politicalDisclosureAdmissionPriority(candidate); ok {
			return float64(priority.latest.Unix())*1e6 + float64(priority.buyers)*1e3 + math.Log1p(float64(priority.minimumKnownAmount))
		}
	}
	if score, ok := candidate.SourceScores[source]; ok {
		return score
	}
	return math.Inf(-1)
}

func retainPromotionSources(candidate SwingCandidate, ranks map[string]int) SwingCandidate {
	clone := candidate
	clone.Sources = make([]string, 0, len(ranks))
	clone.SourceScores = make(map[string]float64, len(ranks))
	clone.promotionSourceRanks = make(map[string]int, len(ranks))
	for _, source := range candidate.Sources {
		if rank, ok := ranks[source]; ok {
			clone.Sources = append(clone.Sources, source)
			clone.promotionSourceRanks[source] = rank
			if score, ok := candidate.SourceScores[source]; ok {
				clone.SourceScores[source] = score
			}
		}
	}
	if _, ok := ranks["weekly_leader"]; !ok {
		clone.WeeklyLeaderRunID, clone.WeeklyLeaderClaimToken, clone.WeeklyLeaderEvidence = "", "", ""
	}
	if _, ok := ranks["potent_industry"]; !ok {
		clone.PotentIndustryRunID, clone.PotentIndustryGroupType, clone.PotentIndustryGroupKey = "", "", ""
		clone.PotentIndustryClaimToken, clone.PotentIndustryEvidence = "", ""
	}
	if _, ok := ranks["premarket_episodic"]; !ok {
		clone.PremarketEpisodicRunID, clone.PremarketEpisodicClaimToken, clone.PremarketEpisodicEvidence = "", "", ""
	}
	if _, ok := ranks["political_disclosure"]; !ok {
		clone.PoliticalDisclosureClaimToken, clone.PoliticalDisclosureEvidence = "", ""
		clone.PoliticalDisclosureClaimReportIDs, clone.PoliticalDisclosureFacts = nil, nil
	}
	if _, graphSelected := ranks["graph_neighbor"]; !graphSelected {
		if _, sympathySelected := ranks["news_sympathy"]; sympathySelected {
			return clone
		}
		clone.GraphContext = ""
	}
	return clone
}
