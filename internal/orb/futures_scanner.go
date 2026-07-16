package orb

import (
	"context"
	"fmt"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

// FuturesCandidateScanner implements CandidateScanner for exchange-listed futures
// via IBKR. Unlike the equities scanner, futures instruments don't require a
// pre-market gap screen — the configured instruments are always entered at the
// session open and the ORB range-watch phase does the actual filtering.
//
// This scanner is asset-class agnostic: index futures (ES, NQ), metals (GC, SI),
// and energy contracts (CL, NG) are all expressed as FuturesInstrument entries
// in config and handled identically here. Asset-class-specific behaviour (session
// open time, tick sizing, ORB prompt flavour) lives in config and executor.go.
type FuturesCandidateScanner struct {
	Instruments []config.FuturesInstrument
	MarketData  data.MarketDataProvider // reserved for future pre-scan enrichment
	Cfg         config.FuturesORBConfig
}

// ScanCandidates returns all configured instruments as ORB candidates.
// Every instrument gets Action="trade" and Conviction=1.0 — range-watch and
// bracket sizing determine whether a trade is actually placed.
func (s *FuturesCandidateScanner) ScanCandidates(ctx context.Context) ([]ScoredCandidate, error) {
	if len(s.Instruments) == 0 {
		return nil, fmt.Errorf("futures scanner: no instruments configured")
	}

	candidates := make([]ScoredCandidate, 0, len(s.Instruments))
	for _, inst := range s.Instruments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		candidates = append(candidates, ScoredCandidate{
			AssetClass: "futures",
			Gap: data.GapCandidate{
				Ticker:        inst.Symbol,
				GapPct:        0, // futures ORB does not rely on a gap threshold
				PreMarketVol:  0,
				PreMarketLast: 0,
				PriorClose:    0,
			},
			Score: ORBAnalysisResult{
				Action:          "trade",
				Conviction:      1.0,
				CatalystType:    "session_open",
				CatalystQuality: "substantive",
				Reasoning: fmt.Sprintf(
					"Futures ORB [%s/%s]: %s — always enters range-watch at session open.",
					inst.AssetClass, inst.Exchange, inst.Symbol,
				),
			},
		})
	}
	return candidates, nil
}

// Ensure FuturesCandidateScanner satisfies the CandidateScanner interface at compile time.
var _ CandidateScanner = (*FuturesCandidateScanner)(nil)
