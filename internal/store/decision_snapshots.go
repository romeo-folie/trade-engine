package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Decision snapshot outcomes (issue #589). The lifecycle is:
//
//	short_circuited                       (terminal insert, no model call)
//	pending -> completed | failed         (one transaction with stage calls)
//	pending -> abandoned                  (startup recovery of stale rows)
//
// The database enforces it: settled rows and stage calls are immutable.
const (
	DecisionOutcomePending        = "pending"
	DecisionOutcomeCompleted      = "completed"
	DecisionOutcomeFailed         = "failed"
	DecisionOutcomeShortCircuited = "short_circuited"
	DecisionOutcomeAbandoned      = "abandoned"
)

// ErrDecisionSnapshotNotPending is returned when a completion targets a
// snapshot that is missing or already settled.
var ErrDecisionSnapshotNotPending = errors.New("decision snapshot is not pending")

// DecisionSnapshot is one immutable decision attempt.
type DecisionSnapshot struct {
	ID              string
	SchemaVersion   int
	Ticker          string
	Timeframe       string
	DecisionCutoff  time.Time
	CodeRevision    string
	Workflow        string
	PolicyVersion   string
	PromptVersion   string
	InputHash       string
	DecisionHash    string
	Inputs          json.RawMessage
	Missing         json.RawMessage
	EffectiveConfig json.RawMessage
	ConfigHash      string
	ScannerRunID    string
	Outcome         string
	OutcomeReason   string
	AnalysisID      string
	FinalOutput     json.RawMessage
	OutputHash      string
	CreatedAt       time.Time
	CompletedAt     *time.Time
}

// DecisionStageCall is one model invocation within a decision.
type DecisionStageCall struct {
	Stage          string
	Ordinal        int
	Attempt        int
	Role           string
	Provider       string
	RequestedModel string
	ResolvedModel  string
	RequestParams  json.RawMessage
	RawPrompt      string
	RawResponse    string
	ParsedResponse json.RawMessage
	Error          string
	InputTokens    int
	OutputTokens   int
	LatencyMs      int64
	Extra          json.RawMessage
}

// DecisionCompletion settles a pending snapshot.
type DecisionCompletion struct {
	Outcome       string // completed | failed
	OutcomeReason string
	AnalysisID    string
	FinalOutput   json.RawMessage
	OutputHash    string
	StageCalls    []DecisionStageCall
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

// InsertDecisionSnapshot writes a new snapshot. Outcome must be pending, or
// short_circuited with a reason (which settles it immediately).
func InsertDecisionSnapshot(ctx context.Context, s DecisionSnapshot) (string, error) {
	switch s.Outcome {
	case DecisionOutcomePending:
	case DecisionOutcomeShortCircuited:
		if s.OutcomeReason == "" {
			return "", fmt.Errorf("short-circuited decision snapshot requires a reason")
		}
	default:
		return "", fmt.Errorf("decision snapshot must be inserted pending or short_circuited, got %q", s.Outcome)
	}
	missing := s.Missing
	if len(missing) == 0 {
		missing = json.RawMessage("[]")
	}
	var id string
	err := GetDB().QueryRow(ctx, `
		INSERT INTO decision_snapshots (
			schema_version, ticker, timeframe, decision_cutoff, code_revision,
			workflow, policy_version, prompt_version, input_hash, decision_hash,
			inputs, missing, effective_config, config_hash, scanner_run_id,
			outcome, outcome_reason, completed_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10,
			$11, $12, $13, $14, $15,
			$16, $17, CASE WHEN $16 = 'pending' THEN NULL ELSE now() END
		) RETURNING id`,
		s.SchemaVersion, s.Ticker, s.Timeframe, s.DecisionCutoff.UTC(), s.CodeRevision,
		s.Workflow, s.PolicyVersion, s.PromptVersion, s.InputHash, s.DecisionHash,
		[]byte(s.Inputs), []byte(missing), []byte(s.EffectiveConfig), s.ConfigHash, nullableString(s.ScannerRunID),
		s.Outcome, nullableString(s.OutcomeReason),
	).Scan(&id)
	return id, err
}

// CompleteDecisionSnapshot records every stage call and settles the snapshot
// in one transaction, so a crash leaves either a pending row (later
// abandoned) or a complete record — never a half-written decision.
func CompleteDecisionSnapshot(ctx context.Context, id string, c DecisionCompletion) error {
	if c.Outcome != DecisionOutcomeCompleted && c.Outcome != DecisionOutcomeFailed {
		return fmt.Errorf("decision completion outcome must be completed or failed, got %q", c.Outcome)
	}
	if c.Outcome == DecisionOutcomeFailed && c.OutcomeReason == "" {
		return fmt.Errorf("failed decision completion requires a reason")
	}
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Lock the parent first so a concurrent abandon cannot interleave.
	var outcome string
	if err := tx.QueryRow(ctx,
		`SELECT outcome FROM decision_snapshots WHERE id = $1 FOR UPDATE`, id,
	).Scan(&outcome); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDecisionSnapshotNotPending
		}
		return err
	}
	if outcome != DecisionOutcomePending {
		return ErrDecisionSnapshotNotPending
	}

	for _, call := range c.StageCalls {
		if _, err := tx.Exec(ctx, `
			INSERT INTO decision_stage_calls (
				snapshot_id, stage, ordinal, attempt, role, provider,
				requested_model, resolved_model, request_params, raw_prompt,
				raw_response, parsed_response, error, input_tokens,
				output_tokens, latency_ms, extra
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
			id, call.Stage, call.Ordinal, call.Attempt, nullableString(call.Role), call.Provider,
			call.RequestedModel, nullableString(call.ResolvedModel), nullableJSON(call.RequestParams),
			nullableString(call.RawPrompt), nullableString(call.RawResponse), nullableJSON(call.ParsedResponse),
			nullableString(call.Error), call.InputTokens, call.OutputTokens, call.LatencyMs,
			nullableJSON(call.Extra),
		); err != nil {
			return fmt.Errorf("insert stage call %s/%d/%d: %w", call.Stage, call.Ordinal, call.Attempt, err)
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE decision_snapshots
		SET outcome = $2, outcome_reason = $3, analysis_id = $4,
		    final_output = $5, output_hash = $6, completed_at = now()
		WHERE id = $1 AND outcome = 'pending'`,
		id, c.Outcome, nullableString(c.OutcomeReason), nullableString(c.AnalysisID),
		nullableJSON(c.FinalOutput), nullableString(c.OutputHash),
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AbandonStaleDecisionSnapshots settles pending snapshots created before
// cutoff as abandoned. Called at startup: a pending row that old belongs to a
// process that crashed or failed to write its completion. Stage calls are
// never written for such rows (completion is one transaction), so the record
// is explicitly incomplete rather than reconstructed.
func AbandonStaleDecisionSnapshots(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := GetDB().Exec(ctx, `
		UPDATE decision_snapshots
		SET outcome = 'abandoned',
		    outcome_reason = 'pending at startup recovery; completion was never recorded',
		    completed_at = now()
		WHERE outcome = 'pending' AND created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// GetDecisionSnapshot loads one snapshot by id.
func GetDecisionSnapshot(ctx context.Context, id string) (*DecisionSnapshot, error) {
	var s DecisionSnapshot
	var scannerRunID, reason, analysisID, outputHash *string
	var finalOutput []byte
	err := GetDB().QueryRow(ctx, `
		SELECT id, schema_version, ticker, timeframe, decision_cutoff, code_revision,
		       workflow, policy_version, prompt_version, input_hash, decision_hash,
		       inputs, missing, effective_config, config_hash, scanner_run_id::text,
		       outcome, outcome_reason, analysis_id::text, final_output, output_hash,
		       created_at, completed_at
		FROM decision_snapshots WHERE id = $1`, id,
	).Scan(&s.ID, &s.SchemaVersion, &s.Ticker, &s.Timeframe, &s.DecisionCutoff, &s.CodeRevision,
		&s.Workflow, &s.PolicyVersion, &s.PromptVersion, &s.InputHash, &s.DecisionHash,
		&s.Inputs, &s.Missing, &s.EffectiveConfig, &s.ConfigHash, &scannerRunID,
		&s.Outcome, &reason, &analysisID, &finalOutput, &outputHash,
		&s.CreatedAt, &s.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	s.ScannerRunID, s.OutcomeReason, s.AnalysisID, s.OutputHash = deref(scannerRunID), deref(reason), deref(analysisID), deref(outputHash)
	s.FinalOutput = finalOutput
	return &s, nil
}

// GetDecisionStageCalls loads a snapshot's stage calls in stable order.
func GetDecisionStageCalls(ctx context.Context, snapshotID string) ([]DecisionStageCall, error) {
	rows, err := GetDB().Query(ctx, `
		SELECT stage, ordinal, attempt, COALESCE(role, ''), provider, requested_model,
		       COALESCE(resolved_model, ''), request_params, COALESCE(raw_prompt, ''),
		       COALESCE(raw_response, ''), parsed_response, COALESCE(error, ''),
		       COALESCE(input_tokens, 0), COALESCE(output_tokens, 0), COALESCE(latency_ms, 0), extra
		FROM decision_stage_calls
		WHERE snapshot_id = $1
		ORDER BY CASE stage WHEN 'arbiter' THEN 1 ELSE 0 END, ordinal, attempt`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DecisionStageCall
	for rows.Next() {
		var c DecisionStageCall
		var params, parsed, extra []byte
		if err := rows.Scan(&c.Stage, &c.Ordinal, &c.Attempt, &c.Role, &c.Provider, &c.RequestedModel,
			&c.ResolvedModel, &params, &c.RawPrompt, &c.RawResponse, &parsed, &c.Error,
			&c.InputTokens, &c.OutputTokens, &c.LatencyMs, &extra); err != nil {
			return nil, err
		}
		c.RequestParams, c.ParsedResponse, c.Extra = params, parsed, extra
		out = append(out, c)
	}
	return out, rows.Err()
}
