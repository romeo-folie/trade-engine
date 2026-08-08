package store_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

var swingUniverseFixtureSequence atomic.Uint64

func beginClaimedSwingUniverseFixture(
	t *testing.T, ctx context.Context, version store.SwingUniverseVersion,
) store.SwingUniverseBuildClaim {
	t.Helper()
	configHash := fmt.Sprintf("test-fixture-%d", swingUniverseFixtureSequence.Add(1))
	claim, owned, err := store.ClaimSwingUniverseBuild(ctx, store.SwingUniverseBuildRequest{
		SessionDate: version.AsOf, Provider: version.Provider, ConfigHash: configHash,
		ConfigSnapshot: version.Filters, StartedAt: version.CapturedAt,
	}, 24*time.Hour)
	if err != nil || !owned {
		t.Fatalf("ClaimSwingUniverseBuild fixture: claim=%+v owned=%v err=%v", claim, owned, err)
	}
	if err := store.UpdateClaimedSwingUniverseMetadata(ctx, *claim, version); err != nil {
		t.Fatalf("UpdateClaimedSwingUniverseMetadata fixture: %v", err)
	}
	return *claim
}

func completeClaimedSwingUniverseFixture(
	t *testing.T, ctx context.Context, claim store.SwingUniverseBuildClaim,
	completedAt time.Time,
) {
	t.Helper()
	if err := store.CompleteClaimedSwingUniverseVersion(ctx, claim, completedAt, store.SwingUniverseQuality{
		MarketDataCoverage: 1, FundamentalsCoverage: 1, MinMarketDataCoverage: 0.90,
	}); err != nil {
		t.Fatalf("CompleteClaimedSwingUniverseVersion fixture: %v", err)
	}
}
