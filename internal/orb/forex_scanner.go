package orb

import (
	"context"
	"fmt"
	"strings"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

// forexAssetClass classifies a forex-path instrument for display/labelling.
// Spot precious metals (XAU/XAG/XPT/XPD vs a currency) trade on the forex
// scheduler but are metals, not FX pairs — the UI labels them distinctly.
func forexAssetClass(symbol string) string {
	base, _, _ := strings.Cut(symbol, "/")
	switch strings.ToUpper(base) {
	case "XAU", "XAG", "XPT", "XPD":
		return "metal"
	default:
		return "forex"
	}
}

// ForexCandidateScanner implements CandidateScanner for forex/CFD instruments via IBKR.
// It returns each configured instrument as a candidate with a default "trade" action
// so the ORB scheduler proceeds to range-watch and execution phases.
type ForexCandidateScanner struct {
	Instruments []config.Instrument
	MarketData  data.MarketDataProvider
	Cfg         config.ForexORBConfig
}

// ScanCandidates returns all configured instruments as ORB candidates.
// Unlike the equities scanner, forex/CFD instruments don't require a pre-market gap screen —
// we always trade the configured instruments and let the ORB range-watch filter entries.
func (s *ForexCandidateScanner) ScanCandidates(ctx context.Context) ([]ScoredCandidate, error) {
	if len(s.Instruments) == 0 {
		return nil, fmt.Errorf("forex scanner: no instruments configured")
	}

	candidates := make([]ScoredCandidate, 0, len(s.Instruments))
	for _, inst := range s.Instruments {
		candidates = append(candidates, ScoredCandidate{
			AssetClass: forexAssetClass(inst.Symbol),
			Gap: data.GapCandidate{
				Ticker:        inst.Symbol,
				GapPct:        0, // forex/CFD ORB does not rely on gap percentage
				PreMarketVol:  0,
				PreMarketLast: 0,
				PriorClose:    0,
			},
			Score: ORBAnalysisResult{
				Action:          "trade",
				Conviction:      1.0,
				CatalystType:    "session_open",
				CatalystQuality: "substantive",
				Reasoning:       "Forex ORB: configured instrument — always enters range-watch at session open.",
			},
		})
	}
	return candidates, nil
}

// Ensure ForexCandidateScanner satisfies the CandidateScanner interface at compile time.
var _ CandidateScanner = (*ForexCandidateScanner)(nil)
