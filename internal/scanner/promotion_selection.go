package scanner

import (
	"math"
	"sort"
	"strings"
)

// PromotionCandidate is the source-neutral evidence needed to choose a
// bounded, diversified set of candidates for watchlist promotion. Key must be
// stable (normally the normalized ticker). Correlations are keyed by the
// other candidate's Key and may be supplied in either direction.
// ExposureCorrelations are keyed by active watchlist/open-position ticker and
// are checked before candidate-to-candidate caps.
type PromotionCandidate struct {
	// Index identifies the candidate in the caller's original slice. It keeps
	// audit attribution correct when normalized keys collide.
	Index                int
	Key                  string
	Industry             string
	TaxonomyStatus       string
	TaxonomyCapExcluded  bool
	Score                float64
	Correlations         map[string]float64
	ExposureCorrelations map[string]float64
}

// PromotionSelection records both accepted candidates and deterministic
// rejections so callers can persist an auditable reason for every cap.
type PromotionSelection struct {
	Selected []PromotionCandidate
	Rejected []PromotionRejection
}

type PromotionRejection struct {
	Candidate           PromotionCandidate
	Reason              string
	ConflictKey         string
	ConflictCorrelation float64
}

// SelectPromotionCandidates ranks candidates by descending score (then key),
// applies the total and per-industry caps, and rejects candidates whose
// absolute correlation with an already selected candidate reaches the
// configured threshold. A non-positive cap or correlation threshold disables
// that constraint. The input is never mutated and output ordering is stable.
func SelectPromotionCandidates(
	candidates []PromotionCandidate, maxTotal, maxPerIndustry int,
	maxAbsCorrelation float64,
) PromotionSelection {
	ordered := append([]PromotionCandidate(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := promotionScore(ordered[i].Score), promotionScore(ordered[j].Score)
		if left != right {
			return left > right
		}
		return strings.TrimSpace(ordered[i].Key) < strings.TrimSpace(ordered[j].Key)
	})
	if maxAbsCorrelation < 0 || math.IsNaN(maxAbsCorrelation) {
		maxAbsCorrelation = 0
	}

	result := PromotionSelection{
		Selected: make([]PromotionCandidate, 0, len(ordered)),
		Rejected: make([]PromotionRejection, 0),
	}
	seen := make(map[string]struct{}, len(ordered))
	byIndustry := make(map[string]int)
	for _, candidate := range ordered {
		key := normalizePromotionKey(candidate.Key)
		if key == "" {
			result.Rejected = append(result.Rejected, PromotionRejection{
				Candidate: candidate, Reason: "missing_key",
			})
			continue
		}
		if _, exists := seen[key]; exists {
			result.Rejected = append(result.Rejected, PromotionRejection{
				Candidate: candidate, Reason: "duplicate_key",
			})
			continue
		}
		seen[key] = struct{}{}
		candidate.Key = key
		industry := strings.TrimSpace(candidate.Industry)
		if isUnknownIndustry(industry) {
			industry = ""
		}
		if maxTotal > 0 && len(result.Selected) >= maxTotal {
			result.Rejected = append(result.Rejected, PromotionRejection{
				Candidate: candidate, Reason: "total_cap",
			})
			continue
		}
		// Candidates without point-in-time taxonomy evidence are not assigned to
		// a synthetic shared industry. Otherwise a taxonomy outage would make a
		// per-industry cap suppress nearly every legacy mover/active candidate.
		industryKey := strings.ToLower(industry)
		if industry != "" && !candidate.TaxonomyCapExcluded && taxonomyCapEligible(candidate.TaxonomyStatus) &&
			maxPerIndustry > 0 && byIndustry[industryKey] >= maxPerIndustry {
			result.Rejected = append(result.Rejected, PromotionRejection{
				Candidate: candidate, Reason: "industry_cap",
			})
			continue
		}
		if maxAbsCorrelation > 0 {
			if conflict, correlation := promotionExposureConflict(candidate, maxAbsCorrelation); conflict != "" {
				result.Rejected = append(result.Rejected, PromotionRejection{
					Candidate: candidate, Reason: "exposure_correlation_cap", ConflictKey: conflict, ConflictCorrelation: correlation,
				})
				continue
			}
			if conflict, correlation := promotionCorrelationConflict(candidate, result.Selected, maxAbsCorrelation); conflict != "" {
				result.Rejected = append(result.Rejected, PromotionRejection{
					Candidate: candidate, Reason: "correlation_cap", ConflictKey: conflict, ConflictCorrelation: correlation,
				})
				continue
			}
		}
		result.Selected = append(result.Selected, candidate)
		if industry != "" && !candidate.TaxonomyCapExcluded && taxonomyCapEligible(candidate.TaxonomyStatus) {
			byIndustry[industryKey]++
		}
	}
	return result
}

func promotionExposureConflict(candidate PromotionCandidate, threshold float64) (string, float64) {
	keys := make([]string, 0, len(candidate.ExposureCorrelations))
	for key := range candidate.ExposureCorrelations {
		keys = append(keys, normalizePromotionKey(key))
	}
	sort.Strings(keys)
	for _, key := range keys {
		correlation, ok := normalizedCorrelation(candidate.ExposureCorrelations, key)
		if ok && math.Abs(correlation) >= threshold {
			return key, correlation
		}
	}
	return "", 0
}

func selectScoredForPromotion(
	scored []scoredCandidate, slots, maxPerIndustry int,
) ([]scoredCandidate, []PromotionRejection) {
	return selectScoredForPromotionWithCorrelation(scored, slots, maxPerIndustry, 0)
}

func selectScoredForPromotionWithCorrelation(
	scored []scoredCandidate, slots, maxPerIndustry int,
	maxAbsCorrelation float64,
) ([]scoredCandidate, []PromotionRejection) {
	candidates := make([]PromotionCandidate, 0, len(scored))
	for index, scoredCandidate := range scored {
		candidates = append(candidates, PromotionCandidate{
			Index: index, Key: scoredCandidate.candidate.Ticker,
			Industry:             scoredCandidate.candidate.Industry,
			TaxonomyStatus:       scoredCandidate.candidate.TaxonomyStatus,
			TaxonomyCapExcluded:  scoredCandidate.candidate.promotionTaxonomyExcluded,
			Score:                scoredCandidate.score.Conviction,
			Correlations:         scoredCandidate.correlations,
			ExposureCorrelations: scoredCandidate.exposureCorrelations,
		})
	}
	selection := SelectPromotionCandidates(candidates, slots, maxPerIndustry, maxAbsCorrelation)
	filtered := make([]scoredCandidate, 0, len(selection.Selected))
	for _, candidate := range selection.Selected {
		if candidate.Index >= 0 && candidate.Index < len(scored) {
			filtered = append(filtered, scored[candidate.Index])
		}
	}
	return filtered, selection.Rejected
}

// promotionIndustryConflict checks a fallback candidate against the other
// candidates already admitted by the promotion selector. Claim-loss fallbacks
// are rescored later, so their effective taxonomy must be checked again before
// the watchlist write.
func promotionIndustryConflict(
	candidate SwingCandidate, peers []scoredCandidate, maxPerIndustry int,
) string {
	if maxPerIndustry <= 0 || candidate.promotionTaxonomyExcluded || !taxonomyCapEligible(candidate.TaxonomyStatus) {
		return ""
	}
	industry := strings.TrimSpace(candidate.Industry)
	if isUnknownIndustry(industry) {
		return ""
	}
	industryKey := strings.ToLower(industry)
	count := 0
	for _, peer := range peers {
		if peer.candidate.promotionTaxonomyExcluded || !taxonomyCapEligible(peer.candidate.TaxonomyStatus) ||
			isUnknownIndustry(peer.candidate.Industry) ||
			strings.ToLower(strings.TrimSpace(peer.candidate.Industry)) != industryKey {
			continue
		}
		count++
		if count >= maxPerIndustry {
			return normalizePromotionKey(peer.candidate.Ticker)
		}
	}
	return ""
}

func activePromotionPeers(
	state []scoredCandidate, active []bool, exclude int,
) []scoredCandidate {
	peers := make([]scoredCandidate, 0, len(state)-1)
	for index, candidate := range state {
		if index != exclude && index < len(active) && active[index] {
			peers = append(peers, candidate)
		}
	}
	return peers
}

func taxonomyCapEligible(status string) bool {
	status = strings.TrimSpace(status)
	return strings.EqualFold(status, "known")
}

func normalizePromotionKey(key string) string {
	return strings.ToUpper(strings.TrimSpace(key))
}

func isUnknownIndustry(industry string) bool {
	switch strings.ToLower(strings.TrimSpace(industry)) {
	case "", "unknown", "unavailable", "n/a", "na":
		return true
	default:
		return false
	}
}

func promotionScore(score float64) float64 {
	if math.IsNaN(score) {
		return math.Inf(-1)
	}
	return score
}

func promotionCorrelationConflict(
	candidate PromotionCandidate, selected []PromotionCandidate, threshold float64,
) (string, float64) {
	for _, prior := range selected {
		correlation, ok := candidate.Correlations[prior.Key]
		if !ok {
			correlation, ok = prior.Correlations[candidate.Key]
		}
		if !ok {
			correlation, ok = normalizedCorrelation(candidate.Correlations, prior.Key)
		}
		if !ok {
			correlation, ok = normalizedCorrelation(prior.Correlations, candidate.Key)
		}
		if ok && math.Abs(correlation) >= threshold {
			return prior.Key, correlation
		}
	}
	return "", 0
}

func normalizedCorrelation(values map[string]float64, target string) (float64, bool) {
	keys := make([]string, 0, len(values))
	for key := range values {
		if normalizePromotionKey(key) == target {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return 0, false
	}
	return values[keys[0]], true
}
