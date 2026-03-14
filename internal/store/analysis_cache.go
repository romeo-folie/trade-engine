package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// CachedAnalysis mirrors one row in the analysis_cache table.
type CachedAnalysis struct {
	ID           string
	Fingerprint  string
	Ticker       string
	Timeframe    string
	Strategy     string
	ModelProfile string
	AnalysisJSON json.RawMessage
	Decision     string
	Confidence   float64
	ReasonCode   string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	LastUsedAt   time.Time
	UseCount     int
}

// GetCachedAnalysis retrieves a non-expired cached analysis by fingerprint.
// Returns nil, nil when the fingerprint is absent or the entry has expired.
func GetCachedAnalysis(ctx context.Context, fingerprint string) (*CachedAnalysis, error) {
	row := GetDB().QueryRow(ctx, `
		SELECT id, fingerprint, ticker, timeframe, strategy, model_profile,
		       analysis_json, decision, confidence, COALESCE(reason_code, ''),
		       created_at, expires_at, last_used_at, use_count
		FROM   analysis_cache
		WHERE  fingerprint = $1
		  AND  expires_at  > now()
	`, fingerprint)

	var c CachedAnalysis
	err := row.Scan(
		&c.ID, &c.Fingerprint, &c.Ticker, &c.Timeframe, &c.Strategy, &c.ModelProfile,
		&c.AnalysisJSON, &c.Decision, &c.Confidence, &c.ReasonCode,
		&c.CreatedAt, &c.ExpiresAt, &c.LastUsedAt, &c.UseCount,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// UpsertCachedAnalysis inserts or replaces a cache entry by fingerprint.
func UpsertCachedAnalysis(ctx context.Context, entry CachedAnalysis) error {
	var reasonCode *string
	if entry.ReasonCode != "" {
		reasonCode = &entry.ReasonCode
	}
	_, err := GetDB().Exec(ctx, `
		INSERT INTO analysis_cache
		    (fingerprint, ticker, timeframe, strategy, model_profile,
		     analysis_json, decision, confidence, reason_code,
		     created_at, expires_at, last_used_at, use_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now(), $10, now(), 0)
		ON CONFLICT (fingerprint) DO UPDATE SET
		    analysis_json  = EXCLUDED.analysis_json,
		    decision       = EXCLUDED.decision,
		    confidence     = EXCLUDED.confidence,
		    reason_code    = EXCLUDED.reason_code,
		    model_profile  = EXCLUDED.model_profile,
		    expires_at     = EXCLUDED.expires_at,
		    last_used_at   = now(),
		    use_count      = analysis_cache.use_count + 1
	`,
		entry.Fingerprint, entry.Ticker, entry.Timeframe, entry.Strategy, entry.ModelProfile,
		entry.AnalysisJSON, entry.Decision, entry.Confidence, reasonCode,
		entry.ExpiresAt,
	)
	return err
}

// TouchCachedAnalysis updates last_used_at and increments use_count on a hit.
func TouchCachedAnalysis(ctx context.Context, fingerprint string) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE analysis_cache
		SET    last_used_at = now(),
		       use_count    = use_count + 1
		WHERE  fingerprint  = $1
	`, fingerprint)
	return err
}

// DeleteExpiredCachedAnalyses removes all rows whose expires_at ≤ cutoff.
// Returns the number of rows deleted.
func DeleteExpiredCachedAnalyses(ctx context.Context, cutoff time.Time) (int, error) {
	tag, err := GetDB().Exec(ctx,
		`DELETE FROM analysis_cache WHERE expires_at <= $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
