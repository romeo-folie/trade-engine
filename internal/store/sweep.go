package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SweepRun is one orb-sweep invocation (see migration 037_sweep_runs.sql).
type SweepRun struct {
	ID         string
	ParentID   *string
	Mode       string
	PeriodFrom time.Time
	PeriodTo   time.Time
	ParamCount int
	Notes      string
	CreatedAt  time.Time
}

// SweepResult is one parameter set's outcome for a (period split × instrument scope).
type SweepResult struct {
	SweepID            string
	ParameterSetHash   string
	ResolvedParameters json.RawMessage
	InstrumentScope    string // "ALL" (portfolio) or a symbol
	PeriodSplit        string // train | validate | forward | full
	Metrics            json.RawMessage
	Trades             int
	RankScore          float64
}

// CreateSweepRun inserts a run header and returns its generated id. parentID is nil for a
// top-level coarse/full run and set for a refine/validate run derived from a parent.
func CreateSweepRun(ctx context.Context, db *pgxpool.Pool, mode string, from, to time.Time, parentID *string, notes string) (string, error) {
	var id string
	err := db.QueryRow(ctx, `
		INSERT INTO sweep_runs (mode, period_from, period_to, parent_id, notes)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		mode, from, to, parentID, notes,
	).Scan(&id)
	return id, err
}

// SetSweepRunParamCount records how many parameter sets the run evaluated.
func SetSweepRunParamCount(ctx context.Context, db *pgxpool.Pool, id string, n int) error {
	_, err := db.Exec(ctx, `UPDATE sweep_runs SET param_count = $1 WHERE id = $2`, n, id)
	return err
}

// InsertSweepResults writes a batch of results in a single round-trip. All rows share the run's
// sweep_id (set by the caller on each SweepResult).
func InsertSweepResults(ctx context.Context, db *pgxpool.Pool, results []SweepResult) error {
	if len(results) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, r := range results {
		batch.Queue(`
			INSERT INTO sweep_results
			  (sweep_id, parameter_set_hash, resolved_parameters, instrument_scope, period_split, metrics, trades, rank_score)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			r.SweepID, r.ParameterSetHash, r.ResolvedParameters, r.InstrumentScope, r.PeriodSplit, r.Metrics, r.Trades, r.RankScore)
	}
	br := db.SendBatch(ctx, batch)
	defer br.Close()
	for range results {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}

// TopSweepResults returns a run's results for a scope+split ordered by rank_score (best first).
// Used to report a coarse run and to seed a refine run from its survivors.
func TopSweepResults(ctx context.Context, db *pgxpool.Pool, sweepID, scope, split string, limit int) ([]SweepResult, error) {
	rows, err := db.Query(ctx, `
		SELECT sweep_id, parameter_set_hash, resolved_parameters, instrument_scope, period_split, metrics, trades, rank_score
		FROM sweep_results
		WHERE sweep_id = $1 AND instrument_scope = $2 AND period_split = $3
		ORDER BY rank_score DESC NULLS LAST
		LIMIT $4`,
		sweepID, scope, split, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SweepResult
	for rows.Next() {
		var r SweepResult
		if err := rows.Scan(&r.SweepID, &r.ParameterSetHash, &r.ResolvedParameters, &r.InstrumentScope,
			&r.PeriodSplit, &r.Metrics, &r.Trades, &r.RankScore); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
