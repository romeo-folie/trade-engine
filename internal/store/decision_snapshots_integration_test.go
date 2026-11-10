package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func newPendingDecisionSnapshot(t *testing.T, ticker string) string {
	t.Helper()
	id, err := store.InsertDecisionSnapshot(context.Background(), store.DecisionSnapshot{
		SchemaVersion: 1, Ticker: ticker, Timeframe: "1d", DecisionCutoff: time.Now().UTC(),
		CodeRevision: "test", Workflow: "ensemble", PolicyVersion: "p1", PromptVersion: "t1",
		InputHash: "in-" + ticker, DecisionHash: "dec-" + ticker,
		Inputs: json.RawMessage(`{"ticker":"` + ticker + `"}`), EffectiveConfig: json.RawMessage(`{}`),
		ConfigHash: "cfg", Outcome: store.DecisionOutcomePending,
	})
	if err != nil {
		t.Fatalf("InsertDecisionSnapshot: %v", err)
	}
	return id
}

func requireDBError(t *testing.T, err error, fragment string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("err = %v, want one containing %q", err, fragment)
	}
}

func TestDecisionSnapshotLifecycleAndImmutability(t *testing.T) {
	ctx := context.Background()
	id := newPendingDecisionSnapshot(t, "DSNAPA")

	// Inputs are immutable even while pending.
	_, err := store.GetDB().Exec(ctx,
		`UPDATE decision_snapshots SET inputs = '{"ticker":"X"}', outcome = 'failed', outcome_reason = 'x', completed_at = now() WHERE id = $1`, id)
	requireDBError(t, err, "inputs are immutable")

	// Stage calls and completion land atomically.
	err = store.CompleteDecisionSnapshot(ctx, id, store.DecisionCompletion{
		Outcome: store.DecisionOutcomeCompleted, FinalOutput: json.RawMessage(`{"action":"HOLD"}`), OutputHash: "out",
		StageCalls: []store.DecisionStageCall{
			{Stage: "analyst", Ordinal: 0, Provider: "a", RequestedModel: "m", ParsedResponse: json.RawMessage(`{"action":"HOLD"}`)},
			{Stage: "analyst", Ordinal: 1, Provider: "b", RequestedModel: "m", Error: "timeout"},
		},
	})
	if err != nil {
		t.Fatalf("CompleteDecisionSnapshot: %v", err)
	}
	snap, err := store.GetDecisionSnapshot(ctx, id)
	if err != nil || snap.Outcome != store.DecisionOutcomeCompleted || snap.OutputHash != "out" || snap.CompletedAt == nil {
		t.Fatalf("snapshot = %+v, err %v", snap, err)
	}
	calls, err := store.GetDecisionStageCalls(ctx, id)
	if err != nil || len(calls) != 2 || calls[1].Error != "timeout" {
		t.Fatalf("calls = %+v, err %v", calls, err)
	}

	// Settled rows cannot change, be deleted, or gain stage calls.
	_, err = store.GetDB().Exec(ctx, `UPDATE decision_snapshots SET outcome_reason = 'edited' WHERE id = $1`, id)
	requireDBError(t, err, "is settled")
	_, err = store.GetDB().Exec(ctx, `DELETE FROM decision_snapshots WHERE id = $1`, id)
	requireDBError(t, err, "cannot be deleted")
	_, err = store.GetDB().Exec(ctx,
		`INSERT INTO decision_stage_calls (snapshot_id, stage, ordinal, provider, requested_model) VALUES ($1, 'arbiter', 0, 'x', 'y')`, id)
	requireDBError(t, err, "stage calls are closed")
	_, err = store.GetDB().Exec(ctx, `UPDATE decision_stage_calls SET error = 'x' WHERE snapshot_id = $1`, id)
	requireDBError(t, err, "stage calls are immutable")
	_, err = store.GetDB().Exec(ctx, `DELETE FROM decision_stage_calls WHERE snapshot_id = $1`, id)
	requireDBError(t, err, "stage calls are immutable")

	// A second completion is refused rather than overwriting the record.
	if err := store.CompleteDecisionSnapshot(ctx, id, store.DecisionCompletion{
		Outcome: store.DecisionOutcomeFailed, OutcomeReason: "late",
	}); !errors.Is(err, store.ErrDecisionSnapshotNotPending) {
		t.Fatalf("second completion err = %v", err)
	}
}

func TestDecisionSnapshotCompletionIsAtomic(t *testing.T) {
	ctx := context.Background()
	id := newPendingDecisionSnapshot(t, "DSNAPB")
	// Duplicate primary key on the second call aborts the whole transaction.
	err := store.CompleteDecisionSnapshot(ctx, id, store.DecisionCompletion{
		Outcome: store.DecisionOutcomeCompleted, FinalOutput: json.RawMessage(`{}`), OutputHash: "out",
		StageCalls: []store.DecisionStageCall{
			{Stage: "analyst", Ordinal: 0, Provider: "a", RequestedModel: "m"},
			{Stage: "analyst", Ordinal: 0, Provider: "a", RequestedModel: "m"},
		},
	})
	if err == nil {
		t.Fatal("expected duplicate stage call to fail")
	}
	snap, _ := store.GetDecisionSnapshot(ctx, id)
	calls, _ := store.GetDecisionStageCalls(ctx, id)
	if snap.Outcome != store.DecisionOutcomePending || len(calls) != 0 {
		t.Fatalf("partial completion persisted: outcome %s, %d calls", snap.Outcome, len(calls))
	}
}

func TestDecisionSnapshotInsertAndCompletionValidation(t *testing.T) {
	ctx := context.Background()
	base := store.DecisionSnapshot{
		SchemaVersion: 1, Ticker: "DSNAPC", Timeframe: "1d", DecisionCutoff: time.Now(), CodeRevision: "t",
		Workflow: "single", PolicyVersion: "p", PromptVersion: "t", InputHash: "i", DecisionHash: "d",
		Inputs: json.RawMessage(`{}`), EffectiveConfig: json.RawMessage(`{}`), ConfigHash: "c",
	}
	for _, bad := range []store.DecisionSnapshot{
		func() store.DecisionSnapshot { s := base; s.Outcome = store.DecisionOutcomeCompleted; return s }(),
		func() store.DecisionSnapshot { s := base; s.Outcome = store.DecisionOutcomeShortCircuited; return s }(),
	} {
		if _, err := store.InsertDecisionSnapshot(ctx, bad); err == nil {
			t.Fatalf("insert accepted invalid outcome %q/%q", bad.Outcome, bad.OutcomeReason)
		}
	}
	sc := base
	sc.Outcome, sc.OutcomeReason = store.DecisionOutcomeShortCircuited, "model_unavailable"
	id, err := store.InsertDecisionSnapshot(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetDecisionSnapshot(ctx, id); got.CompletedAt == nil {
		t.Fatal("a short-circuited snapshot is settled at insert")
	}
	pending := newPendingDecisionSnapshot(t, "DSNAPD")
	if err := store.CompleteDecisionSnapshot(ctx, pending, store.DecisionCompletion{Outcome: store.DecisionOutcomeFailed}); err == nil {
		t.Fatal("failed completion without a reason must be rejected")
	}
	// The database rejects a completed row without output even if the caller
	// bypasses the Go validation.
	_, err = store.GetDB().Exec(ctx,
		`UPDATE decision_snapshots SET outcome = 'completed', completed_at = now() WHERE id = $1`, pending)
	requireDBError(t, err, "decision_snapshots_completed_output")
}

func TestDecisionSnapshotsSurviveAnalysisRetention(t *testing.T) {
	ctx := context.Background()
	id := newPendingDecisionSnapshot(t, "DSNAPE")
	if _, err := store.CleanupPromptCache(ctx); err != nil {
		t.Fatalf("CleanupPromptCache: %v", err)
	}
	if got, _ := store.GetDecisionSnapshot(ctx, id); got == nil {
		t.Fatal("analysis retention must not touch decision snapshots")
	}
}
