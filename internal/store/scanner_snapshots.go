package store

import (
	"context"
	"time"
)

// ScannerRun captures the configuration and aggregate outcome of one scan.
type ScannerRun struct {
	ID                           string
	StartedAt                    time.Time
	CompletedAt                  time.Time
	Status                       string
	Error                        string
	PullbackUniverseEnabled      bool
	PullbackMinMomentumReturnPct float64
	PullbackMinMarketCapMM       float64
	PullbackMinDollarVolume      float64
	CandidateCap                 int
	MinConviction                float64
	TTLDays                      int
	RawCandidateCount            int
	EvaluatedCandidateCount      int
	ScoredCandidateCount         int
	AddedCandidateCount          int
	ExpiredItemCount             int
}

// ScannerCandidateSnapshot records every stage reached by a sourced candidate.
type ScannerCandidateSnapshot struct {
	ScannerRunID             string
	Ticker                   string
	Sources                  []string
	GraphContext             string
	Price                    float64
	ChangePct                float64
	Volume                   int64
	TradeCount               int64
	DollarVolume             float64
	MarketCapMM              float64
	Return1M                 float64
	Return3M                 float64
	Return6M                 float64
	MomentumComposite        float64
	MomentumRank             int
	QualificationKnown       bool
	PullbackQualified        bool
	QualificationStage       string
	QualificationReason      string
	QuarantineRequired       bool
	ActiveWatchlistDuplicate bool
	EligibleForScoring       bool
	LLMAction                string
	LLMConviction            float64
	LLMSetupType             string
	LLMStrategy              string
	LLMTimeframes            []string
	LLMReasoning             string
	LLMRisks                 []string
	LLMError                 string
	PassedMinConviction      bool
	CapSelected              bool
	WatchlistAdded           bool
	WatchlistError           string
	Strategies               []string
	FinalStage               string
}

func BeginScannerRun(ctx context.Context, run ScannerRun) (string, error) {
	var id string
	err := GetDB().QueryRow(ctx, `
		INSERT INTO scanner_runs (
			started_at, pullback_universe_enabled,
			pullback_min_momentum_return_pct, pullback_min_market_cap_mm,
			pullback_min_dollar_volume, candidate_cap, min_conviction, ttl_days
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id`,
		run.StartedAt, run.PullbackUniverseEnabled,
		run.PullbackMinMomentumReturnPct, run.PullbackMinMarketCapMM,
		run.PullbackMinDollarVolume, run.CandidateCap, run.MinConviction, run.TTLDays,
	).Scan(&id)
	return id, err
}

func FinishScannerRun(ctx context.Context, run ScannerRun, candidates []ScannerCandidateSnapshot) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	for _, c := range candidates {
		if c.Sources == nil {
			c.Sources = []string{}
		}
		if c.LLMTimeframes == nil {
			c.LLMTimeframes = []string{}
		}
		if c.LLMRisks == nil {
			c.LLMRisks = []string{}
		}
		if c.Strategies == nil {
			c.Strategies = []string{}
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO scanner_candidate_snapshots (
				scanner_run_id, ticker, sources, graph_context, price, change_pct,
				volume, trade_count, dollar_volume, market_cap_mm,
				return_1m, return_3m, return_6m, momentum_composite, momentum_rank,
				qualification_known, pullback_qualified, qualification_stage,
				qualification_reason, quarantine_required, active_watchlist_duplicate,
				eligible_for_scoring, llm_action, llm_conviction, llm_setup_type,
				llm_strategy, llm_timeframes, llm_reasoning, llm_risks, llm_error,
				passed_min_conviction, cap_selected, watchlist_added, watchlist_error,
				strategies, final_stage
			) VALUES (
				$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,
				$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,
				$35,$36
			)`,
			c.ScannerRunID, c.Ticker, c.Sources, nullableString(c.GraphContext),
			nullableFloat(c.Price), nullableFloat(c.ChangePct), c.Volume, c.TradeCount,
			nullableFloat(c.DollarVolume), nullableFloat(c.MarketCapMM),
			nullableFloat(c.Return1M), nullableFloat(c.Return3M), nullableFloat(c.Return6M),
			nullableFloat(c.MomentumComposite), nullableInt(c.MomentumRank),
			c.QualificationKnown, c.PullbackQualified, c.QualificationStage,
			nullableString(c.QualificationReason), c.QuarantineRequired,
			c.ActiveWatchlistDuplicate, c.EligibleForScoring,
			nullableString(c.LLMAction), nullableFloat(c.LLMConviction),
			nullableString(c.LLMSetupType), nullableString(c.LLMStrategy),
			c.LLMTimeframes, nullableString(c.LLMReasoning), c.LLMRisks,
			nullableString(c.LLMError), c.PassedMinConviction, c.CapSelected,
			c.WatchlistAdded, nullableString(c.WatchlistError), c.Strategies, c.FinalStage,
		)
		if err != nil {
			return err
		}
	}

	_, err = tx.Exec(ctx, `
		UPDATE scanner_runs SET
			completed_at=$1, status=$2, error=$3, raw_candidate_count=$4,
			evaluated_candidate_count=$5, scored_candidate_count=$6,
			added_candidate_count=$7, expired_item_count=$8
		WHERE id=$9`,
		run.CompletedAt, run.Status, nullableString(run.Error), run.RawCandidateCount,
		run.EvaluatedCandidateCount, run.ScoredCandidateCount, run.AddedCandidateCount,
		run.ExpiredItemCount, run.ID,
	)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ScannerFunnel summarizes exact scan-to-signal-to-trade conversion.
type ScannerFunnel struct {
	Runs                    int
	Sourced                 int
	QualificationKnown      int
	PullbackQualified       int
	ScoringEligible         int
	Scored                  int
	Recommended             int
	PassedConviction        int
	WatchlistAdded          int
	SignalCandidates        int
	Signals                 int
	ExecutedTradeCandidates int
	Trades                  int
	ClosedTrades            int
	Wins                    int
	RealizedPnL             float64
	QualificationStages     []ScannerStageCount
	FinalStages             []ScannerStageCount
}

type ScannerStageCount struct {
	Stage string
	Count int
}

func GetScannerFunnel(ctx context.Context, from, to time.Time) (ScannerFunnel, error) {
	var f ScannerFunnel
	err := GetDB().QueryRow(ctx, `
		WITH selected_runs AS (
			SELECT id FROM scanner_runs WHERE started_at >= $1 AND started_at < $2
		), candidate_rollup AS (
			SELECT
				COUNT(*) AS sourced,
				COUNT(*) FILTER (WHERE qualification_known) AS qualification_known,
				COUNT(*) FILTER (WHERE pullback_qualified) AS pullback_qualified,
				COUNT(*) FILTER (WHERE eligible_for_scoring) AS scoring_eligible,
				COUNT(*) FILTER (WHERE llm_action IS NOT NULL) AS scored,
				COUNT(*) FILTER (WHERE llm_action = 'add') AS recommended,
				COUNT(*) FILTER (WHERE passed_min_conviction) AS passed_conviction,
				COUNT(*) FILTER (WHERE watchlist_added) AS watchlist_added
			FROM scanner_candidate_snapshots c
			JOIN selected_runs r ON r.id = c.scanner_run_id
		), signal_rollup AS (
			SELECT COUNT(DISTINCT s.scanner_run_id::text || ':' || s.ticker) AS candidates,
			       COUNT(*) AS signals
			FROM signals s JOIN selected_runs r ON r.id = s.scanner_run_id
		), trade_rollup AS (
			SELECT COUNT(DISTINCT s.scanner_run_id::text || ':' || s.ticker) AS candidates,
			       COUNT(*) AS trades,
			       COUNT(*) FILTER (WHERE t.status = 'closed') AS closed,
			       COUNT(*) FILTER (WHERE t.status = 'closed' AND t.pnl > 0) AS wins,
			       COALESCE(SUM(t.pnl) FILTER (WHERE t.status = 'closed'), 0) AS pnl
			FROM trades t
			JOIN signals s ON s.id = t.signal_id
			JOIN selected_runs r ON r.id = s.scanner_run_id
		)
		SELECT
			(SELECT COUNT(*) FROM selected_runs),
			COALESCE(c.sourced,0), COALESCE(c.qualification_known,0),
			COALESCE(c.pullback_qualified,0), COALESCE(c.scoring_eligible,0),
			COALESCE(c.scored,0), COALESCE(c.recommended,0),
			COALESCE(c.passed_conviction,0), COALESCE(c.watchlist_added,0),
			COALESCE(s.candidates,0), COALESCE(s.signals,0),
			COALESCE(t.candidates,0), COALESCE(t.trades,0), COALESCE(t.closed,0),
			COALESCE(t.wins,0), COALESCE(t.pnl,0)
		FROM candidate_rollup c CROSS JOIN signal_rollup s CROSS JOIN trade_rollup t`,
		from, to,
	).Scan(
		&f.Runs, &f.Sourced, &f.QualificationKnown, &f.PullbackQualified,
		&f.ScoringEligible, &f.Scored, &f.Recommended, &f.PassedConviction,
		&f.WatchlistAdded, &f.SignalCandidates, &f.Signals,
		&f.ExecutedTradeCandidates, &f.Trades, &f.ClosedTrades, &f.Wins,
		&f.RealizedPnL,
	)
	if err != nil {
		return f, err
	}
	f.QualificationStages, err = getScannerStageCounts(
		ctx, from, to, "qualification_stage",
	)
	if err != nil {
		return f, err
	}
	f.FinalStages, err = getScannerStageCounts(ctx, from, to, "final_stage")
	return f, err
}

func getScannerStageCounts(
	ctx context.Context, from, to time.Time, dimension string,
) ([]ScannerStageCount, error) {
	if dimension != "qualification_stage" && dimension != "final_stage" {
		return nil, nil
	}
	rows, err := GetDB().Query(ctx, `
		SELECT CASE WHEN $3 = 'qualification_stage'
		            THEN c.qualification_stage ELSE c.final_stage END AS stage,
		       COUNT(*)
		FROM scanner_candidate_snapshots c
		JOIN scanner_runs r ON r.id = c.scanner_run_id
		WHERE r.started_at >= $1 AND r.started_at < $2
		GROUP BY stage
		ORDER BY COUNT(*) DESC, stage`,
		from, to, dimension,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var counts []ScannerStageCount
	for rows.Next() {
		var count ScannerStageCount
		if err := rows.Scan(&count.Stage, &count.Count); err != nil {
			return nil, err
		}
		counts = append(counts, count)
	}
	return counts, rows.Err()
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}
