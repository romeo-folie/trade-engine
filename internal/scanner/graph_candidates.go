package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/romeo-folie/trade-engine/internal/knowledge"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// graphNeighborCandidates sources additional scanner candidates from the knowledge
// graph. It traverses out from manually-watched tickers (source != "scanner") to
// their trusted graph neighbours — tradeable companies linked by supply chain,
// competition, etc. — and returns them as SwingCandidates to be scored through the
// same LLM + conviction gate as screener candidates.
//
// Seeding only from non-scanner watchlist items serves two purposes: it scopes
// discovery to the user's high-conviction (deliberate) picks, and it prevents
// recursive fan-out — a scanner-added neighbour never spawns its own neighbours.
//
// existing is the screener candidate pool already gathered this scan; neighbours
// duplicating those (or already on the watchlist) are dropped. Errors are non-fatal:
// graph sourcing is best-effort and never blocks a scan.
func (s *MarketScanner) graphNeighborCandidates(ctx context.Context, existing []SwingCandidate) []SwingCandidate {
	gc := s.Cfg.GraphNeighbors
	if !gc.Enabled {
		return nil
	}

	minConf := gc.MinConfidence
	if minConf <= 0 || minConf > 1 {
		minConf = 0.80
	}
	minCorr := gc.MinCorroboration
	if minCorr <= 0 {
		minCorr = 2
	}
	minStrength := gc.MinEdgeStrength
	if minStrength <= 0 || minStrength > 1 {
		minStrength = 0.6
	}
	maxPerScan := gc.MaxPerScan
	if maxPerScan <= 0 {
		maxPerScan = 5
	}

	items, err := store.GetWatchlistItems(ctx, true)
	if err != nil {
		slog.Warn("scanner: graph sourcing — read watchlist failed", "err", err)
		return nil
	}

	// Tickers already covered: every active watchlist ticker and every screener
	// candidate already in the pool. Neighbours matching these are skipped. Step 3
	// (filterExistingWatchlist) re-checks the watchlist for the whole pool, but
	// excluding here too avoids fetching quotes for neighbours we'd only drop later.
	// Safe in the single-threaded scan loop: no watchlist writes occur between this
	// read and step 3.
	covered := make(map[string]struct{}, len(items)+len(existing))
	var seeds []string
	for _, it := range items {
		covered[strings.ToUpper(it.Ticker)] = struct{}{}
		if it.Source != "scanner" {
			seeds = append(seeds, it.Ticker)
		}
	}
	for _, c := range existing {
		covered[strings.ToUpper(c.Ticker)] = struct{}{}
	}

	if len(seeds) == 0 {
		return nil
	}

	// Collect the best (highest-strength) edge per distinct neighbour ticker.
	best := make(map[string]store.NeighborCandidate)
	var eligibleEdges int
	for _, seed := range seeds {
		if ctx.Err() != nil {
			break
		}
		neighbors, err := knowledge.EligibleNeighbors(ctx, seed, minConf, minCorr, minStrength)
		if err != nil {
			slog.Warn("scanner: graph sourcing — eligible neighbors failed", "seed", seed, "err", err)
			continue
		}
		for _, n := range neighbors {
			eligibleEdges++
			key := strings.ToUpper(n.Ticker)
			if _, seen := covered[key]; seen {
				continue
			}
			if cur, ok := best[key]; !ok || n.Strength > cur.Strength {
				best[key] = n
			}
		}
	}

	slog.Info("scanner: graph sourcing",
		"seed_names", len(seeds),
		"eligible_edges", eligibleEdges,
		"distinct_neighbors", len(best),
	)

	if len(best) == 0 {
		return nil
	}

	// Rank distinct neighbours by edge strength, take the strongest maxPerScan.
	ranked := make([]store.NeighborCandidate, 0, len(best))
	for _, n := range best {
		ranked = append(ranked, n)
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].Strength > ranked[j].Strength })
	if len(ranked) > maxPerScan {
		ranked = ranked[:maxPerScan]
	}

	// Build candidates. Graph neighbours carry no screener price/volume, so fetch a
	// quote for a credible price; drop any we can't price (the scorer would otherwise
	// see price 0 and reject on bad data rather than on merit). Without an Alpaca
	// client we can't price anything, so graph sourcing yields nothing.
	if s.AlpacaClient == nil {
		slog.Warn("scanner: graph sourcing — no Alpaca client, cannot price neighbors")
		return nil
	}
	var out []SwingCandidate
	for _, n := range ranked {
		if ctx.Err() != nil {
			break
		}
		q, err := s.AlpacaClient.FetchQuote(ctx, n.Ticker)
		if err != nil || q == nil || q.Last <= 0 {
			slog.Warn("scanner: graph sourcing — skip unpriceable neighbor",
				"ticker", n.Ticker, "err", err)
			continue
		}
		out = append(out, SwingCandidate{
			Ticker:       n.Ticker,
			Price:        q.Last,
			Sources:      []string{"graph_neighbor"},
			GraphContext: graphLinkPhrase(n),
		})
	}
	return out
}

// graphLinkPhrase renders a human-readable explanation of why a neighbour was
// surfaced, oriented by edge direction relative to the watched seed ticker.
func graphLinkPhrase(n store.NeighborCandidate) string {
	rel := strings.ReplaceAll(n.RelationshipType, "_", " ")
	if n.Direction == "inbound" {
		// edge points neighbour → seed
		return fmt.Sprintf("this company %s %s (on watchlist)", rel, n.SeedTicker)
	}
	// outbound: edge points seed → neighbour
	return fmt.Sprintf("%s (on watchlist) %s this company", n.SeedTicker, rel)
}
