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
	refreshable := make(map[string]bool, len(items))
	var seeds []string
	for _, it := range items {
		refreshable[strings.ToUpper(it.Ticker)] = it.Source == "scanner"
		if !it.ScannerOrigin {
			covered[strings.ToUpper(it.Ticker)] = struct{}{}
		}
		if it.Source != "scanner" && it.Source != "earnings_watchlist" {
			seeds = append(seeds, it.Ticker)
		}
	}
	for _, c := range existing {
		if !refreshable[strings.ToUpper(c.Ticker)] {
			covered[strings.ToUpper(c.Ticker)] = struct{}{}
		}
	}

	if len(seeds) == 0 {
		return nil
	}

	// Collect the best edge per distinct neighbour ticker. Equal-strength graph
	// edges are common (for example, reciprocal partner and supply-chain
	// relationships). Choose them by a total, stable ordering so a different
	// database row order cannot change the relationship prose, renew the source
	// lease, and emit a spurious rediscovery notification.
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
			if cur, ok := best[key]; !ok || preferGraphNeighbor(n, cur) {
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

	// Rank distinct neighbours by edge strength, strongest first. Preserve the
	// historical ticker tie-break here: the canonical edge tie-break above is for
	// selecting the relationship prose for one ticker, not changing which
	// equal-strength tickers fit within MaxPerScan.
	ranked := make([]store.NeighborCandidate, 0, len(best))
	for _, n := range best {
		ranked = append(ranked, n)
	}
	rankGraphNeighbors(ranked)

	// Build candidates. Graph neighbours carry no screener price/volume, so fetch a
	// quote for a credible price; drop any we can't price (the scorer would otherwise
	// see price 0 and reject on bad data rather than on merit). Without an Alpaca
	// client we can't price anything, so graph sourcing yields nothing.
	if s.AlpacaClient == nil {
		slog.Warn("scanner: graph sourcing — no Alpaca client, cannot price neighbors")
		return nil
	}
	// Apply maxPerScan to PRICED candidates, not the raw ranking: a handful of
	// unpriceable high-strength tickers (stale/non-US/mistyped) must not consume the
	// budget and starve valid lower-ranked neighbours. Keep pricing down the ranking
	// until the budget fills.
	var out []SwingCandidate
	admissions := 0
	minPrice := s.Cfg.Universe.MinPrice
	if minPrice <= 0 {
		minPrice = 5
	}
	for _, n := range ranked {
		if ctx.Err() != nil {
			break
		}
		if admissions >= maxPerScan && !refreshable[strings.ToUpper(n.Ticker)] {
			continue
		}
		q, err := s.AlpacaClient.FetchQuote(ctx, n.Ticker)
		if err != nil || q == nil || q.Last < minPrice {
			slog.Warn("scanner: graph sourcing — skip unpriceable neighbor",
				"ticker", n.Ticker, "err", err)
			continue
		}
		out = append(out, SwingCandidate{
			Ticker:       n.Ticker,
			Price:        q.Last,
			Sources:      []string{"graph_neighbor"},
			SourceScores: map[string]float64{"graph_neighbor": n.Strength},
			GraphContext: graphLinkPhrase(n),
		})
		if !refreshable[strings.ToUpper(n.Ticker)] {
			admissions++
		}
	}
	return out
}

// preferGraphNeighbor reports whether candidate is the canonical winner over
// current. Strength remains the sole ranking signal; the remaining fields only
// make equal-strength selection stable. They are exactly the fields that shape
// graphLinkPhrase, and therefore the persisted graph-neighbor lease evidence.
func preferGraphNeighbor(candidate, current store.NeighborCandidate) bool {
	if candidate.Strength != current.Strength {
		return candidate.Strength > current.Strength
	}
	return graphNeighborIdentity(candidate) < graphNeighborIdentity(current)
}

func graphNeighborIdentity(candidate store.NeighborCandidate) string {
	return strings.Join([]string{
		strings.ToUpper(strings.TrimSpace(candidate.SeedTicker)),
		strings.ToLower(strings.TrimSpace(candidate.RelationshipType)),
		strings.ToLower(strings.TrimSpace(candidate.Direction)),
		strings.ToUpper(strings.TrimSpace(candidate.Ticker)),
	}, "\x00")
}

func rankGraphNeighbors(candidates []store.NeighborCandidate) {
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Strength > candidates[j].Strength ||
			(candidates[i].Strength == candidates[j].Strength && candidates[i].Ticker < candidates[j].Ticker)
	})
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
