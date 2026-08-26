// Package scanner implements deterministic, source-aware market discovery.
//
// The scanner runs on a configurable interval (default: every 2 hours) during
// market hours only. Each scan cycle:
//  1. Fetches top movers (gainers/losers) and most-active stocks from Alpaca.
//  2. Deterministically qualifies and ranks each source's candidates.
//  3. Deduplicates source evidence, then allocates bounded research capacity.
//  4. Adds qualified candidates to the watchlist (source="scanner") with an
//     expiry timestamp; the normal analyst ensemble evaluates them later.
//  5. Expires stale scanner-added items that have passed their TTL.
//  6. Publishes committed watchlist admission and rediscovery events.
package scanner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/depth"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
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

// MarketScanner discovers swing-trade candidates and manages a time-bounded
// watchlist of scanner-sourced tickers.
type MarketScanner struct {
	AlpacaClient    *data.AlpacaClient
	StockAssets     data.AssetUniverseProvider
	StockSnapshots  data.BulkStockSnapshotProvider
	Analyzer        *analysis.Analyzer
	News            data.NewsProvider
	Fundamentals    CompanyFundamentalsProvider
	DailyBars       data.BulkDailyBarsProvider // optional; required only for correlation-aware promotion
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
	var scanCandidatesEvaluated, scanItemsAdded, scanItemsExpired int
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
	recorder := newScanRecorder(s, start)
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
	// Start the audit run before any terminal source decision. The source
	// claims below then carry this run ID even if validation exits early.
	recorder.register(weeklyCandidates)
	recorder.register(industryCandidates)
	recorder.register(premarketCandidates)
	if err := recorder.begin(ctx); err != nil {
		return err
	}
	defer func() { recorder.finish(scanErr) }()
	if len(weeklyCandidates) > 0 && len(industryCandidates) > 0 &&
		weeklyCandidates[0].UniverseVersionID != industryCandidates[0].UniverseVersionID {
		return fmt.Errorf(
			"discovery source universe mismatch: weekly leader=%s potent industry=%s",
			weeklyCandidates[0].UniverseVersionID,
			industryCandidates[0].UniverseVersionID,
		)
	}
	politicalCandidates, err := s.politicalDisclosureCandidates(ctx, recorder)
	if err != nil {
		return fmt.Errorf("load political disclosure candidates: %w", err)
	}
	defer s.releasePoliticalDisclosureClaims(ctx, politicalCandidates)
	scanID := recorder.run.ID

	if s.Bus != nil {
		s.Bus.Publish(events.ScanStarted, events.ScanStartedPayload{ScanID: scanID})
		defer func() {
			s.Bus.Publish(events.ScanCompleted, events.ScanCompletedPayload{
				ScanID:              scanID,
				CandidatesEvaluated: scanCandidatesEvaluated,
				ItemsAdded:          scanItemsAdded,
				ItemsExpired:        scanItemsExpired,
				DurationMs:          time.Since(start).Milliseconds(),
				Failed:              scanErr != nil,
			})
		}()
	}

	slog.Info("scanner: scan started")

	// 1. Expire stale scanner items before adding new ones.
	expired, err := store.ExpireScannedItems(ctx)
	if err != nil {
		slog.Warn("scanner: expire stale items", "err", err)
	}
	recorder.run.ExpiredItemCount = len(expired)
	scanItemsExpired = len(expired)

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
	// Collapse ordinary/graph/sympathy overlaps before sources carrying durable
	// claim lineage are merged below.
	candidates = deduplicateScannerCandidates(candidates)
	candidates = mergeWeeklyLeaderCandidates(candidates, weeklyCandidates)
	candidates = mergePotentIndustryCandidates(candidates, industryCandidates)
	candidates = mergePremarketEpisodicCandidates(candidates, premarketCandidates)
	candidates = mergePoliticalDisclosureCandidates(candidates, politicalCandidates)
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

	// An active ticker is not a fresh promotion candidate, but it may carry
	// genuinely new source evidence. Refresh its source lease before the normal
	// active-watchlist exclusion and before cap accounting. This keeps a full
	// watchlist renewable without allowing renewals to consume a promotion slot
	// or replace its admission-time conviction/execution profile.
	//
	// News is part of ordinary mover/activity evidence, so active refreshes must
	// see the same bounded news view as new admissions. Restrict this early pass
	// to actual active scanner rows: when the cap is full, enriching every fresh
	// candidate would spend provider calls even though none can be admitted.
	candidates, err = s.enrichActiveRefreshCandidates(ctx, candidates)
	if err != nil {
		return fmt.Errorf("enrich active watchlist refresh candidates: %w", err)
	}
	handledRefreshes, err := s.refreshActiveSourceLeases(ctx, candidates, recorder.run.ID, cycleLease)
	if err != nil {
		return fmt.Errorf("refresh active watchlist source leases: %w", err)
	}
	if len(handledRefreshes) > 0 {
		beforeRefreshFilter := candidates
		filtered := candidates[:0]
		for _, candidate := range candidates {
			if !handledRefreshes[normalizePromotionKey(candidate.Ticker)] {
				filtered = append(filtered, candidate)
			}
		}
		candidates = filtered
		recorder.markRemoved(beforeRefreshFilter, candidates, "active_watchlist")
	}
	// A ticker that lost active status while its refresh was in flight can have
	// both ordinary and graph/sympathy candidates in this pool. Admit it once,
	// retaining the combined source attribution for its normal scoring path.

	// 3.5 Dedup against active watchlist/position exposure only after universe
	// selection. Correlation mode takes one normalized exposure snapshot and
	// reuses it for both same-ticker guards and pairwise selection.
	var exposure promotionExposure
	if s.Cfg.PromotionCorrelationEnabled && store.IsConnected() {
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
		candidates = filterActiveCandidates(candidates, exposure.admissionGuard)
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

	// 4. Enrich new admissions after active-row accounting. The deterministic
	// source rules below decide research qualification; the shared store-level
	// research queue decides capacity atomically with the watchlist write.
	// Active refresh candidates
	// were enriched above and need not be included here because they were removed
	// by the active-watchlist filter.
	since := s.now().Add(-time.Duration(s.Cfg.NewsLookbackHours) * time.Hour)
	candidates = s.enrichWithNews(ctx, candidates, since)
	recorder.setEligible(candidates)

	// 5. Every candidate reaching this point has already passed its source's
	// deterministic qualification. The old generic LLM add/skip request was a
	// second analyst and could veto a valid research candidate; it is removed.
	// Keep a local deterministic score only for stable within-run ordering.
	scored := make([]scoredCandidate, 0, len(candidates))
	for _, c := range candidates {
		scanCandidatesEvaluated++
		scored = append(scored, scoredCandidate{candidate: c,
			score: SwingScoreResult{Conviction: deterministicAdmissionScore(c)}})
		recorder.deterministicQualification(c, researchQueueAdmission(c, s.Cfg.ResearchQueue))
		if snapshot := recorder.candidates[c.Ticker]; snapshot != nil {
			snapshot.FinalStage = "deterministically_qualified"
		}
	}

	// 6. Sort within each source bucket, then use that normalized rank to
	// arbitrate shared overflow. Raw source scores are intentionally on
	// incomparable scales (political freshness, market dollars, and normalized
	// source scores), so using them globally would bias overflow by score units.
	// The database queue allocator remains the sole shared admission limit.
	sortByResearchQueuePriority(scored, s.Cfg.ResearchQueue)
	recorder.selected(scored)

	// 8. Add to watchlist and notify.
	for index := 0; index < len(scored); {
		if err := ctx.Err(); err != nil {
			return err
		}
		sc := scored[index]
		now := s.now()
		expiresAt := now.AddDate(0, 0, s.candidateTTLDays(sc.candidate))
		timeframes := []string{"1H", "4H"}
		discoveryContext, executionProfile, profileErr := buildScannerTradeProfile(
			sc.candidate, SwingScoreResult{},
		)
		if profileErr != nil {
			return fmt.Errorf("build scanner trade profile for %s: %w", sc.candidate.Ticker, profileErr)
		}
		strategies := scannerCandidateStrategies(sc.candidate, SwingScoreResult{
			Strategy: executionProfile.RecommendedLens,
		})
		if err := recorder.setTradeProfile(
			sc.candidate, discoveryContext, executionProfile,
		); err != nil {
			return fmt.Errorf("record scanner trade profile for %s: %w", sc.candidate.Ticker, err)
		}
		leases := s.scannerSourceLeases(sc.candidate, discoveryContext, now)
		if len(leases) > 0 {
			// The row expiration must be derived from the same leases that
			// govern retention. A scanner-wide fallback here could keep a
			// political-only notification alive after its shorter source lease.
			expiresAt = maxSourceLeaseExpiry(leases, leases[0].ExpiresAt)
		}
		item := store.WatchlistItem{
			Ticker:     sc.candidate.Ticker,
			Platform:   "stock",
			Strategies: strategies,
			Timeframes: timeframes,
			// Deterministic admission priority is audit metadata only.
			MinConfidence: 0,
			Source:        "scanner",
			// Persist WHY this candidate was sourced (graph link / sympathy
			// trigger) so the engine's deep-analysis prompt carries the origin
			// thesis instead of losing it after this pass.
			GraphContext:                    sc.candidate.GraphContext,
			DiscoveryContext:                discoveryContext,
			ExecutionProfile:                executionProfile,
			ScannerRunID:                    recorder.run.ID,
			ExpiresAt:                       &expiresAt,
			SourceLeases:                    leases,
			ResearchQueue:                   researchQueueAdmission(sc.candidate, s.Cfg.ResearchQueue),
			PullbackRequalificationComplete: sc.candidate.PullbackQualificationKnown,
		}
		if err := s.renewScannerCycleBeforePromotion(ctx, cycleKey, cycleOwner, interval); err != nil {
			return err
		}
		changedLeases, addErr := s.promoteDiscoveryCandidate(ctx, item, sc.candidate, cycleLease)
		if errors.Is(addErr, store.ErrScannerSourcePromotionClaimLost) {
			var claimErr *store.ScannerSourcePromotionClaimError
			source := ""
			if errors.As(addErr, &claimErr) {
				source = claimErr.Source
			}
			fallback, ok := candidateAfterPromotionClaimLoss(sc.candidate, source)
			if ok {
				// Losing a source claim can move the ticker into another bucket. Put
				// that fallback back among the unattempted candidates and normalize
				// its rank before it can consume the new bucket's reservation/overflow.
				scanCandidatesEvaluated++
				fallbackScore := SwingScoreResult{Conviction: deterministicAdmissionScore(fallback)}
				recorder.deterministicFallback(fallback)
				recorder.deterministicQualification(fallback, researchQueueAdmission(fallback, s.Cfg.ResearchQueue))
				scored = requeueFallbackCandidate(scored, index, scoredCandidate{
					candidate: fallback, score: fallbackScore,
				}, s.Cfg.ResearchQueue)
				continue
			}
		}
		if errors.Is(addErr, store.ErrScannerCycleLeaseLost) {
			return addErr
		}
		if errors.Is(addErr, store.ErrWatchlistActiveItemExists) {
			if len(changedLeases) > 0 && s.Bus != nil {
				s.Bus.Publish(events.ScannerItemRediscovered, scannerRediscoveryPayload(
					scanID, sc.candidate.Ticker, changedLeases,
				))
			}
			recorder.watchlistResult(sc.candidate, strategies, addErr)
			slog.Info(
				"scanner: source fallback lost to active watchlist item",
				"ticker", sc.candidate.Ticker,
			)
			index++
			continue
		}
		if addErr != nil {
			recorder.watchlistResult(sc.candidate, strategies, addErr)
			if errors.Is(addErr, store.ErrResearchQueueCapacity) {
				if auditErr := store.RecordResearchQueueSuppression(ctx, item, *item.ResearchQueue); auditErr != nil {
					slog.Warn("scanner: failed to record research queue suppression", "ticker", sc.candidate.Ticker, "err", auditErr)
				}
				// Leave upstream source claims in processing. The cycle's deferred
				// release returns them to pending so a future free protected slot can
				// admit the same qualified evidence; capacity is not a rejection.
				recorder.researchQueueSuppressed(sc.candidate.Ticker, item.ResearchQueue)
			} else if !errors.Is(addErr, store.ErrScannerSourcePromotionClaimLost) {
				// Watchlist persistence failures are transient and must not
				// terminalize discovery evidence. Each source owns its retry
				// transition; political disclosures requeue this candidate's
				// exact report IDs below, without releasing sibling claims that
				// share the batch token.
				s.markDiscoveryCandidate(
					ctx, sc.candidate, "failed", recorder.run.ID, addErr.Error(),
				)
			}
			slog.Warn("scanner: failed to add watchlist item",
				"ticker", sc.candidate.Ticker, "err", addErr)
			index++
			continue
		}
		// Only candidates that have actually been admitted to the watchlist
		// may consume the industry cap for later fallback decisions. Keeping
		// future candidates inactive prevents a lower-ranked, not-yet-attempted
		// peer from suppressing a higher-ranked fallback.
		recorder.watchlistResult(sc.candidate, strategies, nil)
		bucketPriority := deterministicBucketAdmissionScore(sc.candidate, item.ResearchQueue.Bucket)
		slog.Info("scanner: added to research queue", "ticker", sc.candidate.Ticker,
			"priority", bucketPriority, "bucket", item.ResearchQueue.Bucket)
		scanItemsAdded++

		if s.Bus != nil {
			s.Bus.Publish(events.ScannerItemAdded, events.ScannerItemAddedPayload{
				ScanID:                      scanID,
				Ticker:                      sc.candidate.Ticker,
				Conviction:                  0,
				SetupType:                   "deterministic_research_admission",
				Strategy:                    "",
				Timeframes:                  timeframes,
				Reasoning:                   "deterministic source qualification",
				ExpiresAt:                   expiresAt.Format(time.RFC3339),
				Sources:                     sc.candidate.Sources,
				GraphContext:                sc.candidate.GraphContext,
				WeeklyLeaderEvidence:        sc.candidate.WeeklyLeaderEvidence,
				PotentIndustryEvidence:      sc.candidate.PotentIndustryEvidence,
				PremarketEpisodicEvidence:   sc.candidate.PremarketEpisodicEvidence,
				PoliticalDisclosureEvidence: sc.candidate.PoliticalDisclosureEvidence,
				ResearchBucket:              item.ResearchQueue.Bucket,
				DeterministicPriority:       bucketPriority,
				AdmissionReason:             "deterministic source qualification",
			})
		}
		index++
	}

	durationMs := time.Since(start).Milliseconds()

	slog.Info("scanner: scan completed",
		"candidates_evaluated", len(candidates),
		"items_added", scanItemsAdded,
		"items_expired", len(expired),
		"duration_ms", durationMs,
	)

	return nil
}

func deduplicateScannerCandidates(candidates []SwingCandidate) []SwingCandidate {
	byTicker := make(map[string]int, len(candidates))
	out := make([]SwingCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		captureSourceGraphContext(&candidate)
		key := normalizePromotionKey(candidate.Ticker)
		if index, exists := byTicker[key]; exists {
			merged := &out[index]
			captureSourceGraphContext(merged)
			merged.Sources = mergeSources(merged.Sources, candidate.Sources)
			if merged.SourceScores == nil {
				merged.SourceScores = make(map[string]float64)
			}
			for source, score := range candidate.SourceScores {
				merged.SourceScores[source] = score
			}
			if merged.GraphContext == "" {
				merged.GraphContext = candidate.GraphContext
			}
			for source, context := range candidate.sourceGraphContexts {
				if merged.sourceGraphContexts == nil {
					merged.sourceGraphContexts = make(map[string]string)
				}
				merged.sourceGraphContexts[source] = context
			}
			if hasSource(candidate.Sources, "news_sympathy") {
				// A sympathy source owns its seed trigger and relationship. Keep
				// those fields even when an ordinary/graph candidate was first.
				merged.SympathySeed = candidate.SympathySeed
				merged.SympathyRelationship = candidate.SympathyRelationship
				if candidate.SympathyTrigger != nil {
					trigger := *candidate.SympathyTrigger
					merged.SympathyTrigger = &trigger
				} else {
					// Compatibility for an old in-memory candidate that predates the
					// source-owned trigger. Fresh sympathy candidates never place the
					// seed article in the shared ticker-news slice.
					merged.News = append(append([]SwingNewsItem(nil), candidate.News...), merged.News...)
				}
			} else if len(merged.News) == 0 {
				merged.News = append([]SwingNewsItem(nil), candidate.News...)
			}
			continue
		}
		byTicker[key] = len(out)
		out = append(out, candidate)
	}
	return out
}

func captureSourceGraphContext(candidate *SwingCandidate) {
	if candidate.GraphContext == "" {
		return
	}
	for _, source := range candidate.Sources {
		if source != "graph_neighbor" && source != "news_sympathy" {
			continue
		}
		if candidate.sourceGraphContexts == nil {
			candidate.sourceGraphContexts = make(map[string]string)
		}
		candidate.sourceGraphContexts[source] = candidate.GraphContext
	}
}

func (s *MarketScanner) refreshActiveSourceLeases(
	ctx context.Context, candidates []SwingCandidate, scannerRunID string, cycleLease *store.ScannerCycleLease,
) (map[string]bool, error) {
	items, err := store.GetWatchlistItems(ctx, true)
	if err != nil {
		return nil, err
	}
	active := make(map[string]struct{}, len(items))
	handled := make(map[string]bool)
	for _, item := range items {
		if item.Source == "scanner" {
			active[normalizePromotionKey(item.Ticker)] = struct{}{}
		}
	}
	for _, candidate := range candidates {
		if _, ok := active[normalizePromotionKey(candidate.Ticker)]; !ok {
			continue
		}
		refreshCandidate := candidate
		for {
			// The profile is validation scaffolding only. The store preserves the
			// original one on an active-row refresh; it is never re-scored here.
			discovery, profile, err := buildScannerTradeProfile(refreshCandidate, SwingScoreResult{})
			if err != nil {
				return nil, fmt.Errorf("build refresh profile for %s: %w", refreshCandidate.Ticker, err)
			}
			now := s.now()
			leases := s.scannerSourceLeases(refreshCandidate, discovery, now)
			expiresAt := now.AddDate(0, 0, s.candidateTTLDays(refreshCandidate))
			if len(leases) > 0 {
				expiresAt = maxSourceLeaseExpiry(leases, leases[0].ExpiresAt)
			}
			item := store.WatchlistItem{
				Ticker: refreshCandidate.Ticker, Platform: "stock", Source: "scanner",
				Strategies: scannerCandidateStrategies(refreshCandidate, SwingScoreResult{Strategy: profile.RecommendedLens}),
				Timeframes: []string{"1H", "4H"}, DiscoveryContext: discovery,
				ExecutionProfile: profile, ScannerRunID: scannerRunID, ExpiresAt: &expiresAt,
				GraphContext:                    refreshCandidate.GraphContext,
				SourceLeases:                    leases,
				PullbackRequalificationComplete: refreshCandidate.PullbackQualificationKnown,
			}
			claims := scannerSourcePromotionClaims(refreshCandidate)
			var changedLeases []store.WatchlistSourceLease
			if claims.WeeklyLeader != nil || claims.PotentIndustry != nil ||
				claims.PremarketEpisodic != nil || claims.PoliticalDisclosure != nil {
				changedLeases, err = store.RefreshScannerSourcesOnActiveWatchlistWithChangedLeases(
					ctx, item, claims, "meaningful scanner evidence refreshed an active watchlist lease", now, cycleLease,
				)
			} else {
				changedLeases, err = store.RefreshActiveScannerWatchlistItemWithChangedLeases(ctx, item, cycleLease)
			}
			if errors.Is(err, store.ErrWatchlistRefreshNotActive) {
				// It will be handled by normal scoring if it is not active by the
				// later exposure filter; do not turn this refresh into re-admission.
				break
			}
			if errors.Is(err, store.ErrScannerSourcePromotionClaimLost) {
				var claimErr *store.ScannerSourcePromotionClaimError
				if errors.As(err, &claimErr) {
					if fallback, ok := candidateAfterPromotionClaimLoss(refreshCandidate, claimErr.Source); ok {
						refreshCandidate = fallback
						continue
					}
				}
				// No safe reduced provenance exists (for example, premarket data
				// cannot be reconstructed). Keep the remaining claims retryable.
				handled[normalizePromotionKey(candidate.Ticker)] = true
				slog.Info("scanner: active source refresh lost stale claim", "ticker", refreshCandidate.Ticker)
				break
			}
			if err != nil {
				return nil, fmt.Errorf("refresh %s: %w", refreshCandidate.Ticker, err)
			}
			if len(changedLeases) > 0 && s.Bus != nil {
				s.Bus.Publish(events.ScannerItemRediscovered, scannerRediscoveryPayload(
					scannerRunID, refreshCandidate.Ticker, changedLeases,
				))
			}
			handled[normalizePromotionKey(candidate.Ticker)] = true
			break
		}
	}
	return handled, nil
}

func scannerRediscoveryPayload(
	scanID, ticker string, leases []store.WatchlistSourceLease,
) events.ScannerItemRediscoveredPayload {
	changed := make([]events.ScannerSourceLeaseChangePayload, 0, len(leases))
	for _, lease := range leases {
		changed = append(changed, events.ScannerSourceLeaseChangePayload{
			Source:      lease.Source,
			QualifiedAt: lease.QualifiedAt.UTC().Format(time.RFC3339),
			ObservedAt:  lease.ObservedAt.UTC().Format(time.RFC3339),
			EffectiveAt: lease.EffectiveAt.UTC().Format(time.RFC3339),
			ExpiresAt:   lease.ExpiresAt.UTC().Format(time.RFC3339),
			Evidence:    append([]byte(nil), lease.Evidence...),
		})
	}
	return events.ScannerItemRediscoveredPayload{
		ScanID: scanID, Ticker: ticker, ChangedSources: changed,
	}
}

func (s *MarketScanner) enrichActiveRefreshCandidates(
	ctx context.Context, candidates []SwingCandidate,
) ([]SwingCandidate, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}
	items, err := store.GetWatchlistItems(ctx, true)
	if err != nil {
		return nil, err
	}
	active := make(map[string]store.WatchlistItem, len(items))
	for _, item := range items {
		if item.Source == "scanner" {
			active[normalizePromotionKey(item.Ticker)] = item
		}
	}
	refresh := make([]SwingCandidate, 0, len(active))
	refreshIndexes := make([]int, 0, len(active))
	for index, candidate := range candidates {
		if _, ok := active[normalizePromotionKey(candidate.Ticker)]; ok {
			refresh = append(refresh, candidate)
			refreshIndexes = append(refreshIndexes, index)
		}
	}
	if len(refresh) == 0 {
		return candidates, nil
	}
	for index := range refresh {
		if s.News == nil {
			restoreActiveRefreshNews(&refresh[index], active[normalizePromotionKey(refresh[index].Ticker)])
			continue
		}
		rawNews, err := s.News.FetchNews(ctx, refresh[index].Ticker,
			s.now().Add(-time.Duration(s.Cfg.NewsLookbackHours)*time.Hour))
		if err != nil {
			// A transient provider failure is not fresh "no news" evidence. Keep
			// the persisted source view so it cannot alter a lease fingerprint or
			// erase analyst provenance.
			// The immutable sympathy trigger is held separately. Preserve the
			// persisted ticker-specific news on transient provider failures so
			// ordinary leases cannot churn because enrichment was unavailable.
			restoreActiveRefreshNews(&refresh[index], active[normalizePromotionKey(refresh[index].Ticker)])
			continue
		}
		if len(rawNews) > maxNewsPerTicker {
			rawNews = rawNews[:maxNewsPerTicker]
		}
		refresh[index].News = refresh[index].News[:0]
		for _, news := range rawNews {
			sentiment := "NEUTRAL"
			if news.SentimentScore > sentimentPositiveThreshold {
				sentiment = "POSITIVE"
			} else if news.SentimentScore < sentimentNegativeThreshold {
				sentiment = "NEGATIVE"
			}
			refresh[index].News = append(refresh[index].News, SwingNewsItem{
				Headline: news.Headline, Sentiment: sentiment, PublishedAt: news.PublishedAt,
			})
		}
	}
	out := append([]SwingCandidate(nil), candidates...)
	for index, candidate := range refresh {
		out[refreshIndexes[index]] = candidate
	}
	return out, nil
}

func restoreActiveRefreshNews(candidate *SwingCandidate, item store.WatchlistItem) {
	candidate.News = scannerDiscoveryNews(item.DiscoveryContext)
}

func scannerDiscoveryNews(discovery *tradeprofile.DiscoveryContext) []SwingNewsItem {
	if discovery == nil {
		return nil
	}
	for _, source := range discovery.Sources {
		if source == "news_sympathy" {
			continue
		}
		var evidence struct {
			RecentNews []SwingNewsItem `json:"recent_news"`
		}
		if json.Unmarshal(discovery.EvidenceBySource[source], &evidence) == nil && len(evidence.RecentNews) > 0 {
			return append([]SwingNewsItem(nil), evidence.RecentNews...)
		}
	}
	return nil
}

func scannerNewsSentiment(score float64) string {
	if score > sentimentPositiveThreshold {
		return "POSITIVE"
	}
	if score < sentimentNegativeThreshold {
		return "NEGATIVE"
	}
	return "NEUTRAL"
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

	// Top movers (gainers + losers) must be tradeable and liquid enough for
	// analyst research. These objective floors replace the retired LLM veto.
	minPrice := s.Cfg.Universe.MinPrice
	if minPrice <= 0 {
		minPrice = 5
	}
	movers, err := data.FetchGapMovers(ctx, s.AlpacaClient, data.GapScreen{
		// FetchGapMovers only has IEX snapshot volume. It must not influence a
		// hard cutoff either as a floor or a rank: IEX is partial and can
		// undercount genuinely liquid names, especially near the open. Keep the
		// screener's native gap order until consolidated liquidity is available.
		MinGapPct: 0, MinPrice: minPrice,
		RankByLiquidity: false,
	})
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
			// IEX premarket volume is partial: retain it as observation only, never
			// as a queue-admission liquidity rank.
			DollarVolume: 0,
			Sources:      []string{"mover"},
			SourceScores: map[string]float64{"mover": normalizedProviderRank(i)},
			sourceMarketObservations: map[string]map[string]any{
				"mover": moverMarketObservation(m.PreMarketLast, m.GapPct, m.PreMarketVol),
			},
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
	eligibleActiveRank := 0
	for _, a := range actives {
		if a.Price < minPrice || a.Volume <= 0 {
			continue
		}
		activeRank := eligibleActiveRank
		eligibleActiveRank++
		if existing, ok := byTicker[a.Ticker]; ok {
			existing.Sources = append(existing.Sources, "active")
			if existing.sourceMarketObservations == nil {
				existing.sourceMarketObservations = make(map[string]map[string]any)
			}
			existing.sourceMarketObservations["active"] = activeMarketObservation(a.Price, a.Volume, a.TradeCount)
			if a.Volume > existing.Volume {
				existing.Volume = a.Volume
			}
			if existing.Price == 0 {
				existing.Price = a.Price
			}
			if existing.SourceScores == nil {
				existing.SourceScores = make(map[string]float64)
			}
			existing.SourceScores["active"] = normalizedProviderRank(activeRank)
		} else {
			byTicker[a.Ticker] = &SwingCandidate{
				Ticker:     a.Ticker,
				Price:      a.Price,
				Volume:     a.Volume,
				TradeCount: a.TradeCount,
				// The screener exposes current-session activity, not completed-session
				// median liquidity. Keep it in source observations, never as a
				// completed-liquidity floor or research-queue ranking value.
				DollarVolume: 0,
				Sources:      []string{"active"},
				SourceScores: map[string]float64{"active": normalizedProviderRank(activeRank)},
				sourceMarketObservations: map[string]map[string]any{
					"active": activeMarketObservation(a.Price, a.Volume, a.TradeCount),
				},
			}
		}
	}

	candidates := make([]SwingCandidate, 0, len(byTicker))
	for _, c := range byTicker {
		candidates = append(candidates, *c)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Ticker < candidates[j].Ticker })
	return candidates, nil
}

func normalizedProviderRank(index int) float64 {
	if index < 0 {
		index = 0
	}
	return 1 / float64(index+1)
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
	visitCandidateLineage(candidate, func(item *SwingCandidate) { applyUniverseTaxonomy(item, member) })
}

func excludeUniverseTaxonomyFromPromotion(candidate *SwingCandidate) {
	if candidate == nil {
		return
	}
	visitCandidateLineage(candidate, func(item *SwingCandidate) { item.promotionTaxonomyExcluded = true })
}

func visitCandidateLineage(candidate *SwingCandidate, visit func(*SwingCandidate)) {
	seen := make(map[*SwingCandidate]struct{})
	var walk func(*SwingCandidate)
	walk = func(item *SwingCandidate) {
		if item == nil {
			return
		}
		if _, exists := seen[item]; exists {
			return
		}
		seen[item] = struct{}{}
		visit(item)
		walk(item.weeklyLeaderFallback)
		walk(item.potentIndustryFallback)
		walk(item.potentIndustrySource)
		walk(item.politicalDisclosureFallback)
		walk(item.politicalDisclosureSource)
	}
	walk(candidate)
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
	now := s.now()
	for _, item := range items {
		if item.Source == "earnings_watchlist" {
			// Earnings is another scanner source. Let qualified scanner evidence
			// attach to its active row; addWatchlistItem retains its context and
			// the queue allocator recognizes the existing membership.
			continue
		}
		// An additive context can keep an expired scanner row visible for engine
		// analysis. It is no longer scanner-resident, so fresh scanner evidence
		// must proceed through ordinary scoring and cap admission.
		if expiredScannerResidency(item, now) {
			continue
		}
		if key := normalizePromotionKey(item.Ticker); key != "" {
			existing[key] = struct{}{}
		}
	}

	return filterActiveCandidates(candidates, existing), nil
}

func expiredScannerResidency(item store.WatchlistItem, now time.Time) bool {
	if !item.ScannerOrigin {
		return false
	}
	// An earnings context may take over an expired scanner row and intentionally
	// clear the row-level expiry. Its durable scanner origin still means fresh
	// scanner evidence must re-enter normal admission rather than be filtered as
	// an ordinary earnings watchlist duplicate.
	if item.Source == "earnings_watchlist" && item.ExpiresAt == nil {
		return true
	}
	return item.ExpiresAt != nil && !item.ExpiresAt.After(now)
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
		for j := i; j > 0 && (items[j].score.Conviction > items[j-1].score.Conviction ||
			(items[j].score.Conviction == items[j-1].score.Conviction && items[j].candidate.Ticker < items[j-1].candidate.Ticker)); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func scannerCandidateStrategies(candidate SwingCandidate, score SwingScoreResult) []string {
	primary := strings.ToLower(strings.TrimSpace(score.Strategy))
	if primary == "" {
		// Deterministic admissions no longer obtain an LLM strategy. Persist the
		// same source-owned default that the execution profile recommends so
		// legacy consumers retain a meaningful primary strategy.
		primary = scannerDefaultsForSources(candidate.Sources).lenses[0]
	}
	// pullback_strength is a qualified execution path, never a generic scanner
	// score. The profile also removes this lens for an unqualified candidate,
	// but the legacy strategies list independently drives engine admission and
	// must carry the same contract.
	if !candidate.PullbackQualified && strings.EqualFold(strings.TrimSpace(primary), "pullback_strength") {
		primary = "momentum"
	}
	strategies := candidateStrategies(primary, candidate.PullbackQualified)
	for _, source := range []string{"weekly_leader", "potent_industry", "premarket_episodic"} {
		if hasSource(candidate.Sources, source) {
			strategies = appendUniqueStrategy(strategies, source)
		}
	}
	return strategies
}
