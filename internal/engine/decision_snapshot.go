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
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// minStaleDecisionSnapshotAge is the floor for how long a decision snapshot
// may stay pending before recovery settles it as abandoned. The effective
// threshold also scales with configured model timeouts
// (staleDecisionSnapshotAge), and this process's in-flight snapshots are
// never reaped regardless of age.
const minStaleDecisionSnapshotAge = 30 * time.Minute

// defaultModelTimeout mirrors the analyzer's fallback when timeout_sec is 0.
const defaultModelTimeout = 30 * time.Second

// staleDecisionSnapshotAge returns an abandonment threshold that a live
// analysis in another process cannot reach: twice the sum of every configured
// model timeout (an upper bound on any sequential analyst, specialist,
// fallback, and arbiter chain), and never below the floor.
func staleDecisionSnapshotAge(cfg *config.Config) time.Duration {
	if cfg == nil {
		return minStaleDecisionSnapshotAge
	}
	timeout := func(m config.ModelConfig) time.Duration {
		if m.TimeoutSec > 0 {
			return time.Duration(m.TimeoutSec) * time.Second
		}
		return defaultModelTimeout
	}
	var total time.Duration
	for _, m := range cfg.Models.AnalystConfigs() {
		total += timeout(m)
	}
	if arb := cfg.Models.ArbiterConfig(); arb.Provider != "" {
		total += timeout(arb)
	}
	for _, entry := range cfg.Models.Registry {
		total += timeout(entry.ModelConfig)
	}
	if age := 2 * total; age > minStaleDecisionSnapshotAge {
		return age
	}
	return minStaleDecisionSnapshotAge
}

// decisionSnapshotReapInterval is how often stale pending snapshots are
// settled while the engine runs, so a crash-and-quick-restart (within
// staleDecisionSnapshotAge) is still recovered without another restart.
const decisionSnapshotReapInterval = 15 * time.Minute

// decisionSnapshotWriteTimeout bounds each snapshot write so a slow or
// stalled database cannot delay the live decision. A timed-out completion
// leaves the row pending for the reaper.
const decisionSnapshotWriteTimeout = 2 * time.Second

// decisionSnapshotRecoveryTimeout bounds the stale-snapshot sweep, which runs
// synchronously before the first tick.
const decisionSnapshotRecoveryTimeout = 5 * time.Second

// shortCircuitSnapshotInterval throttles short-circuit snapshots per
// (ticker, reason). These rows carry no model evidence, are immutable, and are
// exempt from retention; one per episode is enough to show a candidate was
// skipped, and a model outage must not write a row per ticker per tick.
const shortCircuitSnapshotInterval = time.Hour

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
	inputsRaw, inputHash, err := snapshot.EncodeInputs(inputs)
	if err != nil {
		return "", err
	}
	missingRaw, err := json.Marshal(sortedMissing(missing))
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, decisionSnapshotWriteTimeout)
	defer cancel()
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
	e.inflightSnapshotsMu.Lock()
	if e.inflightSnapshots == nil {
		e.inflightSnapshots = make(map[string]struct{})
	}
	e.inflightSnapshots[id] = struct{}{}
	e.inflightSnapshotsMu.Unlock()
	return id
}

// inflightSnapshotIDs lists this process's snapshots whose analysis is still
// running; recovery never abandons them, however long they take.
func (e *Engine) inflightSnapshotIDs() []string {
	e.inflightSnapshotsMu.Lock()
	defer e.inflightSnapshotsMu.Unlock()
	ids := make([]string, 0, len(e.inflightSnapshots))
	for id := range e.inflightSnapshots {
		ids = append(ids, id)
	}
	return ids
}

// recordShortCircuitSnapshot records a decision attempt that ended before any
// model call, with the inputs known at that point.
func (e *Engine) recordShortCircuitSnapshot(
	ctx context.Context, ticker, timeframe, strategy string, strategies []string,
	cutoff time.Time, scannerRunID, reason string,
) {
	if !store.IsConnected() || !e.shortCircuitSnapshotDue(ticker, reason, cutoff) {
		return
	}
	inputs := snapshot.Inputs{
		Ticker: ticker, Timeframe: timeframe, Strategy: strategy, Strategies: strategies,
		DecisionCutoff: cutoff.UTC(),
	}
	missing := []snapshot.MissingBlock{{Block: "model_inputs", Reason: "not built: " + reason}}
	if _, err := e.insertDecisionSnapshot(ctx, inputs, missing, scannerRunID,
		store.DecisionOutcomeShortCircuited, reason); err != nil {
		// Not marked recorded: the next occurrence retries instead of being
		// suppressed for a whole interval.
		logger.Warn("decision snapshot: short-circuit record failed", "ticker", ticker, "err", err)
		return
	}
	e.markShortCircuitSnapshot(ticker, reason, cutoff)
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
	// The analysis is over whether or not this write succeeds; a row left
	// pending by a failed write becomes eligible for the reaper.
	defer func() {
		e.inflightSnapshotsMu.Lock()
		delete(e.inflightSnapshots, id)
		e.inflightSnapshotsMu.Unlock()
	}()
	completion := buildDecisionCompletion(result, final, analysisID, analysisErr, persistErr)
	ctx, cancel := context.WithTimeout(ctx, decisionSnapshotWriteTimeout)
	defer cancel()
	if err := store.CompleteDecisionSnapshot(ctx, id, completion); err != nil {
		level := logger.Warn
		if errors.Is(err, store.ErrDecisionSnapshotNotPending) {
			level = logger.Error
		}
		level("decision snapshot: complete failed — row stays pending until the reaper abandons it",
			"snapshot_id", id, "err", err)
	}
}

// buildDecisionCompletion settles a snapshot even when its evidence cannot be
// encoded: an encoding failure becomes a failed outcome naming the problem,
// never a row left pending.
func buildDecisionCompletion(
	result analysis.AnalyzeResult, final *snapshot.FinalOutput, analysisID string, analysisErr, persistErr error,
) store.DecisionCompletion {
	completion := store.DecisionCompletion{AnalysisID: analysisID}
	calls, callsErr := snapshot.StageCalls(result)
	if callsErr == nil {
		completion.StageCalls = calls
	}
	if final == nil {
		completion.Outcome = store.DecisionOutcomeFailed
		completion.OutcomeReason = "analysis_failed"
		if analysisErr != nil {
			completion.OutcomeReason += ": " + analysisErr.Error()
		}
	} else if raw, hash, err := snapshot.Encode(final); err != nil {
		completion.Outcome = store.DecisionOutcomeFailed
		completion.OutcomeReason = "snapshot_final_output_unencodable: " + err.Error()
	} else {
		completion.Outcome = store.DecisionOutcomeCompleted
		completion.FinalOutput, completion.OutputHash = raw, hash
		if persistErr != nil {
			completion.OutcomeReason = "analysis_persistence_failed: " + persistErr.Error()
		}
	}
	if callsErr != nil {
		completion.OutcomeReason = strings.TrimPrefix(completion.OutcomeReason+"; stage_calls_unencodable: "+callsErr.Error(), "; ")
	}
	return completion
}

// shortCircuitSnapshotDue reports whether a short-circuit for this
// (ticker, reason) has not been recorded within the throttle interval.
func (e *Engine) shortCircuitSnapshotDue(ticker, reason string, now time.Time) bool {
	e.shortCircuitMu.Lock()
	defer e.shortCircuitMu.Unlock()
	last, ok := e.shortCircuitRecorded[ticker+"|"+reason]
	return !ok || now.Sub(last) >= shortCircuitSnapshotInterval
}

// markShortCircuitSnapshot records a successful short-circuit write.
func (e *Engine) markShortCircuitSnapshot(ticker, reason string, now time.Time) {
	e.shortCircuitMu.Lock()
	defer e.shortCircuitMu.Unlock()
	if e.shortCircuitRecorded == nil {
		e.shortCircuitRecorded = make(map[string]time.Time)
	}
	e.shortCircuitRecorded[ticker+"|"+reason] = now
}

// recoverDecisionSnapshots settles snapshots left pending longer than the
// stale threshold. It runs before the first tick and then periodically.
func (e *Engine) recoverDecisionSnapshots(ctx context.Context, now time.Time) {
	e.abandonDecisionSnapshotsBefore(ctx, now.Add(-staleDecisionSnapshotAge(e.cfg)))
}

func (e *Engine) abandonDecisionSnapshotsBefore(ctx context.Context, cutoff time.Time) {
	if !store.IsConnected() {
		return
	}
	exclude := e.inflightSnapshotIDs()
	// Bounded like every snapshot write: recovery is best-effort audit
	// housekeeping and must never block engine startup or a tick.
	ctx, cancel := context.WithTimeout(ctx, decisionSnapshotRecoveryTimeout)
	defer cancel()
	n, err := store.AbandonStaleDecisionSnapshots(ctx, cutoff, exclude)
	if err != nil {
		logger.Warn("decision snapshot: startup recovery failed", "err", err)
		return
	}
	if n > 0 {
		logger.Warn("decision snapshot: abandoned stale pending snapshots", "count", n)
	}
}

// runDecisionSnapshotReaper settles stale pending snapshots while running.
func (e *Engine) runDecisionSnapshotReaper(ctx context.Context) {
	ticker := time.NewTicker(decisionSnapshotReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			e.recoverDecisionSnapshots(ctx, now)
		}
	}
}
