package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/knowledge"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// sympathyTrigger is a fresh, sentiment-strong article on a watched seed ticker
// that qualifies to fan out to the seed's graph neighbours.
type sympathyTrigger struct {
	Seed        string
	Headline    string
	Sentiment   float64
	AgeHours    float64
	PublishedAt time.Time
}

// newsSympathyCandidates sources candidates from live news events: when a fresh,
// sentiment-strong article lands on a manually-watched ticker A, A's eligible
// graph neighbours become candidates carrying the triggering headline as their
// thesis. This is the event-driven complement to graphNeighborCandidates, which
// walks the same edges on a timer with no notion of what just happened.
//
// News is read from the store (the engine fetches and persists articles for
// every watchlist ticker each tick), so no extra provider calls are made for
// seeds. Edge eligibility reuses the GraphNeighbors thresholds — one definition
// of a trustworthy edge. A per-(seed, neighbour) cooldown stops a running story
// from re-surfacing the same sympathy idea every scan. Errors are non-fatal:
// sourcing is best-effort and never blocks a scan.
func (s *MarketScanner) newsSympathyCandidates(ctx context.Context, existing []SwingCandidate) []SwingCandidate {
	nc := s.Cfg.NewsSympathy
	if !nc.Enabled {
		return nil
	}

	lookback := time.Duration(nc.LookbackHours) * time.Hour
	if lookback <= 0 {
		lookback = 6 * time.Hour
	}
	minAbsSentiment := nc.MinAbsSentiment
	if minAbsSentiment <= 0 || minAbsSentiment > 1 {
		minAbsSentiment = 0.35
	}
	maxPerScan := nc.MaxPerScan
	if maxPerScan <= 0 {
		maxPerScan = 5
	}
	cooldown := time.Duration(nc.PairCooldownHours) * time.Hour
	if cooldown <= 0 {
		cooldown = 24 * time.Hour
	}

	// Edge eligibility mirrors graphNeighborCandidates exactly.
	gc := s.Cfg.GraphNeighbors
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

	items, err := store.GetWatchlistItems(ctx, true)
	if err != nil {
		slog.Warn("scanner: sympathy sourcing — read watchlist failed", "err", err)
		return nil
	}

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

	now := s.now()
	s.pruneSympathyCooldowns(now, cooldown)

	// One trigger per seed: the freshest article with the strongest sentiment.
	triggers := make(map[string]sympathyTrigger)
	for _, seed := range seeds {
		if ctx.Err() != nil {
			return nil
		}
		news, err := store.GetRecentNews(ctx, seed, now.Add(-lookback))
		if err != nil {
			slog.Warn("scanner: sympathy sourcing — read news failed", "seed", seed, "err", err)
			continue
		}
		if trig, ok := pickSympathyTrigger(seed, news, now, minAbsSentiment); ok {
			triggers[seed] = trig
		}
	}
	if len(triggers) == 0 {
		return nil
	}

	// Fan out each triggered seed to its eligible neighbours; keep the best
	// (highest |sentiment| × edge strength) pairing per distinct neighbour.
	type pairing struct {
		neighbor store.NeighborCandidate
		trigger  sympathyTrigger
		score    float64
	}
	best := make(map[string]pairing)
	for seed, trig := range triggers {
		if ctx.Err() != nil {
			return nil
		}
		neighbors, err := knowledge.EligibleNeighbors(ctx, seed, minConf, minCorr, minStrength)
		if err != nil {
			slog.Warn("scanner: sympathy sourcing — eligible neighbors failed", "seed", seed, "err", err)
			continue
		}
		for _, n := range neighbors {
			key := strings.ToUpper(n.Ticker)
			if _, seen := covered[key]; seen {
				continue
			}
			if s.sympathyOnCooldown(seed, n.Ticker) {
				continue
			}
			score := math.Abs(trig.Sentiment) * n.Strength
			if cur, ok := best[key]; !ok || score > cur.score {
				best[key] = pairing{neighbor: n, trigger: trig, score: score}
			}
		}
	}

	slog.Info("scanner: sympathy sourcing",
		"triggered_seeds", len(triggers),
		"distinct_neighbors", len(best),
	)
	if len(best) == 0 {
		return nil
	}

	ranked := make([]pairing, 0, len(best))
	for _, p := range best {
		ranked = append(ranked, p)
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })

	if s.AlpacaClient == nil {
		slog.Warn("scanner: sympathy sourcing — no Alpaca client, cannot price neighbors")
		return nil
	}

	// Cap applies to PRICED candidates (same rationale as graph sourcing).
	var out []SwingCandidate
	admissions := 0
	for _, p := range ranked {
		if ctx.Err() != nil {
			break
		}
		if admissions >= maxPerScan && !refreshable[strings.ToUpper(p.neighbor.Ticker)] {
			continue
		}
		q, err := s.AlpacaClient.FetchQuote(ctx, p.neighbor.Ticker)
		if err != nil || q == nil || q.Last <= 0 {
			slog.Warn("scanner: sympathy sourcing — skip unpriceable neighbor",
				"ticker", p.neighbor.Ticker, "err", err)
			continue
		}
		out = append(out, SwingCandidate{
			Ticker:       p.neighbor.Ticker,
			Price:        q.Last,
			Sources:      []string{"news_sympathy"},
			GraphContext: sympathyPhrase(p.trigger, p.neighbor),
			SympathySeed: p.trigger.Seed, SympathyRelationship: graphLinkPhrase(p.neighbor),
			News: []SwingNewsItem{{Headline: p.trigger.Headline, Sentiment: fmt.Sprintf("%.6f", p.trigger.Sentiment), PublishedAt: p.trigger.PublishedAt}},
		})
		if !refreshable[strings.ToUpper(p.neighbor.Ticker)] {
			admissions++
		}
		s.markSympathyFired(p.trigger.Seed, p.neighbor.Ticker, now)
	}
	return out
}

// pickSympathyTrigger selects the strongest qualifying article for a seed —
// highest |sentiment| meeting the floor, freshest on ties. Returns false when
// nothing qualifies.
func pickSympathyTrigger(seed string, news []data.NewsItem, now time.Time, minAbsSentiment float64) (sympathyTrigger, bool) {
	var bestItem *data.NewsItem
	for i := range news {
		n := &news[i]
		if math.Abs(n.SentimentScore) < minAbsSentiment {
			continue
		}
		if bestItem == nil ||
			math.Abs(n.SentimentScore) > math.Abs(bestItem.SentimentScore) ||
			(math.Abs(n.SentimentScore) == math.Abs(bestItem.SentimentScore) && n.PublishedAt.After(bestItem.PublishedAt)) {
			bestItem = n
		}
	}
	if bestItem == nil {
		return sympathyTrigger{}, false
	}
	return sympathyTrigger{
		Seed:        seed,
		Headline:    bestItem.Headline,
		Sentiment:   bestItem.SentimentScore,
		AgeHours:    now.Sub(bestItem.PublishedAt).Hours(),
		PublishedAt: bestItem.PublishedAt,
	}, true
}

// sympathyPhrase renders the candidate's origin thesis for the scoring LLM and
// (persisted via the watchlist item) the downstream trading analysts.
func sympathyPhrase(t sympathyTrigger, n store.NeighborCandidate) string {
	label := "NEUTRAL"
	if t.Sentiment > 0 {
		label = "POSITIVE"
	} else if t.Sentiment < 0 {
		label = "NEGATIVE"
	}
	headline := t.Headline
	if len(headline) > 160 {
		headline = headline[:160] + "…"
	}
	return fmt.Sprintf("SYMPATHY TRIGGER — %s news %.0fh ago (%s %.2f): %q. Relationship: %s.",
		t.Seed, t.AgeHours, label, t.Sentiment, headline, graphLinkPhrase(n))
}

// ─── Pair cooldown ─────────────────────────────────────────────────────────────
//
// In-memory is sufficient: the scan loop is single-threaded, and a scanner
// restart forgetting cooldowns at worst re-surfaces an idea one extra time —
// where it still has to clear the LLM conviction gate.

func sympathyPairKey(seed, neighbor string) string {
	return strings.ToUpper(seed) + "|" + strings.ToUpper(neighbor)
}

func (s *MarketScanner) sympathyOnCooldown(seed, neighbor string) bool {
	_, hot := s.sympathyFired[sympathyPairKey(seed, neighbor)]
	return hot
}

func (s *MarketScanner) markSympathyFired(seed, neighbor string, at time.Time) {
	if s.sympathyFired == nil {
		s.sympathyFired = make(map[string]time.Time)
	}
	s.sympathyFired[sympathyPairKey(seed, neighbor)] = at
}

// pruneSympathyCooldowns drops pairs whose cooldown has elapsed.
func (s *MarketScanner) pruneSympathyCooldowns(now time.Time, cooldown time.Duration) {
	for k, at := range s.sympathyFired {
		if now.Sub(at) >= cooldown {
			delete(s.sympathyFired, k)
		}
	}
}
