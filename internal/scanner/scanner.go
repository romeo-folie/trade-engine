// Package scanner implements the autonomous market scanner that discovers
// swing-trade candidates and adds them to the watchlist with a configurable TTL.
//
// The scanner runs on a configurable interval (default: every 2 hours) during
// market hours only. Each scan cycle:
//  1. Fetches top movers (gainers/losers) and most-active stocks from Alpaca.
//  2. Deduplicates candidates and enriches them with recent news.
//  3. Scores each candidate via LLM — evaluating swing-trade suitability,
//     setup type, strategy fit, and conviction.
//  4. Adds high-conviction candidates to the watchlist (source="scanner") with
//     an expiry timestamp.
//  5. Expires stale scanner-added items that have passed their TTL.
//  6. Sends a Telegram summary of what was added.
package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/depth"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// bus wraps events.Bus so we can nil-check before calling Publish.
type bus interface {
	Publish(evType events.Type, payload any)
}

const (
	sentimentPositiveThreshold = 0.3
	sentimentNegativeThreshold = -0.3
	maxNewsPerTicker           = 5
)

// Notifier is satisfied by notify.TelegramBot (and any test double).
type Notifier interface {
	SendImmediate(ctx context.Context, message string) error
}

// MarketScanner discovers swing-trade candidates and manages a time-bounded
// watchlist of scanner-sourced tickers.
type MarketScanner struct {
	AlpacaClient *data.AlpacaClient
	Analyzer     *analysis.Analyzer
	News         data.NewsProvider
	Notifier     Notifier // optional — nil disables Telegram notifications
	Bus          bus
	Cfg          config.ScannerConfig
	Now          func() time.Time // injectable for tests; defaults to time.Now

	// sympathyFired tracks (seed, neighbour) pairs already surfaced by
	// news-sympathy sourcing, pruned after the configured cooldown so one
	// running story doesn't re-surface the same idea every scan. Only touched
	// from the single-threaded scan loop.
	sympathyFired map[string]time.Time
}

// Run starts the scanner loop. It blocks until ctx is cancelled.
// The scanner only fires during US market hours (9:30am–4:00pm ET, Mon–Fri).
func (s *MarketScanner) Run(ctx context.Context) error {
	interval := time.Duration(s.Cfg.ScanIntervalHours) * time.Hour
	if interval <= 0 {
		interval = 2 * time.Hour
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run once immediately at startup if market is open.
	if s.isMarketOpen() {
		if err := s.runScan(ctx); err != nil && err != context.Canceled {
			slog.Error("scanner: initial scan error", "err", err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if !s.isMarketOpen() {
				continue
			}
			if err := s.runScan(ctx); err != nil && err != context.Canceled {
				slog.Error("scanner: scan error", "err", err)
			}
		}
	}
}

// runScan executes one full scan cycle.
func (s *MarketScanner) runScan(ctx context.Context) error {
	start := s.now()

	if s.Bus != nil {
		s.Bus.Publish(events.ScanStarted, events.ScanStartedPayload{})
	}

	slog.Info("scanner: scan started")

	// 1. Expire stale scanner items before adding new ones.
	expired, err := store.ExpireScannedItems(ctx)
	if err != nil {
		slog.Warn("scanner: expire stale items", "err", err)
	}

	// 2. Fetch candidates from both screeners.
	candidates, err := s.fetchCandidates(ctx)
	if err != nil {
		return fmt.Errorf("fetch candidates: %w", err)
	}
	slog.Info("scanner: candidates fetched", "count", len(candidates))

	// 2.5 Source news-sympathy candidates — graph neighbours of watched tickers
	// with fresh, sentiment-strong news, carrying the triggering headline as the
	// thesis. No-op unless news_sympathy is enabled.
	//
	// Sympathy MUST run before the static graph pass: both walk the same edges
	// and skip tickers already in the pool, so whichever runs first claims a
	// shared neighbour. Sympathy carries the richer thesis (trigger headline +
	// direction); if the static pass claimed the ticker first, a fresh-news
	// neighbour would be scored with only the weak "graph link" hint (Codex #393).
	if sympCands := s.newsSympathyCandidates(ctx, candidates); len(sympCands) > 0 {
		candidates = append(candidates, sympCands...)
		slog.Info("scanner: news-sympathy candidates added to pool", "count", len(sympCands))
	}

	// 2.6 Source extra candidates from the knowledge graph — tradeable companies
	// one hop from a manually-watched ticker. No-op unless graph sourcing is
	// enabled. Runs after sympathy so it only fills in neighbours with no fresh
	// news trigger.
	if graphCands := s.graphNeighborCandidates(ctx, candidates); len(graphCands) > 0 {
		candidates = append(candidates, graphCands...)
		slog.Info("scanner: graph neighbors added to pool", "count", len(graphCands))
	}

	if len(candidates) == 0 {
		return nil
	}

	// 3. Dedup against existing active watchlist to avoid re-scoring known tickers.
	candidates, err = s.filterExistingWatchlist(ctx, candidates)
	if err != nil {
		return fmt.Errorf("filter watchlist: %w", err)
	}
	if len(candidates) == 0 {
		return nil
	}

	// 4. Check how much headroom remains under the cap.
	activeCount, err := store.CountActiveScannerItems(ctx)
	if err != nil {
		return fmt.Errorf("count active scanner items: %w", err)
	}
	cap := s.Cfg.CandidateCap
	if cap <= 0 {
		cap = 10
	}
	slots := cap - activeCount
	if slots <= 0 {
		return nil // cap reached; nothing to add until items expire
	}

	// 5. Enrich candidates with news.
	since := s.now().Add(-time.Duration(s.Cfg.NewsLookbackHours) * time.Hour)
	candidates = s.enrichWithNews(ctx, candidates, since)

	// 6. LLM-score each candidate.
	var scored []scoredCandidate
	for _, c := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		result, err := s.scoreCandidate(ctx, c)
		if err != nil {
			slog.Warn("scanner: score candidate failed", "ticker", c.Ticker, "err", err)
			continue
		}
		slog.Info("scanner: candidate scored",
			"ticker", c.Ticker,
			"action", result.Action,
			"conviction", result.Conviction,
			"setup", result.SetupType,
			"reasoning", result.Reasoning,
		)
		if result.Action == "add" && result.Conviction >= s.Cfg.MinConviction {
			scored = append(scored, scoredCandidate{candidate: c, score: result})
		}
	}

	// 7. Sort by conviction descending, take only as many as available slots.
	sortByConviction(scored)
	if len(scored) > slots {
		scored = scored[:slots]
	}

	// 8. Add to watchlist and notify.
	var added []scoredCandidate
	expiresAt := s.now().AddDate(0, 0, s.ttlDays())
	for _, sc := range scored {
		timeframes := sc.score.Timeframes
		if len(timeframes) == 0 {
			timeframes = []string{"1H", "4H"}
		}
		item := store.WatchlistItem{
			Ticker:     sc.candidate.Ticker,
			Platform:   "stock",
			Strategies: []string{sc.score.Strategy},
			Timeframes: timeframes,
			// Persist scanner conviction in MinConfidence so downstream
			// engine logic (#317 RSI loosen) can read it without going
			// back to the scanner's in-memory state.
			MinConfidence: sc.score.Conviction,
			Source:        "scanner",
			// Persist WHY this candidate was sourced (graph link / sympathy
			// trigger) so the engine's deep-analysis prompt carries the origin
			// thesis instead of losing it after this pass.
			GraphContext: sc.candidate.GraphContext,
			ExpiresAt:    &expiresAt,
		}
		if err := store.AddWatchlistItem(ctx, item); err != nil {
			slog.Warn("scanner: failed to add watchlist item", "ticker", sc.candidate.Ticker, "err", err)
			continue
		}
		slog.Info("scanner: added to watchlist", "ticker", sc.candidate.Ticker, "conviction", sc.score.Conviction, "setup", sc.score.SetupType, "strategy", sc.score.Strategy)
		added = append(added, sc)

		if s.Bus != nil {
			s.Bus.Publish(events.ScannerItemAdded, events.ScannerItemAddedPayload{
				Ticker:       sc.candidate.Ticker,
				Conviction:   sc.score.Conviction,
				SetupType:    sc.score.SetupType,
				Strategy:     sc.score.Strategy,
				Timeframes:   timeframes,
				Reasoning:    sc.score.Reasoning,
				ExpiresAt:    expiresAt.Format(time.RFC3339),
				Sources:      sc.candidate.Sources,
				GraphContext: sc.candidate.GraphContext,
			})
		}
	}

	durationMs := time.Since(start).Milliseconds()

	slog.Info("scanner: scan completed",
		"candidates_evaluated", len(candidates),
		"items_added", len(added),
		"items_expired", len(expired),
		"duration_ms", durationMs,
	)

	if s.Bus != nil {
		s.Bus.Publish(events.ScanCompleted, events.ScanCompletedPayload{
			CandidatesEvaluated: len(candidates),
			ItemsAdded:          len(added),
			ItemsExpired:        len(expired),
			DurationMs:          durationMs,
		})
	}

	// 9. Send Telegram summary if anything was added.
	if len(added) > 0 && s.Notifier != nil {
		s.Notifier.SendImmediate(ctx, s.buildTelegramMessage(added, len(expired))) //nolint:errcheck
	}

	return nil
}

// fetchCandidates pulls top movers and most-active stocks and merges them.
func (s *MarketScanner) fetchCandidates(ctx context.Context) ([]SwingCandidate, error) {
	byTicker := make(map[string]*SwingCandidate)

	// Top movers (gainers + losers).
	// 0 = no gap threshold and no price/volume floors: the swing scanner wants the
	// full mover universe and does its own LLM filtering downstream.
	movers, err := data.FetchGapMovers(ctx, s.AlpacaClient, data.GapScreen{MinGapPct: 0})
	if err != nil {
		slog.Warn("scanner: fetch movers failed", "err", err)
	}
	top := s.Cfg.TopMovers
	if top <= 0 {
		top = 20
	}
	for i, m := range movers {
		if i >= top {
			break
		}
		byTicker[m.Ticker] = &SwingCandidate{
			Ticker:    m.Ticker,
			Price:     m.PreMarketLast,
			ChangePct: m.GapPct,
			Volume:    m.PreMarketVol,
			Sources:   []string{"mover"},
		}
	}

	// Most active by volume.
	topActives := s.Cfg.TopActives
	if topActives <= 0 {
		topActives = 20
	}
	actives, err := data.FetchMostActive(ctx, s.AlpacaClient, topActives)
	if err != nil {
		slog.Warn("scanner: fetch most actives failed", "err", err)
	}
	for _, a := range actives {
		if existing, ok := byTicker[a.Ticker]; ok {
			existing.Sources = append(existing.Sources, "active")
			if a.Volume > existing.Volume {
				existing.Volume = a.Volume
			}
			if existing.Price == 0 {
				existing.Price = a.Price
			}
		} else {
			byTicker[a.Ticker] = &SwingCandidate{
				Ticker:     a.Ticker,
				Price:      a.Price,
				Volume:     a.Volume,
				TradeCount: a.TradeCount,
				Sources:    []string{"active"},
			}
		}
	}

	candidates := make([]SwingCandidate, 0, len(byTicker))
	for _, c := range byTicker {
		candidates = append(candidates, *c)
	}
	return candidates, nil
}

// filterExistingWatchlist removes tickers that are already active on the watchlist.
func (s *MarketScanner) filterExistingWatchlist(ctx context.Context, candidates []SwingCandidate) ([]SwingCandidate, error) {
	items, err := store.GetWatchlistItems(ctx, true)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]struct{}, len(items))
	for _, item := range items {
		existing[item.Ticker] = struct{}{}
	}

	var filtered []SwingCandidate
	for _, c := range candidates {
		if _, found := existing[c.Ticker]; !found {
			filtered = append(filtered, c)
		}
	}
	return filtered, nil
}

// enrichWithNews fetches news for each candidate and attaches it.
func (s *MarketScanner) enrichWithNews(ctx context.Context, candidates []SwingCandidate, since time.Time) []SwingCandidate {
	if s.News == nil {
		return candidates
	}
	for i := range candidates {
		if ctx.Err() != nil {
			break
		}
		rawNews, err := s.News.FetchNews(ctx, candidates[i].Ticker, since)
		if err != nil {
			continue
		}
		if len(rawNews) > maxNewsPerTicker {
			rawNews = rawNews[:maxNewsPerTicker]
		}
		for _, n := range rawNews {
			sentiment := "NEUTRAL"
			if n.SentimentScore > sentimentPositiveThreshold {
				sentiment = "POSITIVE"
			} else if n.SentimentScore < sentimentNegativeThreshold {
				sentiment = "NEGATIVE"
			}
			candidates[i].News = append(candidates[i].News, SwingNewsItem{
				Headline:    n.Headline,
				Sentiment:   sentiment,
				PublishedAt: n.PublishedAt,
			})
		}
	}
	return candidates
}

// scoreCandidate calls the LLM scorer for a single candidate.
func (s *MarketScanner) scoreCandidate(ctx context.Context, candidate SwingCandidate) (SwingScoreResult, error) {
	sysPrompt, userMsg := BuildSwingPrompt(candidate, s.Cfg.MinConviction)

	resp, meta, err := s.Analyzer.Analyze(ctx, analysis.AnalysisRequest{
		Ticker:             candidate.Ticker,
		CustomSystemPrompt: sysPrompt,
		CustomUserMessage:  userMsg,
	}, depth.ImpactScore{Score: 0.5, Depth: 1})
	if err != nil {
		return SwingScoreResult{}, err
	}

	return parseSwingResult(meta.RawResponse, resp), nil
}

// parseSwingResult extracts the structured result from the LLM's raw response.
// Tolerant to casing, markdown fences, and chatter around the JSON block — the
// generic extractAnalysisJSON already accepts all of these, so this parser must
// too or candidates are silently downgraded to "skip" despite stage-1 success.
func parseSwingResult(rawResponse string, fallback analysis.AnalysisResponse) SwingScoreResult {
	raw := strings.TrimSpace(rawResponse)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	if !strings.HasPrefix(raw, "{") {
		if start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}"); start >= 0 && end > start {
			raw = raw[start : end+1]
		}
	}

	var parsed struct {
		Action     string   `json:"action"`
		Conviction float64  `json:"conviction"`
		SetupType  string   `json:"setup_type"`
		Strategy   string   `json:"strategy"`
		Timeframes []string `json:"timeframes"`
		Reasoning  string   `json:"reasoning"`
		Risks      []string `json:"risks"`
	}

	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		action := strings.ToLower(strings.TrimSpace(parsed.Action))
		if action == "add" || action == "skip" {
			return SwingScoreResult{
				Action:     action,
				Conviction: parsed.Conviction,
				SetupType:  parsed.SetupType,
				Strategy:   parsed.Strategy,
				Timeframes: parsed.Timeframes,
				Reasoning:  parsed.Reasoning,
				Risks:      parsed.Risks,
			}
		}
	}

	// Defense-in-depth fallback. Prefer the action the generic extractor already
	// normalized over a blind "skip" — otherwise any response that survives
	// extractAnalysisJSON but trips this local parse is silently dropped.
	action := "skip"
	if fallback.Action == analysis.ActionAdd {
		action = "add"
	}
	return SwingScoreResult{
		Action:     action,
		Conviction: fallback.Confidence,
		Reasoning:  fallback.Reasoning,
	}
}

// buildTelegramMessage constructs the Telegram notification for a scan cycle.
func (s *MarketScanner) buildTelegramMessage(added []scoredCandidate, expired int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🔍 *Scanner added %d ticker(s) to watchlist*\n\n", len(added)))
	for _, sc := range added {
		sb.WriteString(fmt.Sprintf("• *%s* — %s (conviction: %.0f%%)\n",
			sc.candidate.Ticker, sc.score.SetupType, sc.score.Conviction*100))
		if sc.candidate.GraphContext != "" {
			sb.WriteString(fmt.Sprintf("  🔗 _%s_\n", sc.candidate.GraphContext))
		}
		if sc.score.Reasoning != "" {
			sb.WriteString(fmt.Sprintf("  _%s_\n", sc.score.Reasoning))
		}
	}
	if expired > 0 {
		sb.WriteString(fmt.Sprintf("\n_Expired %d stale scanner item(s) from watchlist._", expired))
	}
	return sb.String()
}

func (s *MarketScanner) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *MarketScanner) ttlDays() int {
	if s.Cfg.TTLDays > 0 {
		return s.Cfg.TTLDays
	}
	return 3
}

// isMarketOpen returns true if the current time falls within US equity market
// hours (9:30am–4:00pm US/Eastern, Monday–Friday).
func (s *MarketScanner) isMarketOpen() bool {
	nyLoc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return false
	}
	now := s.now().In(nyLoc)
	wd := now.Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return false
	}
	openTime := time.Date(now.Year(), now.Month(), now.Day(), 9, 30, 0, 0, nyLoc)
	closeTime := time.Date(now.Year(), now.Month(), now.Day(), 16, 0, 0, 0, nyLoc)
	return !now.Before(openTime) && now.Before(closeTime)
}

// scoredCandidate pairs a candidate with its LLM score.
type scoredCandidate struct {
	candidate SwingCandidate
	score     SwingScoreResult
}

// sortByConviction sorts in-place, highest conviction first.
func sortByConviction(items []scoredCandidate) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].score.Conviction > items[j-1].score.Conviction; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
