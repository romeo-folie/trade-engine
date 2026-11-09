package engine

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/prompts"
	"github.com/romeo-folie/trade-engine/internal/analysis/snapshot"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// staleDecisionSnapshotAge bounds how long a decision snapshot may stay
// pending before startup recovery settles it as abandoned. It comfortably
// exceeds any analysis deadline (per-model timeouts plus fallback and arbiter
// hops), so a concurrently running process is never mistaken for a crash.
const staleDecisionSnapshotAge = 30 * time.Minute

// Decision snapshots are audit evidence (issue #589). Every function here is
// best-effort: failures are logged and never alter or delay the live decision.

// decisionSnapshotIdentity carries the versioned identity shared by every
// snapshot this engine writes.
type decisionSnapshotIdentity struct {
	config     json.RawMessage
	configHash string
	workflow   string
}

func (e *Engine) decisionSnapshotIdentity() (decisionSnapshotIdentity, error) {
	effective := snapshot.NewEffectiveConfig(e.cfg)
	raw, hash, err := snapshot.Encode(effective)
	if err != nil {
		return decisionSnapshotIdentity{}, err
	}
	return decisionSnapshotIdentity{config: raw, configHash: hash, workflow: effective.Workflow}, nil
}

func sortedMissing(missing []snapshot.MissingBlock) []snapshot.MissingBlock {
	out := slices.Clone(missing)
	slices.SortStableFunc(out, func(a, b snapshot.MissingBlock) int { return strings.Compare(a.Block, b.Block) })
	if out == nil {
		out = []snapshot.MissingBlock{}
	}
	return out
}

func (e *Engine) insertDecisionSnapshot(
	ctx context.Context, inputs snapshot.Inputs, missing []snapshot.MissingBlock,
	scannerRunID, outcome, reason string,
) (string, error) {
	identity, err := e.decisionSnapshotIdentity()
	if err != nil {
		return "", err
	}
	inputsRaw, inputHash, err := snapshot.Encode(inputs)
	if err != nil {
		return "", err
	}
	missingRaw, err := json.Marshal(sortedMissing(missing))
	if err != nil {
		return "", err
	}
	return store.InsertDecisionSnapshot(ctx, store.DecisionSnapshot{
		SchemaVersion:  snapshot.SchemaVersion,
		Ticker:         inputs.Ticker,
		Timeframe:      inputs.Timeframe,
		DecisionCutoff: inputs.DecisionCutoff,
		CodeRevision:   snapshot.CodeRevision(),
		Workflow:       identity.workflow,
		PolicyVersion:  analysis.ResolutionPolicyVersion,
		PromptVersion:  prompts.TemplateVersion,
		InputHash:      inputHash,
		DecisionHash: snapshot.DecisionHash(snapshot.SchemaVersion, inputHash,
			analysis.ResolutionPolicyVersion, prompts.TemplateVersion, identity.configHash),
		Inputs:          inputsRaw,
		Missing:         missingRaw,
		EffectiveConfig: identity.config,
		ConfigHash:      identity.configHash,
		ScannerRunID:    scannerRunID,
		Outcome:         outcome,
		OutcomeReason:   reason,
	})
}

// beginDecisionSnapshot records a pending snapshot of a fully built request
// before any model call. It returns "" when recording is unavailable.
func (e *Engine) beginDecisionSnapshot(
	ctx context.Context, req analysis.AnalysisRequest, missing []snapshot.MissingBlock, scannerRunID string,
) string {
	if !store.IsConnected() {
		return ""
	}
	id, err := e.insertDecisionSnapshot(ctx, snapshot.InputsFromRequest(req), missing,
		scannerRunID, store.DecisionOutcomePending, "")
	if err != nil {
		logger.Warn("decision snapshot: begin failed", "ticker", req.Ticker, "err", err)
		return ""
	}
	return id
}

// recordShortCircuitSnapshot records a decision attempt that ended before any
// model call, with the inputs known at that point.
func (e *Engine) recordShortCircuitSnapshot(
	ctx context.Context, ticker, timeframe, strategy string, strategies []string,
	cutoff time.Time, scannerRunID, reason string,
) {
	if !store.IsConnected() {
		return
	}
	inputs := snapshot.Inputs{
		Ticker: ticker, Timeframe: timeframe, Strategy: strategy, Strategies: strategies,
		DecisionCutoff: cutoff.UTC(),
	}
	missing := []snapshot.MissingBlock{{Block: "model_inputs", Reason: "not built: " + reason}}
	if _, err := e.insertDecisionSnapshot(ctx, inputs, missing, scannerRunID,
		store.DecisionOutcomeShortCircuited, reason); err != nil {
		logger.Warn("decision snapshot: short-circuit record failed", "ticker", ticker, "err", err)
	}
}

// completeDecisionSnapshot settles a pending snapshot with every stage call.
// final is nil for a failed analysis; analysisErr is then required.
func (e *Engine) completeDecisionSnapshot(
	ctx context.Context, id string, result analysis.AnalyzeResult,
	final *snapshot.FinalOutput, analysisID string, analysisErr, persistErr error,
) {
	if id == "" {
		return
	}
	calls, err := snapshot.StageCalls(result)
	if err != nil {
		logger.Warn("decision snapshot: encode stage calls", "snapshot_id", id, "err", err)
		return
	}
	completion := store.DecisionCompletion{StageCalls: calls, AnalysisID: analysisID}
	switch {
	case final == nil:
		completion.Outcome = store.DecisionOutcomeFailed
		completion.OutcomeReason = "analysis_failed"
		if analysisErr != nil {
			completion.OutcomeReason += ": " + analysisErr.Error()
		}
	default:
		raw, hash, err := snapshot.Encode(final)
		if err != nil {
			logger.Warn("decision snapshot: encode final output", "snapshot_id", id, "err", err)
			return
		}
		completion.Outcome = store.DecisionOutcomeCompleted
		completion.FinalOutput, completion.OutputHash = raw, hash
		if persistErr != nil {
			completion.OutcomeReason = "analysis_persistence_failed: " + persistErr.Error()
		}
	}
	if err := store.CompleteDecisionSnapshot(ctx, id, completion); err != nil {
		level := logger.Warn
		if errors.Is(err, store.ErrDecisionSnapshotNotPending) {
			level = logger.Error
		}
		level("decision snapshot: complete failed — row stays pending until startup recovery",
			"snapshot_id", id, "err", err)
	}
}

// recoverDecisionSnapshots settles snapshots left pending by a previous
// process. It runs before the first tick.
func recoverDecisionSnapshots(ctx context.Context, now time.Time) {
	if !store.IsConnected() {
		return
	}
	n, err := store.AbandonStaleDecisionSnapshots(ctx, now.Add(-staleDecisionSnapshotAge))
	if err != nil {
		logger.Warn("decision snapshot: startup recovery failed", "err", err)
		return
	}
	if n > 0 {
		logger.Warn("decision snapshot: abandoned stale pending snapshots", "count", n)
	}
}
