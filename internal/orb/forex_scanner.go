package orb

import (
	"context"
	"fmt"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

// ForexCandidateScanner implements CandidateScanner for forex pairs via IBKR.
// It returns each configured pair as a candidate with a default "trade" action
// so the ORB scheduler proceeds to range-watch and execution phases.
type ForexCandidateScanner struct {
	Pairs      []string
	MarketData data.MarketDataProvider
	Cfg        config.ForexORBConfig
}

// ScanCandidates returns all configured forex pairs as ORB candidates.
// Unlike the equities scanner, forex pairs don't require a pre-market gap screen —
// we always trade the configured pairs and let the ORB range-watch filter entries.
func (s *ForexCandidateScanner) ScanCandidates(ctx context.Context) ([]ScoredCandidate, error) {
	if len(s.Pairs) == 0 {
		return nil, fmt.Errorf("forex scanner: no pairs configured")
	}

	candidates := make([]ScoredCandidate, 0, len(s.Pairs))
	for _, pair := range s.Pairs {
		candidates = append(candidates, ScoredCandidate{
			Gap: data.GapCandidate{
				Ticker:        pair,
				GapPct:        0, // forex ORB does not rely on gap percentage
				PreMarketVol:  0,
				PreMarketLast: 0,
				PriorClose:    0,
			},
			Score: ORBAnalysisResult{
				Action:          "trade",
				Conviction:      1.0,
				CatalystType:    "session_open",
				CatalystQuality: "substantive",
				Reasoning:       "Forex ORB: configured pair — always enters range-watch at session open.",
			},
		})
	}
	return candidates, nil
}

// Ensure ForexCandidateScanner satisfies the CandidateScanner interface at compile time.
var _ CandidateScanner = (*ForexCandidateScanner)(nil)
