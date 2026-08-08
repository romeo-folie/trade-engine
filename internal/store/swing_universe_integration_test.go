package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestSwingUniversePointInTimeEvidence(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	claim := beginClaimedSwingUniverseFixture(t, ctx, store.SwingUniverseVersion{
		Provider:       "test-provider",
		AsOf:           base,
		CapturedAt:     base.Add(time.Minute),
		Filters:        json.RawMessage(`{"min_price":0}`),
		SourceEvidence: json.RawMessage(`{"page_count":1}`),
	})
	versionID := claim.ActiveVersionID

	zero := 0.0
	available := true
	active := true
	tradable := true
	taxonomyAsOf := base.Add(-24 * time.Hour)
	err := store.ReplaceClaimedSwingUniverseMembers(ctx, claim, []store.SwingUniverseMember{
		{
			Ticker:           "ZERO",
			AssetID:          "asset-zero",
			Name:             "Zero Corp",
			Eligible:         true,
			Active:           &active,
			Tradable:         &tradable,
			Price:            &zero,
			SpreadBPS:        &zero,
			DataAvailable:    &available,
			Sector:           "Technology",
			Industry:         "Software",
			TaxonomyStatus:   "known",
			TaxonomyAsOf:     &taxonomyAsOf,
			TaxonomyProvider: "test-provider",
			TaxonomyEvidence: json.RawMessage(`{"confidence":0}`),
			SourceEvidence:   json.RawMessage(`{"rank":0}`),
		},
		{
			Ticker:           "UNKNOWN",
			Eligible:         false,
			RemovalStage:     "taxonomy",
			FailureReason:    "classification unavailable",
			TaxonomyStatus:   "unavailable",
			SourceEvidence:   json.RawMessage(`{"partial_failure":true}`),
			TaxonomyEvidence: json.RawMessage(`{"provider_error":"timeout"}`),
		},
	})
	if err != nil {
		t.Fatalf("ReplaceSwingUniverseMembers: %v", err)
	}
	completedAt := base.Add(2 * time.Minute)
	completeClaimedSwingUniverseFixture(t, ctx, claim, completedAt)

	if err := store.ReplaceSwingUniverseMembers(ctx, versionID, nil); err == nil {
		t.Fatal("completed universe accepted member replacement")
	}
	buildingID, err := store.BeginSwingUniverseVersion(ctx, store.SwingUniverseVersion{
		Provider: "move-target", AsOf: base, CapturedAt: completedAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("BeginSwingUniverseVersion move target: %v", err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE swing_universe_members
		SET universe_version_id=$1
		WHERE universe_version_id=$2 AND ticker='ZERO'`,
		buildingID, versionID,
	); err == nil {
		t.Fatal("completed universe member moved to a building version")
	}
	var completedCount, buildingCount int
	if err := store.GetDB().QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE universe_version_id=$1),
			COUNT(*) FILTER (WHERE universe_version_id=$2)
		FROM swing_universe_members
		WHERE ticker='ZERO'`,
		versionID, buildingID,
	).Scan(&completedCount, &buildingCount); err != nil {
		t.Fatalf("verify member parent after rejected move: %v", err)
	}
	if completedCount != 1 || buildingCount != 0 {
		t.Fatalf(
			"rejected move changed member lineage: completed=%d building=%d",
			completedCount, buildingCount,
		)
	}
	if got, err := store.GetLatestCompletedSwingUniverseVersionAsOf(
		ctx, completedAt.Add(-time.Nanosecond),
	); err != nil || (got != nil && got.ID == versionID) {
		t.Fatalf("future completed version leaked: got=%+v err=%v", got, err)
	}
	if got, err := store.GetCompletedSwingUniverseVersionByIDAsOf(
		ctx, versionID, completedAt.Add(-time.Nanosecond),
	); err != nil || got != nil {
		t.Fatalf("future exact version leaked: got=%+v err=%v", got, err)
	}
	version, err := store.GetLatestCompletedSwingUniverseVersionAsOf(
		ctx, completedAt.Add(time.Nanosecond),
	)
	if err != nil || version == nil || version.ID != versionID {
		t.Fatalf("latest completed version = %+v, err=%v", version, err)
	}
	exactVersion, err := store.GetCompletedSwingUniverseVersionByIDAsOf(
		ctx, versionID, completedAt.Add(time.Nanosecond),
	)
	if err != nil || exactVersion == nil || exactVersion.ID != versionID ||
		!exactVersion.AsOf.Equal(base) {
		t.Fatalf("exact completed version = %+v, err=%v", exactVersion, err)
	}
	if version.MemberCount != 2 || version.EligibleCount != 1 ||
		version.UnknownTaxonomyCount != 1 {
		t.Fatalf("version counts = %+v", version)
	}

	members, err := store.ListEligibleSwingUniverseMembers(ctx, versionID)
	if err != nil {
		t.Fatalf("ListEligibleSwingUniverseMembers: %v", err)
	}
	if len(members) != 1 || members[0].Price == nil || *members[0].Price != 0 ||
		members[0].SpreadBPS == nil || *members[0].SpreadBPS != 0 ||
		members[0].AssetID != "asset-zero" || members[0].Active == nil ||
		!*members[0].Active || members[0].Tradable == nil || !*members[0].Tradable {
		t.Fatalf("zero-valued member evidence lost: %+v", members)
	}
	allMembers, err := store.ListSwingUniverseMembers(ctx, versionID, false)
	if err != nil || len(allMembers) != 2 {
		t.Fatalf("full asset-master enumeration = %+v, err=%v", allMembers, err)
	}
	industryMembers, err := store.ListIndustrySwingUniverseMembers(
		ctx, versionID, "Software", completedAt, 48*time.Hour,
	)
	if err != nil || len(industryMembers) != 1 || industryMembers[0].Ticker != "ZERO" {
		t.Fatalf("fresh industry members = %+v, err=%v", industryMembers, err)
	}
	industryMembers, err = store.ListIndustrySwingUniverseMembers(
		ctx, versionID, "Software", completedAt.Add(-time.Nanosecond), 48*time.Hour,
	)
	if err != nil || len(industryMembers) != 0 {
		t.Fatalf("future universe leaked through industry lookup: %+v, err=%v", industryMembers, err)
	}
	industryMembers, err = store.ListIndustrySwingUniverseMembers(
		ctx, versionID, "Software", completedAt.Add(72*time.Hour), 48*time.Hour,
	)
	if err != nil || len(industryMembers) != 0 {
		t.Fatalf("stale industry taxonomy did not fail closed: %+v, err=%v", industryMembers, err)
	}
	if got, err := store.GetUsableSwingUniverseVersion(
		ctx, completedAt.Add(time.Minute), time.Hour,
	); err != nil || got == nil || got.ID != versionID {
		t.Fatalf("fresh cached version = %+v, err=%v", got, err)
	}
	if got, err := store.GetUsableSwingUniverseVersion(
		ctx, completedAt.Add(3*time.Hour), time.Hour,
	); err != nil || got != nil {
		t.Fatalf("expired cached version = %+v, err=%v", got, err)
	}
}

func TestSwingUniverseMemberListsRequireCompletedVersion(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	id, err := store.BeginSwingUniverseVersion(ctx, store.SwingUniverseVersion{
		Provider: "unfinished-list-test", AsOf: now, CapturedAt: now,
	})
	if err != nil {
		t.Fatalf("BeginSwingUniverseVersion: %v", err)
	}
	if err := store.ReplaceSwingUniverseMembers(ctx, id, []store.SwingUniverseMember{{
		Ticker: "BUILDING", Eligible: true, TaxonomyStatus: "unknown",
	}}); err != nil {
		t.Fatalf("ReplaceSwingUniverseMembers: %v", err)
	}

	if got, err := store.ListSwingUniverseMembers(ctx, id, false); err != nil || len(got) != 0 {
		t.Fatalf("building universe exposed by general list: got=%+v err=%v", got, err)
	}
	if got, err := store.ListEligibleSwingUniverseMembers(ctx, id); err != nil || len(got) != 0 {
		t.Fatalf("building universe exposed by eligible list: got=%+v err=%v", got, err)
	}
	if got, err := store.ListSwingUniverseMembersForAudit(ctx, id); err != nil ||
		len(got) != 1 || got[0].Ticker != "BUILDING" {
		t.Fatalf("building audit list = %+v, err=%v", got, err)
	}

	if err := store.FailSwingUniverseVersion(ctx, id, now.Add(time.Second), errors.New("provider failed")); err != nil {
		t.Fatalf("FailSwingUniverseVersion: %v", err)
	}
	if got, err := store.ListSwingUniverseMembers(ctx, id, false); err != nil || len(got) != 0 {
		t.Fatalf("failed universe exposed by general list: got=%+v err=%v", got, err)
	}
	if got, err := store.ListEligibleSwingUniverseMembers(ctx, id); err != nil || len(got) != 0 {
		t.Fatalf("failed universe exposed by eligible list: got=%+v err=%v", got, err)
	}
	if got, err := store.ListSwingUniverseMembersForAudit(ctx, id); err != nil ||
		len(got) != 1 || got[0].Ticker != "BUILDING" {
		t.Fatalf("failed audit list = %+v, err=%v", got, err)
	}
	if got, err := store.GetCompletedSwingUniverseVersionByIDAsOf(
		ctx, id, now.Add(time.Hour),
	); err != nil || got != nil {
		t.Fatalf("failed version validated as completed: got=%+v err=%v", got, err)
	}
}

func TestCompanyTaxonomyStrictAsOfAndStaleness(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	_, err := store.InsertCompanyTaxonomyObservation(ctx, store.CompanyTaxonomyObservation{
		Ticker:      "POINT_IN_TIME",
		Provider:    "test-provider",
		EffectiveAt: base,
		ObservedAt:  base.Add(time.Hour),
		Sector:      "Technology",
		Industry:    "Semiconductors",
		Status:      "known",
		Evidence:    json.RawMessage(`{"classification":"provider"}`),
	})
	if err != nil {
		t.Fatalf("InsertCompanyTaxonomyObservation: %v", err)
	}
	_, err = store.InsertCompanyTaxonomyObservation(ctx, store.CompanyTaxonomyObservation{
		Ticker:      "POINT_IN_TIME",
		Provider:    "test-provider",
		EffectiveAt: base.Add(24 * time.Hour),
		ObservedAt:  base.Add(25 * time.Hour),
		Sector:      "Technology",
		Industry:    "Hardware",
		Status:      "known",
	})
	if err != nil {
		t.Fatalf("insert future taxonomy: %v", err)
	}

	if got, err := store.GetCompanyTaxonomyAsOf(
		ctx, "POINT_IN_TIME", base.Add(30*time.Minute), 7*24*time.Hour,
	); err != nil || got != nil {
		t.Fatalf("observation available before observed_at: got=%+v err=%v", got, err)
	}
	got, err := store.GetCompanyTaxonomyAsOf(
		ctx, "POINT_IN_TIME", base.Add(2*time.Hour), 7*24*time.Hour,
	)
	if err != nil || got == nil || got.Industry != "Semiconductors" || got.Status != "known" {
		t.Fatalf("historical taxonomy = %+v, err=%v", got, err)
	}
	got, err = store.GetCompanyTaxonomyAsOf(
		ctx, "POINT_IN_TIME", base.Add(20*24*time.Hour), 7*24*time.Hour,
	)
	if err != nil || got == nil || got.Industry != "Hardware" || got.Status != "stale" {
		t.Fatalf("stale taxonomy = %+v, err=%v", got, err)
	}
}

func TestFailSwingUniverseVersionIsTerminal(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	id, err := store.BeginSwingUniverseVersion(ctx, store.SwingUniverseVersion{
		Provider: "failed-provider", AsOf: now, CapturedAt: now,
	})
	if err != nil {
		t.Fatalf("BeginSwingUniverseVersion: %v", err)
	}
	if err := store.FailSwingUniverseVersion(ctx, id, now, errors.New("partial provider failure")); err != nil {
		t.Fatalf("FailSwingUniverseVersion: %v", err)
	}
	if err := store.CompleteSwingUniverseVersion(ctx, id, now); err == nil {
		t.Fatal("failed version was completed")
	}
}

func TestSwingUniverseBuildClaimLifecycleAndQualityGate(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	started := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Microsecond)
	identity := fmt.Sprintf("claim-lifecycle-%d", started.UnixNano())
	request := store.SwingUniverseBuildRequest{
		SessionDate: started, Provider: identity, ConfigHash: identity,
		ConfigSnapshot: json.RawMessage(`{"fixture":"claim-lifecycle"}`),
		StartedAt:      started,
	}
	first, owned, err := store.ClaimSwingUniverseBuild(ctx, request, time.Hour)
	if err != nil || !owned || first.AttemptCount != 1 || first.ActiveVersionID == "" {
		t.Fatalf("first claim = %+v owned=%v err=%v", first, owned, err)
	}
	duplicate, owned, err := store.ClaimSwingUniverseBuild(ctx, request, time.Hour)
	if err != nil || owned || duplicate.ID != first.ID ||
		duplicate.ActiveVersionID != first.ActiveVersionID {
		t.Fatalf("duplicate claim = %+v owned=%v err=%v", duplicate, owned, err)
	}
	if err := store.ReplaceClaimedSwingUniverseMembers(ctx, *first, []store.SwingUniverseMember{{
		Ticker: "OLD", Eligible: true, TaxonomyStatus: "unknown",
	}}); err != nil {
		t.Fatal(err)
	}
	rejected := store.SwingUniverseQuality{
		EvidenceRequiredCount: 10, UsableSnapshotCount: 10,
		UsableBarHistoryCount: 8, CoherentMarketDataCount: 8,
		MarketDataCoverage: 0.8, FundamentalsCoverage: 1,
		MinMarketDataCoverage: 0.9,
	}
	if err := store.FailClaimedSwingUniverseVersion(
		ctx, *first, started.Add(2*time.Minute), rejected, errors.New("coverage rejected"),
	); err != nil {
		t.Fatal(err)
	}
	request.StartedAt = started.Add(3 * time.Minute)
	second, owned, err := store.ClaimSwingUniverseBuild(ctx, request, time.Hour)
	if err != nil || !owned || second.AttemptCount != 2 ||
		second.ActiveVersionID == first.ActiveVersionID {
		t.Fatalf("reclaim = %+v owned=%v err=%v", second, owned, err)
	}
	if err := store.ReplaceClaimedSwingUniverseMembers(ctx, *first, nil); err == nil {
		t.Fatal("stale owner replaced members after reclaim")
	}
	if err := store.CompleteClaimedSwingUniverseVersion(ctx, *second, request.StartedAt.Add(time.Minute), store.SwingUniverseQuality{
		EvidenceRequiredCount: 10, UsableSnapshotCount: 10,
		UsableBarHistoryCount: 8, CoherentMarketDataCount: 8,
		MarketDataCoverage: 0.8, FundamentalsCoverage: 1,
		MinMarketDataCoverage: 0.9,
	}); err == nil {
		t.Fatal("below-threshold universe was approved")
	}
	if err := store.ReplaceClaimedSwingUniverseMembers(ctx, *second, []store.SwingUniverseMember{{
		Ticker: "NEW", Eligible: true, TaxonomyStatus: "unknown",
	}}); err != nil {
		t.Fatal(err)
	}
	approved := store.SwingUniverseQuality{
		EvidenceRequiredCount: 10, UsableSnapshotCount: 10,
		UsableBarHistoryCount: 9, CoherentMarketDataCount: 9,
		BarFailureCount: 1, ProviderFailureCount: 1,
		MarketDataCoverage: 0.9, FundamentalsCoverage: 1,
		MinMarketDataCoverage: 0.9,
	}
	completedAt := request.StartedAt.Add(2 * time.Minute)
	if err := store.CompleteClaimedSwingUniverseVersion(ctx, *second, completedAt, approved); err != nil {
		t.Fatal(err)
	}
	request.StartedAt = completedAt.Add(time.Minute)
	reused, owned, err := store.ClaimSwingUniverseBuild(ctx, request, time.Hour)
	if err != nil || owned || reused.Status != "completed" ||
		reused.PublishedVersionID != second.ActiveVersionID || reused.AttemptCount != 2 {
		t.Fatalf("restart reuse = %+v owned=%v err=%v", reused, owned, err)
	}
	version, err := store.GetApprovedSwingUniverseForSessionAsOf(
		ctx, request.SessionDate, request.StartedAt, request.Provider, request.ConfigHash,
	)
	if err != nil || version == nil || version.ID != second.ActiveVersionID ||
		version.Quality.MarketDataCoverage != 0.9 {
		t.Fatalf("approved session version = %+v err=%v", version, err)
	}
	oldMembers, err := store.ListSwingUniverseMembersForAudit(ctx, first.ActiveVersionID)
	if err != nil || len(oldMembers) != 1 || oldMembers[0].Ticker != "OLD" {
		t.Fatalf("immutable rejected attempt members = %+v err=%v", oldMembers, err)
	}
}

func TestSwingUniverseExpiredLeaseCannotBeResurrected(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	started := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Microsecond)
	identity := fmt.Sprintf("expired-lease-%d", started.UnixNano())
	claim, owned, err := store.ClaimSwingUniverseBuild(ctx, store.SwingUniverseBuildRequest{
		SessionDate: started, Provider: identity, ConfigHash: identity,
		ConfigSnapshot: json.RawMessage(`{"fixture":"expired-lease"}`), StartedAt: started,
	}, time.Minute)
	if err != nil || !owned {
		t.Fatalf("claim = %+v owned=%v err=%v", claim, owned, err)
	}
	if _, err := store.GetDB().Exec(ctx, `
		UPDATE swing_universe_build_claims SET lease_until=clock_timestamp()-interval '1 second'
		WHERE id=$1`, claim.ID); err != nil {
		t.Fatal(err)
	}
	afterExpiry := time.Now().UTC()
	if err := store.RenewSwingUniverseBuildLease(ctx, claim.ID, claim.OwnerToken, claim.AttemptCount, afterExpiry, time.Hour); err == nil {
		t.Fatal("expired lease renewed")
	}
	if err := store.UpdateClaimedSwingUniverseMetadata(ctx, *claim, store.SwingUniverseVersion{
		AsOf: afterExpiry,
	}); err == nil {
		t.Fatal("expired lease updated metadata")
	}
	if err := store.ReplaceClaimedSwingUniverseMembers(ctx, *claim, []store.SwingUniverseMember{{
		Ticker: "LATE", TaxonomyStatus: "unknown",
	}}); err == nil {
		t.Fatal("expired lease replaced members")
	}
	if err := store.FailClaimedSwingUniverseVersion(ctx, *claim, afterExpiry, store.SwingUniverseQuality{}, errors.New("late")); err == nil {
		t.Fatal("expired lease failed version")
	}
}

func TestApprovedUniverseSessionLookupSurvivesWeekendWithoutFallingBack(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	friday := time.Date(2026, time.August, 7, 16, 30, 0, 0, location)
	monday := time.Date(2026, time.August, 10, 8, 0, 0, 0, location)
	claim := beginClaimedSwingUniverseFixture(t, ctx, store.SwingUniverseVersion{
		Provider: "weekend-session-fixture", AsOf: friday,
		CapturedAt: friday, Filters: json.RawMessage(`{"fixture":"weekend"}`),
	})
	completeClaimedSwingUniverseFixture(t, ctx, claim, friday.Add(time.Minute))
	version, err := store.GetLatestApprovedSwingUniverseForSessionAsOf(ctx, friday, monday)
	if err != nil || version == nil || version.ID != claim.ActiveVersionID {
		t.Fatalf("Friday universe at Monday checkpoint = %+v err=%v", version, err)
	}
	thursday := friday.AddDate(0, 0, -1)
	version, err = store.GetLatestApprovedSwingUniverseForSessionAsOf(ctx, thursday, monday)
	if err != nil || version != nil {
		t.Fatalf("missing Thursday session fell back = %+v err=%v", version, err)
	}
}
