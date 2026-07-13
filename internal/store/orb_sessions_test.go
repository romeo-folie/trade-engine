package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

// TestGetRecentORBSessions_SameDateDeterministic guards the futures-visibility fix: a single
// day has multiple concurrent sessions (equities/forex/futures) sharing one date, and a
// date-only sort dropped one arbitrarily under LIMIT. The created_at tiebreak must keep all
// same-date sessions present and order them deterministically (newest-created first).
func TestGetRecentORBSessions_SameDateDeterministic(t *testing.T) {
	if !store.IsConnected() {
		t.Skip("DATABASE_URL not set; skipping store integration test")
	}
	ctx := context.Background()
	db := store.GetDB()

	// Far-future date so these are the most-recent rows; unique names so re-runs don't
	// collide with the (date, session_name) unique constraint.
	date := time.Date(2099, 6, 1, 0, 0, 0, 0, time.UTC)
	suffix := time.Now().UnixNano()
	names := make([]string, 3)
	for i := range names {
		names[i] = fmt.Sprintf("det-%d-%d", suffix, i)
		if _, err := store.CreateORBSession(ctx, db, date, names[i]); err != nil {
			t.Fatalf("CreateORBSession %s: %v", names[i], err)
		}
		time.Sleep(3 * time.Millisecond) // distinct created_at for a stable tiebreak
	}

	got, err := store.GetRecentORBSessions(ctx, db, 100)
	if err != nil {
		t.Fatalf("GetRecentORBSessions: %v", err)
	}
	pos := make(map[string]int, len(got))
	for i, s := range got {
		pos[s.SessionName] = i
	}
	// All three same-date sessions must be present — the bug dropped one under LIMIT.
	for _, n := range names {
		if _, ok := pos[n]; !ok {
			t.Fatalf("same-date session %q missing from result (dropped under limit)", n)
		}
	}
	// And ordered newest-created first: names[2] before names[1] before names[0].
	if !(pos[names[2]] < pos[names[1]] && pos[names[1]] < pos[names[0]]) {
		t.Errorf("same-date sessions not in created_at DESC order: %s=%d %s=%d %s=%d",
			names[2], pos[names[2]], names[1], pos[names[1]], names[0], pos[names[0]])
	}
}
