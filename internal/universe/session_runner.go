package universe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type BuildLeaseActiveError struct {
	RetryAt time.Time
}

type HistoricalSessionUnavailableError struct {
	SessionDate time.Time
}

func (e *HistoricalSessionUnavailableError) Error() string {
	return fmt.Sprintf(
		"no approved universe exists for historical session %s; live provider data cannot backfill it",
		e.SessionDate.Format("2006-01-02"),
	)
}

func (e *BuildLeaseActiveError) Error() string {
	return fmt.Sprintf("swing universe build is leased until %s", e.RetryAt.Format(time.RFC3339))
}

func (e *BuildLeaseActiveError) RetryAtTime() time.Time { return e.RetryAt }

type SessionRunner struct {
	Builder Builder
	Config  config.UniverseConfig
	Now     func() time.Time
}

func (r *SessionRunner) RunSession(
	ctx context.Context, session engine.USMarketSession,
) (*store.SwingUniverseVersion, error) {
	return r.runSession(ctx, session, false)
}

// RunScheduledSession authorizes the configured delayed session even when its
// due time crosses midnight. It still requires that session to be the latest
// actually completed one, preventing current data from becoming a backfill.
func (r *SessionRunner) RunScheduledSession(
	ctx context.Context, session engine.USMarketSession,
) (*store.SwingUniverseVersion, error) {
	return r.runSession(ctx, session, true)
}

func (r *SessionRunner) runSession(
	ctx context.Context, session engine.USMarketSession, scheduled bool,
) (*store.SwingUniverseVersion, error) {
	if !r.Config.Enabled {
		return nil, nil
	}
	now := r.now().UTC()
	if now.Before(session.Close) {
		return nil, fmt.Errorf("swing universe session has not closed")
	}
	marketDataReadyAt := session.Close.Add(requiredUniverseFeedDelay(r.Config))
	if now.Before(marketDataReadyAt) {
		return nil, fmt.Errorf(
			"swing universe market data for session %s is not complete until %s",
			session.Date.Format("2006-01-02"), marketDataReadyAt.Format(time.RFC3339),
		)
	}
	snapshot, configHash, err := UniverseConfigEvidence(r.Config)
	if err != nil {
		return nil, err
	}
	if !r.sessionAuthorized(now, session, scheduled) {
		existing, lookupErr := store.GetApprovedSwingUniverseForSessionAsOf(
			ctx, session.Date, now, r.Config.Provider, configHash,
		)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if existing != nil {
			return existing, nil
		}
		return nil, &HistoricalSessionUnavailableError{SessionDate: session.Date}
	}
	lease := time.Duration(r.Config.RunLeaseMinutes) * time.Minute
	if lease <= 0 {
		lease = 2 * time.Hour
	}
	claim, owned, err := store.ClaimSwingUniverseBuild(
		ctx,
		store.SwingUniverseBuildRequest{
			SessionDate: session.Date, Provider: r.Config.Provider,
			ConfigHash: configHash, ConfigSnapshot: snapshot, StartedAt: now,
		},
		lease,
	)
	if err != nil {
		return nil, err
	}
	if !owned {
		if claim.Status == "completed" && claim.PublishedVersionID != "" {
			return store.GetCompletedSwingUniverseVersionByIDAsOf(
				ctx, claim.PublishedVersionID, now,
			)
		}
		return nil, &BuildLeaseActiveError{RetryAt: claim.LeaseUntil}
	}
	buildCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		interval := lease / 3
		if interval < time.Second {
			interval = time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-buildCtx.Done():
				return
			case beatAt := <-ticker.C:
				if renewErr := store.RenewSwingUniverseBuildLease(
					buildCtx, claim.ID, claim.OwnerToken, claim.AttemptCount,
					beatAt.UTC(), lease,
				); renewErr != nil {
					cancel()
					return
				}
			}
		}
	}()

	builder := r.Builder
	builder.Config = r.Config
	if builder.Now == nil && r.Now != nil {
		builder.Now = r.Now
	}
	builder.Repository = claimedStoreRepository{claim: *claim}
	result, err := builder.Refresh(buildCtx)
	cancel()
	<-heartbeatDone
	if err != nil {
		return nil, err
	}
	return store.GetCompletedSwingUniverseVersionByIDAsOf(
		ctx, result.VersionID, r.now().UTC(),
	)
}

func (r *SessionRunner) sessionAuthorized(
	now time.Time, session engine.USMarketSession, scheduled bool,
) bool {
	if now.Before(session.Close.Add(requiredUniverseFeedDelay(r.Config))) {
		return false
	}
	nowAtExchange := now.In(session.Date.Location())
	sameDate := nowAtExchange.Year() == session.Date.Year() &&
		nowAtExchange.YearDay() == session.Date.YearDay()
	if !scheduled {
		return sameDate
	}
	delay := time.Duration(r.Config.AfterCloseDelayMinutes) * time.Minute
	if delay < 0 {
		delay = 0
	}
	expected := engine.MostRecentCompletedMarketSession(now, delay)
	latestCompleted := engine.MostRecentCompletedMarketSession(now, 0)
	return sameSessionDate(expected, session) &&
		sameSessionDate(latestCompleted, session)
}

func requiredUniverseFeedDelay(cfg config.UniverseConfig) time.Duration {
	if cfg.SnapshotFeed == "delayed_sip" || cfg.HistoricalFeed == "sip" {
		return data.AlpacaSIPRecentDataDelay
	}
	return 0
}

func sameSessionDate(left, right engine.USMarketSession) bool {
	return left.Date.Year() == right.Date.Year() &&
		left.Date.YearDay() == right.Date.YearDay()
}

func UniverseConfigEvidence(
	cfg config.UniverseConfig,
) (json.RawMessage, string, error) {
	snapshot, err := json.Marshal(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("serialize universe config snapshot: %w", err)
	}
	identity := cfg
	identity.AfterCloseDelayMinutes = 0
	identity.RunLeaseMinutes = 0
	encodedIdentity, err := json.Marshal(identity)
	if err != nil {
		return nil, "", fmt.Errorf("serialize universe config identity: %w", err)
	}
	sum := sha256.Sum256(encodedIdentity)
	return snapshot, hex.EncodeToString(sum[:]), nil
}

func (r *SessionRunner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

type claimedStoreRepository struct {
	claim store.SwingUniverseBuildClaim
}

func (r claimedStoreRepository) Begin(
	ctx context.Context, version store.SwingUniverseVersion,
) (string, error) {
	if err := store.UpdateClaimedSwingUniverseMetadata(ctx, r.claim, version); err != nil {
		return "", err
	}
	return r.claim.ActiveVersionID, nil
}

func (r claimedStoreRepository) UpdateMetadata(
	ctx context.Context, _ string, version store.SwingUniverseVersion,
) error {
	return store.UpdateClaimedSwingUniverseMetadata(ctx, r.claim, version)
}

func (r claimedStoreRepository) ReplaceMembers(
	ctx context.Context, _ string, members []store.SwingUniverseMember,
) error {
	return store.ReplaceClaimedSwingUniverseMembers(ctx, r.claim, members)
}

func (r claimedStoreRepository) Complete(
	ctx context.Context, _ string, at time.Time, quality store.SwingUniverseQuality,
) error {
	return store.CompleteClaimedSwingUniverseVersion(ctx, r.claim, at, quality)
}

func (r claimedStoreRepository) Fail(
	ctx context.Context, _ string, at time.Time,
	quality store.SwingUniverseQuality, cause error,
) error {
	return store.FailClaimedSwingUniverseVersion(ctx, r.claim, at, quality, cause)
}

func (r claimedStoreRepository) TaxonomyAsOf(
	ctx context.Context, ticker string, asOf time.Time, maxAge time.Duration,
) (*store.CompanyTaxonomyObservation, error) {
	return store.GetCompanyTaxonomyAsOf(ctx, ticker, asOf, maxAge)
}

func (r claimedStoreRepository) InsertTaxonomy(
	ctx context.Context, observation store.CompanyTaxonomyObservation,
) (string, error) {
	return store.InsertCompanyTaxonomyObservation(ctx, observation)
}
