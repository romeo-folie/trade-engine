package store_test

import (
	"context"
	"encoding/json"
	"errors"
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
	versionID, err := store.BeginSwingUniverseVersion(ctx, store.SwingUniverseVersion{
		Provider:       "test-provider",
		AsOf:           base,
		CapturedAt:     base.Add(time.Minute),
		Filters:        json.RawMessage(`{"min_price":0}`),
		SourceEvidence: json.RawMessage(`{"page_count":1}`),
	})
	if err != nil {
		t.Fatalf("BeginSwingUniverseVersion: %v", err)
	}

	zero := 0.0
	available := true
	active := true
	tradable := true
	taxonomyAsOf := base.Add(-24 * time.Hour)
	err = store.ReplaceSwingUniverseMembers(ctx, versionID, []store.SwingUniverseMember{
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
	if err := store.CompleteSwingUniverseVersion(ctx, versionID, completedAt); err != nil {
		t.Fatalf("CompleteSwingUniverseVersion: %v", err)
	}

	if err := store.ReplaceSwingUniverseMembers(ctx, versionID, nil); err == nil {
		t.Fatal("completed universe accepted member replacement")
	}
	if got, err := store.GetLatestCompletedSwingUniverseVersionAsOf(
		ctx, completedAt.Add(-time.Nanosecond),
	); err != nil || got != nil {
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
