package scanner

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

// scannerProfileDefaults are deliberately source-owned. The scoring model may
// prefer one of these lenses, but cannot expand the set or make a discovery
// label into a strategy.
type scannerProfileDefaults struct {
	lenses      []string
	primary     string
	entryTiming string
	rationale   string
}

func buildScannerTradeProfile(
	candidate SwingCandidate, score SwingScoreResult,
) (*tradeprofile.DiscoveryContext, *tradeprofile.ExecutionProfile, error) {
	sources, err := tradeprofile.CanonicalizeSources(candidate.Sources)
	if err != nil {
		return nil, nil, fmt.Errorf("canonicalize scanner sources: %w", err)
	}
	if len(sources) == 0 {
		return nil, nil, fmt.Errorf("scanner candidate has no discovery sources")
	}

	discovery := &tradeprofile.DiscoveryContext{
		Version:          tradeprofile.DiscoveryContextVersion,
		Sources:          sources,
		Thesis:           scannerDiscoveryThesis(candidate, sources),
		EvidenceBySource: scannerEvidenceBySource(candidate, sources),
	}
	canonicalDiscovery, err := discovery.Canonicalize()
	if err != nil {
		return nil, nil, fmt.Errorf("canonicalize discovery context: %w", err)
	}

	defaults := scannerDefaultsForSources(sources)
	lenses := append([]string(nil), defaults.lenses...)
	recommended := lenses[0]
	if scoreLens := strings.ToLower(strings.TrimSpace(score.Strategy)); containsLens(lenses, scoreLens) {
		recommended = scoreLens
		lenses = preferLens(lenses, scoreLens)
	}
	profile := &tradeprofile.ExecutionProfile{
		Version:              tradeprofile.ExecutionProfileVersion,
		RecommendedLens:      recommended,
		AdmissibleLenses:     lenses,
		PrimaryTimeframe:     defaults.primary,
		EntryTimingTimeframe: defaults.entryTiming,
		ScorerRationale:      scannerProfileRationale(defaults.rationale, score),
	}
	canonicalProfile, err := profile.Canonicalize()
	if err != nil {
		return nil, nil, fmt.Errorf("canonicalize execution profile: %w", err)
	}
	return &canonicalDiscovery, &canonicalProfile, nil
}

func scannerDefaultsForSources(sources []string) scannerProfileDefaults {
	// Source priority chooses the appropriate workflow when a ticker is found
	// through several routes. It is intentionally independent from source order,
	// which reflects discovery/merge timing rather than execution semantics.
	for _, source := range []struct {
		name     string
		defaults scannerProfileDefaults
	}{
		{
			name: "weekly_leader",
			defaults: scannerProfileDefaults{
				lenses: []string{"trend_following", "momentum", "pullback_strength"}, primary: "1D", entryTiming: "4H",
				rationale: "Weekly leadership is a daily trend/relative-strength thesis with a qualified pullback alternative.",
			},
		},
		{
			name: "potent_industry",
			defaults: scannerProfileDefaults{
				lenses: []string{"trend_following", "momentum"}, primary: "1D", entryTiming: "4H",
				rationale: "Potent-industry leadership is a daily trend and momentum thesis.",
			},
		},
		{
			name: "premarket_episodic",
			defaults: scannerProfileDefaults{
				lenses: []string{"momentum", "news_sentiment"}, primary: "4H", entryTiming: "1H",
				rationale: "A premarket catalyst requires intraday momentum or news follow-through and 1H timing.",
			},
		},
		{
			name: "news_sympathy",
			defaults: scannerProfileDefaults{
				lenses: []string{"news_sentiment", "momentum"}, primary: "4H",
				rationale: "News-sympathy provenance needs event spillover and momentum confirmation on 4H.",
			},
		},
		{
			name: "graph_neighbor",
			defaults: scannerProfileDefaults{
				lenses: []string{"news_sentiment", "momentum"}, primary: "4H",
				rationale: "A graph relationship is a thesis to confirm with news or momentum on 4H.",
			},
		},
	} {
		if containsSource(sources, source.name) {
			return source.defaults
		}
	}
	return scannerProfileDefaults{
		lenses: []string{"momentum", "news_sentiment"}, primary: "4H",
		rationale: "Ordinary mover/activity discovery requires 4H momentum or news confirmation.",
	}
}

func scannerEvidenceBySource(
	candidate SwingCandidate, sources []string,
) map[string]json.RawMessage {
	evidenceBySource := make(map[string]json.RawMessage, len(sources))
	for index, source := range sources {
		evidence := map[string]any{}
		if score, ok := candidate.SourceScores[source]; ok {
			evidence["source_score"] = score
		}
		if index == 0 {
			evidence["market_observation"] = scannerMarketObservation(candidate)
			if len(candidate.News) > 0 {
				evidence["recent_news"] = candidate.News
			}
		}
		switch source {
		case "graph_neighbor", "news_sympathy":
			if candidate.GraphContext != "" {
				evidence["relationship_thesis"] = candidate.GraphContext
			}
		case "weekly_leader":
			if candidate.WeeklyLeaderEvidence != "" {
				evidence["leader_evidence"] = candidate.WeeklyLeaderEvidence
			}
			if candidate.WeeklyLeaderRunID != "" {
				evidence["source_run_id"] = candidate.WeeklyLeaderRunID
			}
		case "potent_industry":
			if candidate.PotentIndustryEvidence != "" {
				evidence["industry_evidence"] = candidate.PotentIndustryEvidence
			}
			if candidate.PotentIndustryRunID != "" {
				evidence["source_run_id"] = candidate.PotentIndustryRunID
			}
			if candidate.PotentIndustryGroupType != "" || candidate.PotentIndustryGroupKey != "" {
				evidence["group"] = map[string]string{
					"type": candidate.PotentIndustryGroupType,
					"key":  candidate.PotentIndustryGroupKey,
				}
			}
		case "premarket_episodic":
			if candidate.PremarketEpisodicEvidence != "" {
				evidence["episodic_evidence"] = candidate.PremarketEpisodicEvidence
			}
			if candidate.PremarketEpisodicRunID != "" {
				evidence["source_run_id"] = candidate.PremarketEpisodicRunID
			}
		case "political_disclosure":
			if politicalDisclosureEvidenceObject(candidate.PoliticalDisclosureFacts) {
				// Preserve the common market/news observations for the first
				// source and attach the structured disclosure facts alongside
				// them. Replacing the whole object would drop the exact
				// point-in-time evidence the scanner and later audits need.
				var facts map[string]any
				if err := json.Unmarshal(candidate.PoliticalDisclosureFacts, &facts); err == nil {
					for key, value := range facts {
						evidence[key] = value
					}
				}
			}
			if candidate.PoliticalDisclosureEvidence != "" {
				evidence["summary"] = candidate.PoliticalDisclosureEvidence
				evidence["data_quality"] = map[string]any{
					"limitations": []string{"structured disclosure evidence was unavailable"},
				}
			}
		}
		encoded, err := json.Marshal(evidence)
		if err != nil {
			// All values above are scanner-owned primitives. Retain a valid
			// evidence object if a future field ever becomes non-serializable.
			encoded = json.RawMessage(`{}`)
		}
		evidenceBySource[source] = encoded
	}
	return evidenceBySource
}

func scannerMarketObservation(candidate SwingCandidate) map[string]any {
	return map[string]any{
		"ticker":             candidate.Ticker,
		"price":              candidate.Price,
		"change_pct":         candidate.ChangePct,
		"volume":             candidate.Volume,
		"trade_count":        candidate.TradeCount,
		"dollar_volume":      candidate.DollarVolume,
		"market_cap_mm":      candidate.MarketCapMM,
		"return_1m_pct":      candidate.Return1M,
		"return_3m_pct":      candidate.Return3M,
		"return_6m_pct":      candidate.Return6M,
		"pullback_qualified": candidate.PullbackQualified,
		"sector":             candidate.Sector,
		"industry":           candidate.Industry,
	}
}

func scannerDiscoveryThesis(candidate SwingCandidate, sources []string) string {
	for _, source := range sources {
		var evidence string
		switch source {
		case "weekly_leader":
			evidence = candidate.WeeklyLeaderEvidence
		case "potent_industry":
			evidence = candidate.PotentIndustryEvidence
		case "premarket_episodic":
			evidence = candidate.PremarketEpisodicEvidence
		case "political_disclosure":
			evidence = politicalDisclosureThesis(candidate)
		case "graph_neighbor", "news_sympathy":
			evidence = candidate.GraphContext
		}
		if strings.TrimSpace(evidence) != "" {
			return compactScannerText(fmt.Sprintf("%s: %s", source, evidence), 280)
		}
	}
	if len(candidate.News) > 0 && strings.TrimSpace(candidate.News[0].Headline) != "" {
		return compactScannerText(fmt.Sprintf("%s surfaced by %s with recent news: %s",
			candidate.Ticker, strings.Join(sources, ", "), candidate.News[0].Headline), 280)
	}
	return compactScannerText(fmt.Sprintf("%s surfaced by %s", candidate.Ticker, strings.Join(sources, ", ")), 280)
}

func politicalDisclosureEvidenceObject(raw json.RawMessage) bool {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	return len(raw) > 1 && raw[0] == '{' && json.Valid(raw)
}

func politicalDisclosureThesis(candidate SwingCandidate) string {
	var facts tradeprofile.PoliticalDisclosureEvidence
	if err := json.Unmarshal(candidate.PoliticalDisclosureFacts, &facts); err == nil &&
		facts.Version == tradeprofile.PoliticalDisclosureEvidenceVersion &&
		facts.ReportCount > 0 {
		return summarizePoliticalDisclosure(facts)
	}
	return candidate.PoliticalDisclosureEvidence
}

func scannerProfileRationale(defaultRationale string, score SwingScoreResult) string {
	rationale := defaultRationale
	if setup := strings.TrimSpace(score.SetupType); setup != "" {
		rationale += " Scorer setup: " + setup + "."
	}
	if reasoning := strings.TrimSpace(score.Reasoning); reasoning != "" {
		rationale += " Scorer: " + reasoning
	}
	return compactScannerText(rationale, 400)
}

func compactScannerText(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}
	if limit <= len("…") {
		return "…"
	}

	// limit is a byte limit because these values are stored in bounded text
	// fields. Trim at a rune boundary after reserving room for the ellipsis so
	// the result remains valid UTF-8 and never exceeds the requested limit.
	end := limit - len("…")
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return strings.TrimSpace(value[:end]) + "…"
}

func containsSource(sources []string, target string) bool {
	for _, source := range sources {
		if source == target {
			return true
		}
	}
	return false
}

func containsLens(lenses []string, target string) bool {
	for _, lens := range lenses {
		if lens == target {
			return true
		}
	}
	return false
}

func preferLens(lenses []string, target string) []string {
	ordered := make([]string, 0, len(lenses))
	ordered = append(ordered, target)
	for _, lens := range lenses {
		if lens != target {
			ordered = append(ordered, lens)
		}
	}
	return ordered
}
