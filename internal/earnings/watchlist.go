package earnings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

type MarketCalendarFetcher interface {
	FetchMarketEarningsCalendar(context.Context, time.Time, time.Time) ([]data.EarningsRelease, error)
}

// WatchlistFeeder selects liquid, next-session earnings names and attaches an
// independently expiring context to the ordinary watchlist funnel.
type WatchlistFeeder struct {
	Calendar           MarketCalendarFetcher
	Bars               data.BulkDailyBarsProvider
	Cfg                config.EarningsWatchlistConfig
	ResearchQueue      config.ResearchQueueConfig
	ResearchStaging    config.ResearchStagingConfig
	MaxActiveWatchlist int
	Now                func() time.Time
	Bus                interface{ Publish(events.Type, any) }
}

type rankedRelease struct {
	release data.EarningsRelease
	dollar  float64
}

func (f *WatchlistFeeder) RunSession(ctx context.Context, completed engine.USMarketSession, decisionAt time.Time) (err error) {
	target := engine.NextMarketSession(completed.Date)
	attemptID := ""
	stage := "validate_dependencies"
	defer func() {
		if err == nil || ctx.Err() != nil {
			return
		}
		args := []any{
			"completed_session_date", completed.Date.Format("2006-01-02"),
			"target_session_date", target.Date.Format("2006-01-02"),
			"stage", stage,
			"err", err,
		}
		if attemptID != "" {
			args = append(args, "attempt_id", attemptID)
		}
		slog.Error("earnings watchlist run failed", args...)
		if attemptID != "" {
			if statusErr := store.FailEarningsWatchlistRun(ctx, target.Date, attemptID, err); statusErr != nil {
				slog.Warn("earnings watchlist failed to persist durable failure status",
					"target_session_date", target.Date.Format("2006-01-02"),
					"attempt_id", attemptID,
					"err", statusErr,
				)
			}
		}
	}()
	if f.Calendar == nil || f.Bars == nil {
		return fmt.Errorf("earnings watchlist requires calendar and daily-bars providers")
	}
	now := f.now()
	slog.Info("earnings watchlist run targeting session",
		"completed_session_date", completed.Date.Format("2006-01-02"),
		"target_session_date", target.Date.Format("2006-01-02"),
		"target_open", target.Open,
		"decision_at", decisionAt,
		"now", now,
	)
	stage = "expire_contexts"
	if err := store.ExpireWatchlistDiscoveryContexts(ctx); err != nil {
		return fmt.Errorf("expire discovery contexts: %w", err)
	}
	now = f.now()
	if !now.Before(target.Open) {
		// Startup during the target session must not pre-publish tomorrow with
		// yesterday's ranking data. The ordinary after-close run will use the
		// completed session that actually precedes tomorrow.
		slog.Info("earnings watchlist run skipped",
			"reason", "target_session_started",
			"completed_session_date", completed.Date.Format("2006-01-02"),
			"target_session_date", target.Date.Format("2006-01-02"),
		)
		return nil
	}
	stage = "claim"
	attemptID, claimed, err := store.ClaimEarningsWatchlistRun(ctx, target.Date, now)
	if err != nil {
		return err
	}
	if !claimed {
		stage = "lease"
		leaseHeld, availabilityErr := store.EarningsWatchlistRunLeaseHeld(ctx, target.Date, now)
		if availabilityErr != nil {
			return availabilityErr
		}
		if leaseHeld {
			slog.Info("earnings watchlist run skipped",
				"reason", "another_worker",
				"target_session_date", target.Date.Format("2006-01-02"),
			)
			return store.ErrEarningsWatchlistRunLeased
		}
		slog.Info("earnings watchlist run reused durable result",
			"target_session_date", target.Date.Format("2006-01-02"),
		)
		return nil
	}
	slog.Info("earnings watchlist run claimed",
		"target_session_date", target.Date.Format("2006-01-02"),
		"attempt_id", attemptID,
	)
	watchlistSessions := f.Cfg.WatchlistSessions
	if watchlistSessions <= 0 {
		watchlistSessions = 3
	}
	expires := engine.MarketSessionOffset(target.Date, watchlistSessions-1).Close
	stage = "load_cohort"
	candidates, hasCohort, err := store.LoadEarningsWatchlistCohort(ctx, target.Date)
	if err != nil {
		return fmt.Errorf("load persisted earnings candidate cohort: %w", err)
	}
	if !hasCohort {
		stage = "calendar"
		releases, err := f.Calendar.FetchMarketEarningsCalendar(ctx, target.Date, target.Date)
		if err != nil {
			return fmt.Errorf("fetch market earnings calendar for %s: %w", target.Date.Format("2006-01-02"), err)
		}
		unique := nextSessionReleases(releases, target.Date)
		slog.Info("earnings watchlist calendar loaded",
			"target_session_date", target.Date.Format("2006-01-02"),
			"release_count", len(releases),
			"eligible_unique_count", len(unique),
		)
		var selected []store.EarningsWatchlistCandidate
		if len(unique) > 0 {
			symbols := make([]string, 0, len(unique))
			for _, release := range unique {
				symbols = append(symbols, release.Symbol)
			}
			from := completed.Date.AddDate(0, 0, -35)
			to := completed.Date.AddDate(0, 0, 1)
			stage = "bars"
			barResult, err := f.Bars.FetchDailyBars(ctx, symbols, from, to, f.Cfg.BarBatchSize)
			if err != nil {
				return fmt.Errorf("fetch ranking daily bars: %w", err)
			}
			ranked := rankReleases(unique, barResult.Bars)
			cap := f.Cfg.CandidateCap
			if cap > 5 {
				cap = 5
			}
			if cap > len(ranked) {
				cap = len(ranked)
			}
			slog.Info("earnings watchlist candidates ranked",
				"target_session_date", target.Date.Format("2006-01-02"),
				"eligible_unique_count", len(unique),
				"bar_symbol_count", len(barResult.Bars),
				"bar_failure_count", len(barResult.Failures),
				"ranked_count", len(ranked),
				"selected_count", cap,
				"candidate_cap", f.Cfg.CandidateCap,
			)
			selected = make([]store.EarningsWatchlistCandidate, 0, cap)
			for index, candidate := range ranked[:cap] {
				selected = append(selected, store.EarningsWatchlistCandidate{
					Ticker: candidate.release.Symbol, ContextKey: "earnings_scheduled:" + target.Date.Format("2006-01-02"),
					Context: earningsDiscoveryContext(candidate.release, target, index+1, candidate.dollar), ExpiresAt: expires,
					LiquidityRank: index + 1, MedianDollarVolume: candidate.dollar,
				})
			}
		}
		stage = "persist_cohort"
		candidates, err = store.PersistEarningsWatchlistCohort(ctx, target.Date, attemptID, selected)
		if err != nil {
			return fmt.Errorf("persist earnings candidate cohort: %w", err)
		}
	}
	if len(candidates) == 0 {
		stage = "complete_empty"
		if err := store.CompleteEarningsWatchlistRun(ctx, target.Date, attemptID); err != nil {
			return err
		}
		slog.Info("earnings watchlist run published", "target_session_date", target.Date.Format("2006-01-02"), "attempt_id", attemptID, "selected_count", 0)
		return nil
	}
	for index := range candidates {
		candidate := &candidates[index]
		priority, _ := json.Marshal(map[string]any{
			"bucket": "earnings_scheduled", "liquidity_rank": candidate.LiquidityRank,
			"median_dollar_volume": candidate.MedianDollarVolume, "ticker": candidate.Ticker,
		})
		candidate.ResearchQueue = &store.ResearchQueueAdmission{
			Capacity: f.ResearchQueue.Capacity, Bucket: "earnings_scheduled", Reservation: f.ResearchQueue.Reservations.EarningsScheduled,
			Overflow: f.ResearchQueue.Overflow, Priority: priority,
		}
		candidate.MaxActiveWatchlist = f.MaxActiveWatchlist
	}
	stage = "research_staging"
	publishedTickers, err := store.PublishedEarningsWatchlistCandidates(ctx, target.Date)
	if err != nil {
		return fmt.Errorf("load previously admitted earnings candidates: %w", err)
	}
	pending := make([]store.EarningsWatchlistCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if _, alreadyPublished := publishedTickers[strings.ToUpper(strings.TrimSpace(candidate.Ticker))]; alreadyPublished {
			continue
		}
		// A delayed retry must never resurrect an earnings item after its
		// intended market-session lifetime has already elapsed.
		if !candidate.ExpiresAt.After(now) {
			continue
		}
		pending = append(pending, candidate)
	}
	stagingCapacity := f.ResearchStaging.Capacity
	if stagingCapacity <= 0 {
		stagingCapacity = 30
	}
	stagingLease := time.Duration(f.ResearchStaging.LeaseMinutes) * time.Minute
	if stagingLease <= 0 {
		stagingLease = 15 * time.Minute
	}
	stagingIDs := make([]string, 0, len(pending))
	releaseStaging := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, id := range stagingIDs {
			if releaseErr := store.ReleaseResearchCandidate(cleanupCtx, id); releaseErr != nil {
				slog.Warn("earnings watchlist: release research staging candidate", "err", releaseErr)
			}
		}
	}
	defer releaseStaging()
	stagedCandidates := make([]store.EarningsWatchlistCandidate, 0, len(pending))
	stagingDeferrals := make([]store.EarningsWatchlistDeferral, 0)
	for _, candidate := range pending {
		id, _, queueErr := store.QueueResearchCandidate(ctx, store.ResearchStagingRequest{
			RunID: attemptID, Ticker: candidate.Ticker, Platform: "stock",
			Bucket: "earnings_scheduled", Priority: candidate.ResearchQueue.Priority,
			Capacity: stagingCapacity, Lease: stagingLease,
		})
		if queueErr != nil {
			if errors.Is(queueErr, store.ErrResearchStagingCapacity) {
				stagingDeferrals = append(stagingDeferrals, store.EarningsWatchlistDeferral{Candidate: candidate, Reason: queueErr})
				continue
			}
			return fmt.Errorf("stage earnings candidate %s: %w", candidate.Ticker, queueErr)
		}
		stagingIDs = append(stagingIDs, id)
		candidate.ResearchStagingID = id
		candidate.ResearchStagingLease = stagingLease
		stagedCandidates = append(stagedCandidates, candidate)
	}
	stillStaged := stagedCandidates[:0]
	for _, candidate := range stagedCandidates {
		if err := store.RenewResearchCandidate(ctx, candidate.ResearchStagingID, stagingLease); err != nil {
			if errors.Is(err, store.ErrResearchStagingReservationLost) {
				stagingDeferrals = append(stagingDeferrals, store.EarningsWatchlistDeferral{Candidate: candidate, Reason: err})
				continue
			}
			return fmt.Errorf("renew research staging candidate %s: %w", candidate.Ticker, err)
		}
		stillStaged = append(stillStaged, candidate)
	}
	stagedCandidates = stillStaged
	stage = "publish"
	publications, queueDeferrals, err := store.PublishEarningsWatchlistRun(ctx, target.Date, attemptID, stagedCandidates)
	for _, publication := range publications {
		outcome := "admitted"
		if publication.Disposition == "rediscovered" {
			outcome = "rediscovered"
		}
		for _, candidate := range stagedCandidates {
			if strings.EqualFold(strings.TrimSpace(candidate.Ticker), strings.TrimSpace(publication.Ticker)) {
				f.publishPromotionOutcome(attemptID, candidate, outcome, "", "")
				break
			}
		}
	}
	if err != nil {
		return err
	}
	deferrals := append(stagingDeferrals, queueDeferrals...)
	for _, deferred := range deferrals {
		reasonCode := "queue_capacity"
		if errors.Is(deferred.Reason, store.ErrResearchStagingCapacity) || errors.Is(deferred.Reason, store.ErrResearchStagingReservationLost) {
			reasonCode = "staging_capacity"
		} else if errors.Is(deferred.Reason, store.ErrWatchlistCapacity) {
			reasonCode = "watchlist_capacity"
		}
		f.publishPromotionOutcome(attemptID, deferred.Candidate, "not_admitted", reasonCode, deferred.Reason.Error())
	}
	if len(deferrals) > 0 {
		return fmt.Errorf("%d earnings candidates deferred by admission capacity: %w", len(deferrals), deferrals[0].Reason)
	}
	if err := store.CompleteEarningsWatchlistRun(ctx, target.Date, attemptID); err != nil {
		return err
	}
	slog.Info("earnings watchlist run published",
		"target_session_date", target.Date.Format("2006-01-02"),
		"attempt_id", attemptID,
		"selected_count", len(candidates),
		"admitted_count", len(publications),
		"expires_at", expires,
	)
	return nil
}

func (f *WatchlistFeeder) publishPromotionOutcome(
	runID string, candidate store.EarningsWatchlistCandidate, outcome, reasonCode, reason string,
) {
	if f.Bus == nil {
		return
	}
	f.Bus.Publish(events.ScannerPromotionOutcome, events.ScannerPromotionOutcomePayload{
		RunID: runID, Ticker: candidate.Ticker, Outcome: outcome,
		QueueBucket: "earnings_scheduled", ReasonCode: reasonCode, Reason: reason,
		Sources: []events.ScannerPromotionSourcePayload{{
			Source: "earnings_scheduled", Rank: candidate.LiquidityRank,
			Evidence: fmt.Sprintf("scheduled earnings; liquidity rank %d; median dollar volume %.0f", candidate.LiquidityRank, candidate.MedianDollarVolume),
		}},
	})
}

func nextSessionReleases(releases []data.EarningsRelease, target time.Time) []data.EarningsRelease {
	seen := make(map[string]data.EarningsRelease)
	for _, release := range releases {
		symbol := strings.ToUpper(strings.TrimSpace(release.Symbol))
		if symbol == "" || !sameETDate(release.Date, target) {
			continue
		}
		release.Symbol = symbol
		if current, ok := seen[symbol]; !ok || strings.TrimSpace(release.Hour) < strings.TrimSpace(current.Hour) {
			seen[symbol] = release
		}
	}
	out := make([]data.EarningsRelease, 0, len(seen))
	for _, release := range seen {
		out = append(out, release)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

func rankReleases(releases []data.EarningsRelease, bars map[string][]data.Candle) []rankedRelease {
	ranked := make([]rankedRelease, 0, len(releases))
	for _, release := range releases {
		volumes := make([]float64, 0, len(bars[release.Symbol]))
		for _, bar := range bars[release.Symbol] {
			if bar.Close > 0 && bar.Volume > 0 {
				volumes = append(volumes, bar.Close*float64(bar.Volume))
			}
		}
		if len(volumes) == 0 {
			continue
		}
		sort.Float64s(volumes)
		middle := len(volumes) / 2
		median := volumes[middle]
		if len(volumes)%2 == 0 {
			median = (volumes[middle-1] + volumes[middle]) / 2
		}
		ranked = append(ranked, rankedRelease{release: release, dollar: median})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].dollar == ranked[j].dollar {
			return ranked[i].release.Symbol < ranked[j].release.Symbol
		}
		return ranked[i].dollar > ranked[j].dollar
	})
	return ranked
}

func (f *WatchlistFeeder) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func earningsDiscoveryContext(release data.EarningsRelease, target engine.USMarketSession, rank int, dollar float64) tradeprofile.DiscoveryContext {
	evidence, _ := json.Marshal(map[string]any{
		"scheduled_date": target.Date.Format("2006-01-02"), "session": strings.TrimSpace(release.Hour),
		"calendar_source": "finnhub", "liquidity_rank": rank, "median_dollar_volume": dollar,
	})
	return tradeprofile.DiscoveryContext{
		Version: tradeprofile.DiscoveryContextVersion, Sources: []string{"earnings_scheduled"},
		Thesis:           "Scheduled earnings event; assess ordinary news, technical, and market evidence.",
		EvidenceBySource: map[string]json.RawMessage{"earnings_scheduled": evidence},
	}
}

func sameETDate(left, right time.Time) bool {
	// Finnhub's date field is a calendar date, parsed as UTC midnight by the
	// existing adapter. It must not be converted to an instant in ET (which
	// would turn it into the preceding date); compare its provider calendar
	// representation with the exchange-session calendar date instead.
	return left.Format("2006-01-02") == right.Format("2006-01-02")
}
