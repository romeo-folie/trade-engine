package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestResearchStagingReleasesCapacityAfterAdmission(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	const runID = "research-staging-release-rt"
	t.Cleanup(func() {
		_, _ = GetDB().Exec(ctx, `DELETE FROM research_queue_items WHERE run_id=$1`, runID)
	})
	var used int
	if err := GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM research_queue_items WHERE expires_at > NOW()`).Scan(&used); err != nil {
		t.Fatalf("count staging items: %v", err)
	}
	request := ResearchStagingRequest{
		RunID: runID, Ticker: "STAGE_A_RT", Platform: "stock", Bucket: "market_activity",
		Capacity: used + 1, Lease: time.Minute,
	}
	id, created, err := QueueResearchCandidate(ctx, request)
	if err != nil || !created || id == "" {
		t.Fatalf("queue first candidate: id=%q created=%t err=%v", id, created, err)
	}
	request.Ticker = "STAGE_B_RT"
	if _, _, err := QueueResearchCandidate(ctx, request); !errors.Is(err, ErrResearchStagingCapacity) {
		t.Fatalf("queue second candidate at capacity: %v, want ErrResearchStagingCapacity", err)
	}
	if err := ReleaseResearchCandidate(ctx, id); err != nil {
		t.Fatalf("release first candidate: %v", err)
	}
	id, created, err = QueueResearchCandidate(ctx, request)
	if err != nil || !created || id == "" {
		t.Fatalf("queue second candidate after release: id=%q created=%t err=%v", id, created, err)
	}
}

func TestResearchStagingConcurrentRunRequiresItsOwnCapacity(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	const ticker = "STAGE_SHARED_OWNER_RT"
	t.Cleanup(func() {
		_, _ = GetDB().Exec(ctx, `DELETE FROM research_queue_items WHERE ticker=$1`, ticker)
	})
	var used int
	if err := GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM research_queue_items WHERE expires_at > NOW()`).Scan(&used); err != nil {
		t.Fatalf("count staging items: %v", err)
	}
	request := ResearchStagingRequest{
		RunID: "research-staging-owner-a-rt", Ticker: ticker, Platform: "stock", Bucket: "market_activity",
		Capacity: used + 1, Lease: time.Minute,
	}
	ownerID, created, err := QueueResearchCandidate(ctx, request)
	if err != nil || !created {
		t.Fatalf("queue owner: created=%t err=%v", created, err)
	}
	request.RunID = "research-staging-owner-b-rt"
	if _, _, err := QueueResearchCandidate(ctx, request); !errors.Is(err, ErrResearchStagingCapacity) {
		t.Fatalf("queue concurrent run at capacity: %v, want ErrResearchStagingCapacity", err)
	}
	if err := ReleaseResearchCandidate(ctx, ownerID); err != nil {
		t.Fatalf("release owner candidate: %v", err)
	}
}

func TestResearchStagingRenewalDoesNotReviveExpiredReservation(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	const runID = "research-staging-renew-rt"
	const ticker = "STAGE_RENEW_RT"
	t.Cleanup(func() {
		_, _ = GetDB().Exec(ctx, `DELETE FROM research_queue_items WHERE run_id=$1`, runID)
	})
	var used int
	if err := GetDB().QueryRow(ctx, `SELECT COUNT(*) FROM research_queue_items WHERE expires_at > NOW()`).Scan(&used); err != nil {
		t.Fatalf("count staging items: %v", err)
	}
	id, created, err := QueueResearchCandidate(ctx, ResearchStagingRequest{
		RunID: runID, Ticker: ticker, Platform: "stock", Bucket: "market_activity",
		Capacity: used + 1, Lease: time.Minute,
	})
	if err != nil || !created {
		t.Fatalf("queue renewable candidate: created=%t err=%v", created, err)
	}
	if err := RenewResearchCandidate(ctx, id, time.Minute); err != nil {
		t.Fatalf("renew live staging candidate: %v", err)
	}
	if _, err := GetDB().Exec(ctx, `UPDATE research_queue_items SET expires_at=NOW()-INTERVAL '1 second' WHERE id=$1::uuid`, id); err != nil {
		t.Fatalf("expire staging fixture: %v", err)
	}
	if err := RenewResearchCandidate(ctx, id, time.Minute); !errors.Is(err, ErrResearchStagingReservationLost) {
		t.Fatalf("renew expired staging candidate: %v, want ErrResearchStagingReservationLost", err)
	}
}
