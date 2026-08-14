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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
	pullbackMinMarketCapMM     = 100.0
	pullbackMinDollarVolume    = 1_000_000.0
)

// CompanyFundamentalsProvider supplies the free profile data needed by the
// research universe's market-cap floor.
type CompanyFundamentalsProvider interface {
	FetchCompanyFundamentals(ctx context.Context, ticker string) (data.CompanyFundamentals, error)
}

// Notifier is satisfied by notify.TelegramBot (and any test double).
type Notifier interface {
	SendImmediate(ctx context.Context, message string) error
}

// MarketScanner discovers swing-trade candidates and manages a time-bounded
// watchlist of scanner-sourced tickers.
type MarketScanner struct {
	AlpacaClient    *data.AlpacaClient
	Analyzer        *analysis.Analyzer
	News            data.NewsProvider
	Fundamentals    CompanyFundamentalsProvider
	DailyBars       data.BulkDailyBarsProvider // optional; required only for correlation-aware promotion
	Notifier        Notifier                   // optional — nil disables Telegram notifications
	Bus             bus
	Cfg             config.ScannerConfig
	Now             func() time.Time // injectable for tests; defaults to time.Now
	CycleOwnerToken string           // optional stable process token for regular-scan leases
	cycleOwnerMu    sync.Mutex
	// PullbackUniverseEnabled applies the research universe before LLM scoring:
	// >$100M market cap, >$1M dollar volume, >=30% on a 1/3/6-month horizon,
	// then retain the top momentum quartile.
	PullbackUniverseEnabled bool
	// PullbackMinMomentumReturnPct mirrors the entry-gate threshold so universe
	// ranking and execution eligibility cannot diverge. Zero resolves to 30%.
	PullbackMinMomentumReturnPct float64
	// UniverseVersionID and its evidence timestamps link a scan to the exact
	// immutable eligible-universe denominator used by newer discovery sources.
	// They remain empty for legacy scans and while scanner.universe is disabled.
	UniverseVersionID string
	UniverseAsOf      *time.Time
	RegimeReference   string

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
func (s *MarketScanner) runScan(ctx context.Context) (scanErr error) {
	start := s.now()
	if s.Cfg.PromotionCorrelationEnabled {
		if s.DailyBars == nil {
			return fmt.Errorf("promotion correlation enabled but daily-bars provider is unavailable")
		}
		if !store.IsConnected() {
			return fmt.Errorf("promotion correlation enabled but database is unavailable")
		}
	}
	scanCtx, cancelScan := context.WithCancel(ctx)
	defer cancelScan()
	ctx = scanCtx
	cycleKey := ""
	var cycleLease *store.ScannerCycleLease
	s.cycleOwnerMu.Lock()
	if s.CycleOwnerToken == "" {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			s.cycleOwnerMu.Unlock()
			return fmt.Errorf("generate scanner cycle owner token: %w", err)
		}
		s.CycleOwnerToken = hex.EncodeToString(token[:])
	}
	cycleOwner := s.CycleOwnerToken
	s.cycleOwnerMu.Unlock()
	interval := time.Duration(s.Cfg.ScanIntervalHours) * time.Hour
	if interval <= 0 {
		interval = 2 * time.Hour
	}
	if store.IsConnected() {
		cycleKey = "regular:" + start.UTC().Truncate(interval).Format(time.RFC3339)
		claimed, claimErr := store.ClaimScannerCycle(ctx, cycleKey, cycleOwner, interval)
		if claimErr != nil {
			return fmt.Errorf("claim scanner cycle: %w", claimErr)
		}
		if !claimed {
			slog.Info("scanner: cycle already claimed", "run_key", cycleKey)
			return nil
		}
		cycleLease = &store.ScannerCycleLease{
			RunKey: cycleKey, OwnerToken: cycleOwner, Duration: interval,
		}
		heartbeatCtx, stopHeartbeat := context.WithCancel(context.Background())
		defer stopHeartbeat()
		heartbeat := interval / 3
		if heartbeat < time.Second {
			heartbeat = time.Second
		}
		go func() {
			ticker := time.NewTicker(heartbeat)
			defer ticker.Stop()
			for {
				select {
				case <-heartbeatCtx.Done():
					return
				case <-ticker.C:
					if renewed, err := store.RenewScannerCycle(
						heartbeatCtx, cycleKey, cycleOwner, interval,
					); err != nil || !renewed {
						slog.Warn("scanner: renew cycle lease", "run_key", cycleKey, "renewed", renewed, "err", err)
						cancelScan()
						return
					}
				}
			}
		}()
		defer func() {
			completeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := store.CompleteScannerCycle(completeCtx, cycleKey, cycleOwner, scanErr != nil); err != nil {
				slog.Warn("scanner: complete cycle lease", "run_key", cycleKey, "err", err)
			}
		}()
	}
	weeklyCandidates, err := s.weeklyLeaderCandidates(ctx)
	if err != nil {
		return fmt.Errorf("load weekly leader candidates: %w", err)
	}
	defer s.releaseWeeklyLeaderClaims(ctx, weeklyCandidates)
	industryCandidates, err := s.potentIndustryCandidates(ctx)
	if err != nil {
		return fmt.Errorf("load potent industry candidates: %w", err)
	}
	defer s.releasePotentIndustryClaims(ctx, industryCandidates)
	premarketCandidates, err := s.premarketEpisodicCandidates(ctx)
	if err != nil {
		return fmt.Errorf("load premarket episodic candidates: %w", err)
	}
	defer s.releasePremarketEpisodicClaims(ctx, premarketCandidates)
	if len(weeklyCandidates) > 0 && len(industryCandidates) > 0 &&
		weeklyCandidates[0].UniverseVersionID != industryCandidates[0].UniverseVersionID {
		return fmt.Errorf(
			"discovery source universe mismatch: weekly leader=%s potent industry=%s",
			weeklyCandidates[0].UniverseVersionID,
			industryCandidates[0].UniverseVersionID,
		)
	}
	recorder := newScanRecorder(s, start)
	// Premarket observations normally use today's newest eligible universe,
	// while after-close weekly/industry claims may still belong to the previous
	// completed session. Prefer the newest premarket denominator for the scanner
	// run; every source's exact universe remains independently linked by its
	// token-fenced source row.
	if len(premarketCandidates) > 0 {
		recorder.run.UniverseVersionID = premarketCandidates[0].UniverseVersionID
		asOf := premarketCandidates[0].UniverseAsOf
		recorder.run.UniverseAsOf = &asOf
	} else if len(weeklyCandidates) > 0 {
		recorder.run.UniverseVersionID = weeklyCandidates[0].UniverseVersionID
		asOf := weeklyCandidates[0].UniverseAsOf
		recorder.run.UniverseAsOf = &asOf
	} else if len(industryCandidates) > 0 {
		recorder.run.UniverseVersionID = industryCandidates[0].UniverseVersionID
		asOf := industryCandidates[0].UniverseAsOf
		recorder.run.UniverseAsOf = &asOf
	}
	recorder.begin(ctx)
	defer func() { recorder.finish(scanErr) }()

	if s.Bus != nil {
		s.Bus.Publish(events.ScanStarted, events.ScanStartedPayload{})
	}

	slog.Info("scanner: scan started")

	// 1. Expire stale scanner items before adding new ones.
	expired, err := store.ExpireScannedItems(ctx)
	if err != nil {
		slog.Warn("scanner: expire stale items", "err", err)
	}
	recorder.run.ExpiredItemCount = len(expired)

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
	candidates = mergeWeeklyLeaderCandidates(candidates, weeklyCandidates)
	candidates = mergePotentIndustryCandidates(candidates, industryCandidates)
	candidates = mergePremarketEpisodicCandidates(candidates, premarketCandidates)
	promotionCapActive := s.Cfg.PromotionMaxPerIndustry > 0
	if promotionCapActive {
		if strings.TrimSpace(recorder.run.UniverseVersionID) == "" || !store.IsConnected() {
			slog.Warn("scanner: point-in-time universe unavailable; continuing without industry cap")
			// Diversification is optional. A transient taxonomy failure must not
			// suppress the legacy movers/actives scan; leave the pool uncapped for
			// this run and retain the error in logs for operational visibility.
			promotionCapActive = false
		} else {
			hydratedCandidates, hydrateErr := s.hydrateCandidateTaxonomy(
				ctx, candidates, recorder.run.UniverseVersionID, recorder.run.StartedAt,
			)
			if hydrateErr != nil {
				slog.Warn("scanner: hydrate candidate taxonomy failed; continuing without industry cap",
					"err", hydrateErr)
				promotionCapActive = false
			} else {
				candidates = hydratedCandidates
			}
		}
	}
	promotionMaxPerIndustry := 0
	if promotionCapActive {
		promotionMaxPerIndustry = s.Cfg.PromotionMaxPerIndustry
	}
	recorder.register(candidates)

	if len(candidates) == 0 {
		return nil
	}

	// 3. Mark the complete pullback universe before removing active names.
	// The universe is an additive entry path: standard scanner candidates stay
	// in the pool, while qualified names carry a durable pullback strategy tag.
	// Ranking before active-name removal prevents later scans from promoting
	// names that were never in the full universe's top quartile.
	requalificationRequired, err := pullbackRequalificationRequired(ctx)
	if err != nil {
		return fmt.Errorf("load pullback requalification state: %w", err)
	}
	if s.PullbackUniverseEnabled || len(requalificationRequired) > 0 {
		recorder.setQuarantine(requalificationRequired)
		qualificationInput := candidates
		if !s.PullbackUniverseEnabled {
			qualificationInput = filterCandidatesByTicker(candidates, requalificationRequired)
		}
		qualification := s.qualifyPullbackUniverse(ctx, qualificationInput)
		recorder.qualification(qualification)
		if len(qualification.qualified) == 0 {
			slog.Info("scanner: no candidates passed pullback-strength universe")
		}
		if s.PullbackUniverseEnabled {
			candidates = markPullbackQualification(candidates, qualification)
		} else {
			candidates = markQuarantinedQualificationKnown(
				candidates, qualification.known, requalificationRequired,
			)
		}
		beforeRequalificationFilter := candidates
		candidates = filterUnavailableRequalifications(candidates, requalificationRequired)
		recorder.markRemoved(beforeRequalificationFilter, candidates, "requalification_unavailable")
		s.markDiscoveryCandidates(ctx, beforeRequalificationFilter, candidates,
			"failed", recorder.run.ID, "pullback requalification evidence unavailable")
	}

	// 3.5 Dedup against active watchlist/position exposure only after universe
	// selection. Correlation mode takes one normalized exposure snapshot and
	// reuses it for both same-ticker guards and pairwise selection.
	var exposure promotionExposure
	if s.Cfg.PromotionCorrelationEnabled {
		exposure, err = loadPromotionExposure(ctx)
		if err != nil {
			return fmt.Errorf("load promotion exposure: %w", err)
		}
		// Position exposure is the stronger exclusion. Apply it first so a
		// ticker that is both held and watchlisted is audited as open_position,
		// rather than being misattributed to the less-specific watchlist guard.
		beforeExposureFilter := candidates
		candidates = filterActiveCandidates(candidates, exposure.positions)
		recorder.markRemoved(beforeExposureFilter, candidates, "open_position")
		s.markDiscoveryCandidates(ctx, beforeExposureFilter, candidates,
			"open_position", recorder.run.ID, "ticker already has an open position")
		beforeWatchlistFilter := candidates
		candidates = filterActiveCandidates(candidates, exposure.watchlist)
		recorder.markRemoved(beforeWatchlistFilter, candidates, "active_watchlist")
		s.markDiscoveryCandidates(ctx, beforeWatchlistFilter, candidates,
			"active_watchlist", recorder.run.ID, "already active on watchlist")
	} else {
		beforeWatchlistFilter := candidates
		candidates, err = s.filterExistingWatchlist(ctx, candidates)
		if err != nil {
			return fmt.Errorf("filter watchlist: %w", err)
		}
		recorder.markRemoved(beforeWatchlistFilter, candidates, "active_watchlist")
		s.markDiscoveryCandidates(ctx, beforeWatchlistFilter, candidates,
			"active_watchlist", recorder.run.ID, "already active on watchlist")
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
		for _, c := range candidates {
			if snapshot := recorder.candidates[c.Ticker]; snapshot != nil {
				snapshot.FinalStage = "candidate_cap"
			}
		}
		s.markDiscoveryCandidates(ctx, candidates, nil,
			"cap_suppressed", recorder.run.ID, "scanner candidate cap reached")
		return nil // cap reached; nothing to add until items expire
	}

	// 5. Enrich candidates with news.
	since := s.now().Add(-time.Duration(s.Cfg.NewsLookbackHours) * time.Hour)
	candidates = s.enrichWithNews(ctx, candidates, since)
	recorder.setEligible(candidates)

	// 6. LLM-score each candidate.
	var scored []scoredCandidate
	for _, c := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		result, err := s.scoreCandidate(ctx, c)
		recorder.scored(c, result, err, s.Cfg.MinConviction)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.markDiscoveryCandidate(ctx, c, "failed", recorder.run.ID, err.Error())
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
		} else {
			reason := fmt.Sprintf(
				"normal scanner rejected candidate: action=%s conviction=%.3f threshold=%.3f",
				result.Action, result.Conviction, s.Cfg.MinConviction,
			)
			s.markDiscoveryCandidate(ctx, c, "rejected", recorder.run.ID, reason)
		}
	}

	// 7. Sort by conviction descending, take only as many as available slots.
	if err := s.enrichPromotionCorrelations(ctx, scored, start, exposure); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, candidate := range scored {
		if len(candidate.correlationUnavailable) > 0 {
			recorder.promotionCorrelationUnavailable(
				candidate.candidate.Ticker, candidate.correlationUnavailable,
			)
		}
	}
	sortByConviction(scored)
	var promotionRejections []PromotionRejection
	correlationCapActive := s.Cfg.PromotionCorrelationEnabled && s.DailyBars != nil &&
		s.Cfg.PromotionMaxAbsCorrelation > 0
	if promotionCapActive || correlationCapActive {
		promotionInput := scored
		var rejected []PromotionRejection
		maxCorrelation := 0.0
		if correlationCapActive {
			maxCorrelation = s.Cfg.PromotionMaxAbsCorrelation
		}
		scored, rejected = selectScoredForPromotionWithCorrelation(
			scored, slots, promotionMaxPerIndustry, maxCorrelation,
		)
		for _, rejected := range rejected {
			promotionRejections = append(promotionRejections, rejected)
			reason := rejected.Reason
			if rejected.ConflictKey != "" {
				reason += ": " + rejected.ConflictKey
			}
			if rejected.Candidate.Index >= 0 && rejected.Candidate.Index < len(promotionInput) {
				sc := promotionInput[rejected.Candidate.Index]
				s.markDiscoveryCandidate(ctx, sc.candidate, "cap_suppressed", recorder.run.ID, reason)
				recorder.promotionRejectedWithEvidence(sc.candidate.Ticker, rejected)
			}
		}
	}
	if len(scored) > slots {
		for _, suppressed := range scored[slots:] {
			s.markDiscoveryCandidate(
				ctx, suppressed.candidate, "cap_suppressed", recorder.run.ID,
				"candidate ranked beyond available scanner slots",
			)
		}
		scored = scored[:slots]
	}
	recorder.selected(scored)

	// 8. Add to watchlist and notify.
	var added []scoredCandidate
	promotionState := append([]scoredCandidate(nil), scored...)
	promotionActive := make([]bool, len(scored))
	for index := range scored {
		if err := ctx.Err(); err != nil {
			return err
		}
		sc := scored[index]
		expiresAt := s.now().AddDate(0, 0, s.candidateTTLDays(sc.candidate))
		// A claim-loss fallback may change the candidate's effective taxonomy.
		// Re-check later candidates against peers that were actually admitted,
		// without allowing unattempted lower-ranked candidates to consume the cap.
		if conflict := promotionIndustryConflict(
			sc.candidate,
			activePromotionPeers(promotionState, promotionActive, index),
			promotionMaxPerIndustry,
		); conflict != "" {
			reason := "industry_cap: " + conflict
			s.markDiscoveryCandidate(ctx, sc.candidate, "cap_suppressed", recorder.run.ID, reason)
			recorder.promotionRejected(sc.candidate.Ticker, reason)
			promotionActive[index] = false
			continue
		}
		timeframes := sc.score.Timeframes
		if len(timeframes) == 0 {
			timeframes = []string{"1H", "4H"}
		}
		strategies := candidateStrategies(sc.score.Strategy, sc.candidate.PullbackQualified)
		if hasSource(sc.candidate.Sources, "weekly_leader") {
			strategies = appendUniqueStrategy(strategies, "weekly_leader")
		}
		if hasSource(sc.candidate.Sources, "potent_industry") {
			strategies = appendUniqueStrategy(strategies, "potent_industry")
		}
		if hasSource(sc.candidate.Sources, "premarket_episodic") {
			strategies = appendUniqueStrategy(strategies, "premarket_episodic")
		}
		discoveryContext, executionProfile, profileErr := buildScannerTradeProfile(
			sc.candidate, sc.score,
		)
		if profileErr != nil {
			return fmt.Errorf("build scanner trade profile for %s: %w", sc.candidate.Ticker, profileErr)
		}
		if err := recorder.setTradeProfile(
			sc.candidate, discoveryContext, executionProfile,
		); err != nil {
			return fmt.Errorf("record scanner trade profile for %s: %w", sc.candidate.Ticker, err)
		}
		item := store.WatchlistItem{
			Ticker:     sc.candidate.Ticker,
			Platform:   "stock",
			Strategies: strategies,
			Timeframes: timeframes,
			// Persist scanner conviction in MinConfidence so downstream
			// engine logic (#317 RSI loosen) can read it without going
			// back to the scanner's in-memory state.
			MinConfidence: sc.score.Conviction,
			Source:        "scanner",
			// Persist WHY this candidate was sourced (graph link / sympathy
			// trigger) so the engine's deep-analysis prompt carries the origin
			// thesis instead of losing it after this pass.
			GraphContext:                    sc.candidate.GraphContext,
			DiscoveryContext:                discoveryContext,
			ExecutionProfile:                executionProfile,
			ScannerRunID:                    recorder.run.ID,
			ExpiresAt:                       &expiresAt,
			PullbackRequalificationComplete: sc.candidate.PullbackQualificationKnown,
		}
		if err := s.renewScannerCycleBeforePromotion(ctx, cycleKey, cycleOwner, interval); err != nil {
			return err
		}
		addErr := s.promoteDiscoveryCandidate(ctx, item, sc.candidate, cycleLease)
		skipCandidate := false
		for errors.Is(addErr, store.ErrScannerSourcePromotionClaimLost) {
			var claimErr *store.ScannerSourcePromotionClaimError
			source := ""
			if errors.As(addErr, &claimErr) {
				source = claimErr.Source
			}
			fallback, ok := candidateAfterPromotionClaimLoss(sc.candidate, source)
			if !ok {
				break
			}
			fallbackScore, scoreErr := s.scoreCandidate(ctx, fallback)
			recorder.fallbackScored(
				fallback, fallbackScore, scoreErr, s.Cfg.MinConviction,
			)
			fallbackDiscovery, fallbackProfile, fallbackProfileErr := buildScannerTradeProfile(
				fallback, fallbackScore,
			)
			if fallbackProfileErr != nil {
				return fmt.Errorf("build fallback scanner trade profile for %s: %w", fallback.Ticker, fallbackProfileErr)
			}
			if err := recorder.setTradeProfile(
				fallback, fallbackDiscovery, fallbackProfile,
			); err != nil {
				return fmt.Errorf("record fallback scanner trade profile for %s: %w", fallback.Ticker, err)
			}
			if scoreErr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				s.markDiscoveryCandidate(
					ctx, fallback, "failed", recorder.run.ID, scoreErr.Error(),
				)
				slog.Warn("scanner: source fallback score failed",
					"ticker", fallback.Ticker, "err", scoreErr)
				promotionActive[index] = false
				skipCandidate = true
				break
			}
			if fallbackScore.Action != "add" ||
				fallbackScore.Conviction < s.Cfg.MinConviction {
				s.markDiscoveryCandidate(
					ctx, fallback, "rejected", recorder.run.ID,
					fmt.Sprintf(
						"fallback rejected: action=%s conviction=%.3f threshold=%.3f",
						fallbackScore.Action, fallbackScore.Conviction,
						s.Cfg.MinConviction,
					),
				)
				slog.Info("scanner: source fallback rejected after claim loss",
					"ticker", fallback.Ticker,
					"action", fallbackScore.Action,
					"conviction", fallbackScore.Conviction)
				promotionActive[index] = false
				skipCandidate = true
				break
			}
			timeframes = fallbackScore.Timeframes
			if len(timeframes) == 0 {
				timeframes = []string{"1H", "4H"}
			}
			strategies = candidateStrategies(
				fallbackScore.Strategy, fallback.PullbackQualified,
			)
			if hasSource(fallback.Sources, "weekly_leader") {
				strategies = appendUniqueStrategy(strategies, "weekly_leader")
			}
			if hasSource(fallback.Sources, "potent_industry") {
				strategies = appendUniqueStrategy(strategies, "potent_industry")
			}
			if hasSource(fallback.Sources, "premarket_episodic") {
				strategies = appendUniqueStrategy(strategies, "premarket_episodic")
			}
			item.Strategies = strategies
			item.Timeframes = timeframes
			item.MinConfidence = fallbackScore.Conviction
			item.GraphContext = fallback.GraphContext
			item.DiscoveryContext = fallbackDiscovery
			item.ExecutionProfile = fallbackProfile
			item.PullbackRequalificationComplete =
				fallback.PullbackQualificationKnown
			sc = scoredCandidate{candidate: fallback, score: fallbackScore}
			peers := activePromotionPeers(promotionState, promotionActive, index)
			if conflict := promotionIndustryConflict(fallback, peers, promotionMaxPerIndustry); conflict != "" {
				reason := "industry_cap: " + conflict
				s.markDiscoveryCandidate(ctx, fallback, "cap_suppressed", recorder.run.ID, reason)
				recorder.promotionRejected(fallback.Ticker, reason)
				slog.Info("scanner: source fallback suppressed by promotion cap",
					"ticker", fallback.Ticker, "conflict", conflict)
				promotionActive[index] = false
				skipCandidate = true
				break
			}
			promotionState[index] = sc
			scored[index] = sc
			expiresAt = s.now().AddDate(0, 0, s.candidateTTLDays(sc.candidate))
			item.ExpiresAt = &expiresAt
			if err := s.renewScannerCycleBeforePromotion(ctx, cycleKey, cycleOwner, interval); err != nil {
				return err
			}
			addErr = s.promoteDiscoveryFallback(ctx, item, fallback, cycleLease)
		}
		if skipCandidate {
			continue
		}
		if errors.Is(addErr, store.ErrScannerCycleLeaseLost) {
			return addErr
		}
		if errors.Is(addErr, store.ErrWatchlistActiveItemExists) {
			promotionActive[index] = false
			recorder.watchlistResult(sc.candidate, sc.score, strategies, addErr)
			slog.Info(
				"scanner: source fallback lost to active watchlist item",
				"ticker", sc.candidate.Ticker,
			)
			continue
		}
		if addErr != nil {
			promotionActive[index] = false
			recorder.watchlistResult(sc.candidate, sc.score, strategies, addErr)
			if !errors.Is(addErr, store.ErrScannerSourcePromotionClaimLost) {
				s.markDiscoveryCandidate(
					ctx, sc.candidate, "failed", recorder.run.ID, addErr.Error(),
				)
			}
			slog.Warn("scanner: failed to add watchlist item",
				"ticker", sc.candidate.Ticker, "err", addErr)
			continue
		}
		// Only candidates that have actually been admitted to the watchlist
		// may consume the industry cap for later fallback decisions. Keeping
		// future candidates inactive prevents a lower-ranked, not-yet-attempted
		// peer from suppressing a higher-ranked fallback.
		promotionActive[index] = true
		recorder.watchlistResult(sc.candidate, sc.score, strategies, nil)
		slog.Info("scanner: added to watchlist", "ticker", sc.candidate.Ticker, "conviction", sc.score.Conviction, "setup", sc.score.SetupType, "strategy", sc.score.Strategy)
		added = append(added, sc)

		if s.Bus != nil {
			s.Bus.Publish(events.ScannerItemAdded, events.ScannerItemAddedPayload{
				Ticker:                    sc.candidate.Ticker,
				Conviction:                sc.score.Conviction,
				SetupType:                 sc.score.SetupType,
				Strategy:                  sc.score.Strategy,
				Timeframes:                timeframes,
				Reasoning:                 sc.score.Reasoning,
				ExpiresAt:                 expiresAt.Format(time.RFC3339),
				Sources:                   sc.candidate.Sources,
				GraphContext:              sc.candidate.GraphContext,
				WeeklyLeaderEvidence:      sc.candidate.WeeklyLeaderEvidence,
				PotentIndustryEvidence:    sc.candidate.PotentIndustryEvidence,
				PremarketEpisodicEvidence: sc.candidate.PremarketEpisodicEvidence,
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
		s.Notifier.SendImmediate(ctx, s.buildTelegramMessage(added, len(expired), promotionRejections)) //nolint:errcheck
	}

	return nil
}

type pullbackQualification struct {
	qualified []SwingCandidate
	known     map[string]struct{}
	outcomes  map[string]pullbackQualificationOutcome
}

type pullbackQualificationOutcome struct {
	candidate SwingCandidate
	stage     string
	reason    string
	known     bool
	qualified bool
	rank      int
}

func markPullbackQualification(all []SwingCandidate, result pullbackQualification) []SwingCandidate {
	qualifiedByTicker := make(map[string]SwingCandidate, len(result.qualified))
	for _, candidate := range result.qualified {
		qualifiedByTicker[candidate.Ticker] = candidate
	}
	out := append([]SwingCandidate(nil), all...)
	for i := range out {
		_, out[i].PullbackQualificationKnown = result.known[out[i].Ticker]
		outcome := result.outcomes[out[i].Ticker]
		if out[i].weeklyLeaderFallback != nil {
			fallback := cloneSwingCandidate(*out[i].weeklyLeaderFallback)
			applyPullbackOutcome(&fallback, outcome)
			out[i].weeklyLeaderFallback = &fallback
		}
		if out[i].potentIndustryFallback != nil {
			fallback := cloneSwingCandidate(*out[i].potentIndustryFallback)
			applyPullbackOutcome(&fallback, outcome)
			out[i].potentIndustryFallback = &fallback
		}
		if out[i].potentIndustrySource != nil {
			source := cloneSwingCandidate(*out[i].potentIndustrySource)
			applyPullbackOutcome(&source, outcome)
			out[i].potentIndustrySource = &source
		}
		qualifiedCandidate, ok := qualifiedByTicker[out[i].Ticker]
		if !ok {
			continue
		}
		out[i].PullbackQualified = true
		out[i].Return1M = qualifiedCandidate.Return1M
		out[i].Return3M = qualifiedCandidate.Return3M
		out[i].Return6M = qualifiedCandidate.Return6M
		out[i].MarketCapMM = qualifiedCandidate.MarketCapMM
		out[i].DollarVolume = qualifiedCandidate.DollarVolume
	}
	return out
}

func applyPullbackOutcome(
	candidate *SwingCandidate, outcome pullbackQualificationOutcome,
) {
	candidate.PullbackQualificationKnown = outcome.known
	candidate.PullbackQualified = outcome.qualified
	if !outcome.known {
		return
	}
	candidate.Price = outcome.candidate.Price
	candidate.Volume = outcome.candidate.Volume
	candidate.Return1M = outcome.candidate.Return1M
	candidate.Return3M = outcome.candidate.Return3M
	candidate.Return6M = outcome.candidate.Return6M
	candidate.MarketCapMM = outcome.candidate.MarketCapMM
	candidate.DollarVolume = outcome.candidate.DollarVolume
}

func filterUnavailableRequalifications(
	candidates []SwingCandidate, required map[string]struct{},
) []SwingCandidate {
	filtered := make([]SwingCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if _, quarantined := required[candidate.Ticker]; quarantined &&
			!candidate.PullbackQualificationKnown {
			continue
		}
		filtered = append(filtered, candidate)
	}
	return filtered
}

func filterCandidatesByTicker(
	candidates []SwingCandidate, included map[string]struct{},
) []SwingCandidate {
	filtered := make([]SwingCandidate, 0, len(included))
	for _, candidate := range candidates {
		if _, ok := included[candidate.Ticker]; ok {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func markQuarantinedQualificationKnown(
	candidates []SwingCandidate,
	known map[string]struct{},
	required map[string]struct{},
) []SwingCandidate {
	out := append([]SwingCandidate(nil), candidates...)
	for i := range out {
		_, quarantined := required[out[i].Ticker]
		_, definitive := known[out[i].Ticker]
		out[i].PullbackQualificationKnown = quarantined && definitive
		if out[i].weeklyLeaderFallback != nil {
			fallback := cloneSwingCandidate(*out[i].weeklyLeaderFallback)
			fallback.PullbackQualificationKnown = quarantined && definitive
			out[i].weeklyLeaderFallback = &fallback
		}
		if out[i].potentIndustryFallback != nil {
			fallback := cloneSwingCandidate(*out[i].potentIndustryFallback)
			fallback.PullbackQualificationKnown = quarantined && definitive
			out[i].potentIndustryFallback = &fallback
		}
		if out[i].potentIndustrySource != nil {
			source := cloneSwingCandidate(*out[i].potentIndustrySource)
			source.PullbackQualificationKnown = quarantined && definitive
			out[i].potentIndustrySource = &source
		}
	}
	return out
}

func pullbackRequalificationRequired(ctx context.Context) (map[string]struct{}, error) {
	items, err := store.GetWatchlistItems(ctx, false)
	if err != nil {
		return nil, err
	}
	required := make(map[string]struct{})
	for _, item := range items {
		if item.PullbackRequalificationRequired {
			required[item.Ticker] = struct{}{}
		}
	}
	return required, nil
}

func candidateStrategies(primary string, pullbackQualified bool) []string {
	strategies := []string{primary}
	if pullbackQualified && primary != "pullback_strength" {
		strategies = append(strategies, "pullback_strength")
	}
	return strategies
}

func hasSource(sources []string, target string) bool {
	for _, source := range sources {
		if source == target {
			return true
		}
	}
	return false
}

func appendUniqueStrategy(strategies []string, strategy string) []string {
	for _, existing := range strategies {
		if existing == strategy {
			return strategies
		}
	}
	return append(strategies, strategy)
}

func (s *MarketScanner) renewScannerCycleBeforePromotion(
	ctx context.Context, runKey, ownerToken string, lease time.Duration,
) error {
	if runKey == "" {
		return nil
	}
	renewed, err := store.RenewScannerCycle(ctx, runKey, ownerToken, lease)
	if err != nil {
		return fmt.Errorf("renew scanner cycle before promotion: %w", err)
	}
	if !renewed {
		return errors.New("scanner cycle lease lost before promotion")
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

// hydrateCandidateTaxonomy joins ordinary mover/active candidates to the
// immutable universe selected for this scan. Discovery sources that already
// carry taxonomy are refreshed from the same point-in-time member when one is
// available; missing members remain uncapped by the promotion selector.
func (s *MarketScanner) hydrateCandidateTaxonomy(
	ctx context.Context, candidates []SwingCandidate, versionID string, asOf time.Time,
) ([]SwingCandidate, error) {
	if s.Cfg.PromotionMaxPerIndustry <= 0 ||
		strings.TrimSpace(versionID) == "" || !store.IsConnected() {
		return candidates, nil
	}
	out := append([]SwingCandidate(nil), candidates...)
	for i := range out {
		member, err := store.GetEligibleSwingUniverseMemberAsOf(
			ctx, versionID, out[i].Ticker, asOf,
		)
		if err != nil {
			return nil, err
		}
		if member == nil {
			excludeUniverseTaxonomyFromPromotion(&out[i])
			continue
		}
		applyUniverseTaxonomyToLineage(&out[i], member)
	}
	return out, nil
}

func applyUniverseTaxonomyToLineage(candidate *SwingCandidate, member *store.SwingUniverseMember) {
	if candidate == nil || member == nil {
		return
	}
	applyUniverseTaxonomy(candidate, member)
	for _, nested := range []*SwingCandidate{
		candidate.weeklyLeaderFallback,
		candidate.potentIndustryFallback,
		candidate.potentIndustrySource,
	} {
		applyUniverseTaxonomy(nested, member)
	}
}

func excludeUniverseTaxonomyFromPromotion(candidate *SwingCandidate) {
	if candidate == nil {
		return
	}
	candidate.promotionTaxonomyExcluded = true
	for _, nested := range []*SwingCandidate{
		candidate.weeklyLeaderFallback,
		candidate.potentIndustryFallback,
		candidate.potentIndustrySource,
	} {
		if nested != nil {
			nested.promotionTaxonomyExcluded = true
		}
	}
}

func applyUniverseTaxonomy(candidate *SwingCandidate, member *store.SwingUniverseMember) {
	if candidate == nil || member == nil {
		return
	}
	candidate.Sector = member.Sector
	candidate.Industry = member.Industry
	candidate.TaxonomyStatus = member.TaxonomyStatus
	candidate.promotionTaxonomyExcluded = false
	memberFlag := true
	candidate.UniverseMember = &memberFlag
	candidate.TaxonomyAsOf = cloneTimePointer(member.TaxonomyAsOf)
	candidate.TaxonomyProvider = member.TaxonomyProvider
	candidate.TaxonomyEvidence = append(json.RawMessage(nil), member.TaxonomyEvidence...)
	if candidate.MarketCapMM == 0 && member.MarketCapMM != nil {
		candidate.MarketCapMM = *member.MarketCapMM
	}
	if candidate.DollarVolume == 0 && member.DollarVolume != nil {
		candidate.DollarVolume = *member.DollarVolume
	}
}

// filterExistingWatchlist removes tickers that are already active on the watchlist.
func (s *MarketScanner) filterExistingWatchlist(ctx context.Context, candidates []SwingCandidate) ([]SwingCandidate, error) {
	items, err := store.GetWatchlistItems(ctx, true)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]struct{}, len(items))
	for _, item := range items {
		if key := normalizePromotionKey(item.Ticker); key != "" {
			existing[key] = struct{}{}
		}
	}

	return filterActiveCandidates(candidates, existing), nil
}

func filterActiveCandidates(
	candidates []SwingCandidate, existing map[string]struct{},
) []SwingCandidate {
	filtered := make([]SwingCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if _, found := existing[normalizePromotionKey(candidate.Ticker)]; !found {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

// qualifyPullbackUniverse applies the study's objective liquidity/strength
// screen and retains the top quartile by equal-weighted 1/3/6-month return.
// Missing fundamentals or history fail closed because this screen defines the
// strategy universe.
func (s *MarketScanner) qualifyPullbackUniverse(ctx context.Context, candidates []SwingCandidate) pullbackQualification {
	result := pullbackQualification{
		known:    make(map[string]struct{}, len(candidates)),
		outcomes: make(map[string]pullbackQualificationOutcome, len(candidates)),
	}
	for _, candidate := range candidates {
		result.outcomes[candidate.Ticker] = pullbackQualificationOutcome{
			candidate: candidate,
			stage:     "not_evaluated",
		}
	}
	if s.AlpacaClient == nil || s.Fundamentals == nil {
		slog.Warn("scanner: pullback universe unavailable — market data or fundamentals provider missing")
		for ticker, outcome := range result.outcomes {
			outcome.stage = "provider_unavailable"
			outcome.reason = "market data or fundamentals provider missing"
			result.outcomes[ticker] = outcome
		}
		return result
	}
	qualified := make([]SwingCandidate, 0, len(candidates))
	minMomentumReturn := s.PullbackMinMomentumReturnPct
	if minMomentumReturn <= 0 {
		minMomentumReturn = 30
	}
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		history, err := s.AlpacaClient.FetchHistory(ctx, candidate.Ticker, data.Timeframe1Day, 130)
		if err != nil || len(history) < 127 {
			slog.Warn("scanner: pullback universe history unavailable", "ticker", candidate.Ticker, "err", err)
			outcome := result.outcomes[candidate.Ticker]
			outcome.stage = "history_unavailable"
			if err != nil {
				outcome.reason = err.Error()
			} else {
				outcome.reason = fmt.Sprintf("need 127 daily bars; got %d", len(history))
			}
			result.outcomes[candidate.Ticker] = outcome
			continue
		}
		fundamentals, err := s.Fundamentals.FetchCompanyFundamentals(ctx, candidate.Ticker)
		if err != nil {
			outcome := result.outcomes[candidate.Ticker]
			outcome.stage = "fundamentals_unavailable"
			outcome.reason = err.Error()
			result.outcomes[candidate.Ticker] = outcome
			continue
		}
		if fundamentals.MarketCapitalization <= 0 {
			slog.Warn("scanner: pullback universe fundamentals unavailable",
				"ticker", candidate.Ticker)
			outcome := result.outcomes[candidate.Ticker]
			outcome.stage = "fundamentals_unavailable"
			outcome.reason = "market capitalization unavailable"
			result.outcomes[candidate.Ticker] = outcome
			continue
		}
		result.known[candidate.Ticker] = struct{}{}
		price := candidate.Price
		if price <= 0 {
			price = history[len(history)-1].Close
		}
		volume := candidate.Volume
		if volume <= 0 {
			volume = history[len(history)-1].Volume
		}
		dollarVolume := price * float64(volume)
		candidate.Price = price
		candidate.Volume = volume
		candidate.MarketCapMM = fundamentals.MarketCapitalization
		candidate.DollarVolume = dollarVolume
		candidate.Return1M = scannerHorizonReturn(price, history, 21)
		candidate.Return3M = scannerHorizonReturn(price, history, 63)
		candidate.Return6M = scannerHorizonReturn(price, history, 126)
		outcome := result.outcomes[candidate.Ticker]
		outcome.candidate = candidate
		outcome.known = true
		if fundamentals.MarketCapitalization < pullbackMinMarketCapMM {
			outcome.stage = "market_cap"
			outcome.reason = fmt.Sprintf("%.2fM below %.2fM minimum",
				fundamentals.MarketCapitalization, pullbackMinMarketCapMM)
			result.outcomes[candidate.Ticker] = outcome
			continue
		}
		if dollarVolume < pullbackMinDollarVolume {
			outcome.stage = "dollar_volume"
			outcome.reason = fmt.Sprintf("%.2f below %.2f minimum",
				dollarVolume, pullbackMinDollarVolume)
			result.outcomes[candidate.Ticker] = outcome
			continue
		}
		if maxScannerReturn(candidate) < minMomentumReturn {
			outcome.stage = "momentum"
			outcome.reason = fmt.Sprintf("best horizon %.2f%% below %.2f%% minimum",
				maxScannerReturn(candidate), minMomentumReturn)
			result.outcomes[candidate.Ticker] = outcome
			continue
		}
		outcome.stage = "momentum_eligible"
		result.outcomes[candidate.Ticker] = outcome
		qualified = append(qualified, candidate)
	}
	for i := 1; i < len(qualified); i++ {
		for j := i; j > 0 && momentumComposite(qualified[j]) > momentumComposite(qualified[j-1]); j-- {
			qualified[j], qualified[j-1] = qualified[j-1], qualified[j]
		}
	}
	keep := (len(qualified) + 3) / 4
	for i, candidate := range qualified {
		outcome := result.outcomes[candidate.Ticker]
		outcome.rank = i + 1
		if i < keep {
			outcome.stage = "qualified"
			outcome.qualified = true
		} else {
			outcome.stage = "below_top_quartile"
			outcome.reason = fmt.Sprintf("momentum rank %d of %d", i+1, len(qualified))
		}
		result.outcomes[candidate.Ticker] = outcome
	}
	if keep < len(qualified) {
		qualified = qualified[:keep]
	}
	slog.Info("scanner: pullback universe qualified",
		"input", len(candidates), "qualified_top_quartile", len(qualified))
	result.qualified = qualified
	return result
}

func scannerHorizonReturn(price float64, history []data.Candle, sessions int) float64 {
	// FetchHistory includes the current daily bar, so a 21-session return uses
	// index len-1-21 (not len-21, which would be only 20 intervals).
	if len(history) <= sessions {
		return -100
	}
	base := history[len(history)-1-sessions].Close
	if price <= 0 || base <= 0 {
		return -100
	}
	return (price/base - 1) * 100
}

func maxScannerReturn(c SwingCandidate) float64 {
	v := c.Return1M
	if c.Return3M > v {
		v = c.Return3M
	}
	if c.Return6M > v {
		v = c.Return6M
	}
	return v
}

func momentumComposite(c SwingCandidate) float64 {
	return (c.Return1M + c.Return3M + c.Return6M) / 3
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
func (s *MarketScanner) buildTelegramMessage(added []scoredCandidate, expired int, rejectionSets ...[]PromotionRejection) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🔍 *Scanner added %d ticker(s) to watchlist*\n\n", len(added)))
	for _, sc := range added {
		sb.WriteString(fmt.Sprintf("• *%s* — %s (conviction: %.0f%%)\n",
			sc.candidate.Ticker, sc.score.SetupType, sc.score.Conviction*100))
		if sc.candidate.GraphContext != "" {
			sb.WriteString(fmt.Sprintf("  🔗 _%s_\n", sc.candidate.GraphContext))
		}
		if sc.candidate.WeeklyLeaderEvidence != "" {
			sb.WriteString(fmt.Sprintf(
				"  📈 _Weekly leader: %s_\n", sc.candidate.WeeklyLeaderEvidence,
			))
		}
		if sc.candidate.PotentIndustryEvidence != "" {
			sb.WriteString(fmt.Sprintf(
				"  🧭 _Potent industry: %s_\n",
				sc.candidate.PotentIndustryEvidence,
			))
		}
		if sc.candidate.PremarketEpisodicEvidence != "" {
			sb.WriteString(fmt.Sprintf(
				"  🌅 _Premarket episodic: %s_\n",
				sc.candidate.PremarketEpisodicEvidence,
			))
		}
		if sc.score.Reasoning != "" {
			sb.WriteString(fmt.Sprintf("  _%s_\n", sc.score.Reasoning))
		}
	}
	if expired > 0 {
		sb.WriteString(fmt.Sprintf("\n_Expired %d stale scanner item(s) from watchlist._", expired))
	}
	if len(rejectionSets) > 0 && len(rejectionSets[0]) > 0 {
		correlationRejections := 0
		for _, rejection := range rejectionSets[0] {
			if rejection.Reason == "correlation_cap" || rejection.Reason == "exposure_correlation_cap" {
				correlationRejections++
			}
		}
		if correlationRejections > 0 {
			sb.WriteString(fmt.Sprintf("\n_Correlation/exposure filter retained %d rejected alternative(s) for audit._", correlationRejections))
		}
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

func (s *MarketScanner) candidateTTLDays(candidate SwingCandidate) int {
	// Discovery-source TTLs are explicit policy for those sources. For a
	// multi-source candidate, retain it for the longest contributing source;
	// ordinary scanner TTL applies only when no source-specific policy exists.
	days := 0
	if hasSource(candidate.Sources, "weekly_leader") && s.Cfg.WeeklyLeader.TTLDays > days {
		days = s.Cfg.WeeklyLeader.TTLDays
	}
	if hasSource(candidate.Sources, "potent_industry") && s.Cfg.PotentIndustry.TTLDays > days {
		days = s.Cfg.PotentIndustry.TTLDays
	}
	if days > 0 {
		return days
	}
	return s.ttlDays()
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
	candidate            SwingCandidate
	score                SwingScoreResult
	correlations         map[string]float64
	exposureCorrelations map[string]float64
	// correlationUnavailable records per-symbol provider failures. The
	// correlation selector intentionally fails open for these symbols, but the
	// omission remains visible in the persisted scanner evidence.
	correlationUnavailable map[string]string
}

// sortByConviction sorts in-place, highest conviction first.
func sortByConviction(items []scoredCandidate) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].score.Conviction > items[j-1].score.Conviction; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
