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
type PromotionCandidate struct {
	// Index identifies the candidate in the caller's original slice. It keeps
	// audit attribution correct when normalized keys collide.
	Index          int
	Key            string
	Industry       string
	TaxonomyStatus string
	Score          float64
	Correlations   map[string]float64
}

// PromotionSelection records both accepted candidates and deterministic
// rejections so callers can persist an auditable reason for every cap.
type PromotionSelection struct {
	Selected []PromotionCandidate
	Rejected []PromotionRejection
}

type PromotionRejection struct {
	Candidate   PromotionCandidate
	Reason      string
	ConflictKey string
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
		if industry != "" && taxonomyCapEligible(candidate.TaxonomyStatus) &&
			maxPerIndustry > 0 && byIndustry[industryKey] >= maxPerIndustry {
			result.Rejected = append(result.Rejected, PromotionRejection{
				Candidate: candidate, Reason: "industry_cap",
			})
			continue
		}
		if maxAbsCorrelation > 0 {
			if conflict := promotionCorrelationConflict(candidate, result.Selected, maxAbsCorrelation); conflict != "" {
				result.Rejected = append(result.Rejected, PromotionRejection{
					Candidate: candidate, Reason: "correlation_cap", ConflictKey: conflict,
				})
				continue
			}
		}
		result.Selected = append(result.Selected, candidate)
		if industry != "" && taxonomyCapEligible(candidate.TaxonomyStatus) {
			byIndustry[industryKey]++
		}
	}
	return result
}

func selectScoredForPromotion(
	scored []scoredCandidate, slots, maxPerIndustry int,
) ([]scoredCandidate, []PromotionRejection) {
	candidates := make([]PromotionCandidate, 0, len(scored))
	for index, scoredCandidate := range scored {
		candidates = append(candidates, PromotionCandidate{
			Index: index, Key: scoredCandidate.candidate.Ticker,
			Industry:       scoredCandidate.candidate.Industry,
			TaxonomyStatus: scoredCandidate.candidate.TaxonomyStatus,
			Score:          scoredCandidate.score.Conviction,
		})
	}
	selection := SelectPromotionCandidates(candidates, slots, maxPerIndustry, 0)
	selected := make(map[int]struct{}, len(selection.Selected))
	for _, candidate := range selection.Selected {
		selected[candidate.Index] = struct{}{}
	}
	filtered := make([]scoredCandidate, 0, len(selection.Selected))
	for index, candidate := range scored {
		if _, ok := selected[index]; ok {
			filtered = append(filtered, candidate)
		}
	}
	return filtered, selection.Rejected
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
) string {
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
			return prior.Key
		}
	}
	return ""
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
