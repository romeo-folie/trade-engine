package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrPoliticalDisclosureRetryLeaseLost = errors.New("political disclosure retry claim is no longer active")

type PoliticalDisclosureReport struct {
	Source            string
	ExternalID        string
	Chamber           string
	FilerName         string
	Office            string
	ReportType        string
	IsAmendment       bool
	FilingDate        time.Time
	DocumentURL       string
	ContentHash       string
	ExtractionStatus  string
	ExtractionError   string
	RetryableFailure  bool
	RetryAttemptCount int
	NextRetryAt       *time.Time
	RetryClaimToken   string
	RetryClaimedAt    *time.Time
	RetryLeaseUntil   *time.Time
	RawMetadata       json.RawMessage
	Transactions      []PoliticalDisclosureTransaction
}

type PoliticalDisclosureTransaction struct {
	Ordinal          int
	Owner            string
	AssetName        string
	Ticker           string
	AssetType        string
	TransactionType  string
	TransactionDate  time.Time
	NotificationDate *time.Time
	AmountMin        *int64
	AmountMax        *int64
	AmountText       string
	Comment          string
	RawData          json.RawMessage
}

type PoliticalDisclosureCandidate struct {
	ReportID            string
	ExternalID          string
	Ticker              string
	Source              string
	Chamber             string
	FilerName           string
	Office              string
	ReportType          string
	IsAmendment         bool
	FilingDate          time.Time
	DocumentURL         string
	ExtractionStatus    string
	TransactionCount    int
	LatestTransaction   time.Time
	Evidence            json.RawMessage
	PromotionClaimToken string
	Claimed             bool
}

type PoliticalDisclosureReportFilter struct {
	From   time.Time
	To     time.Time
	Source string
	Filer  string
	Ticker string
	Status string
	Limit  int
}

type PoliticalDisclosureReportSummary struct {
	Source            string
	ExternalID        string
	FilerName         string
	Office            string
	IsAmendment       bool
	FilingDate        time.Time
	DocumentURL       string
	ExtractionStatus  string
	ExtractionError   string
	RetryableFailure  bool
	RetryAttemptCount int
	NextRetryAt       *time.Time
	TransactionCount  int
	DistinctTickers   int
	Tickers           []string
}

func ListPoliticalDisclosureReports(
	ctx context.Context, filter PoliticalDisclosureReportFilter,
) ([]PoliticalDisclosureReportSummary, int, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := GetDB().Query(ctx, `
		SELECT r.source, r.external_id, r.filer_name, COALESCE(r.office,''), r.is_amendment,
			r.filing_date, r.document_url, r.extraction_status,
			COALESCE(r.extraction_error,''), r.retryable_failure,
			r.retry_attempt_count, r.next_retry_at, COUNT(t.id)::int,
			COUNT(DISTINCT t.ticker) FILTER (WHERE t.ticker IS NOT NULL)::int,
			COALESCE(array_agg(DISTINCT t.ticker ORDER BY t.ticker)
				FILTER (WHERE t.ticker IS NOT NULL), ARRAY[]::text[]),
			COUNT(*) OVER()::int
		FROM political_disclosure_reports r
		LEFT JOIN political_disclosure_transactions t ON t.report_id=r.id
		WHERE r.filing_date >= $1::date AND r.filing_date <= $2::date
		  AND ($3='' OR r.source=$3)
		  AND ($4='' OR r.filer_name ILIKE '%' || $4 || '%')
		  AND ($5='' OR r.extraction_status=$5)
		  AND ($6='' OR EXISTS (
			SELECT 1 FROM political_disclosure_transactions matched
			WHERE matched.report_id=r.id AND matched.ticker=$6
		  ))
		GROUP BY r.id
		ORDER BY r.filing_date DESC, r.source, r.filer_name, r.external_id
		LIMIT $7`, filter.From, filter.To, strings.ToLower(strings.TrimSpace(filter.Source)),
		strings.TrimSpace(filter.Filer), strings.ToLower(strings.TrimSpace(filter.Status)),
		strings.ToUpper(strings.TrimSpace(filter.Ticker)), limit,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var summaries []PoliticalDisclosureReportSummary
	total := 0
	for rows.Next() {
		var item PoliticalDisclosureReportSummary
		if err := rows.Scan(
			&item.Source, &item.ExternalID, &item.FilerName, &item.Office, &item.IsAmendment,
			&item.FilingDate, &item.DocumentURL, &item.ExtractionStatus,
			&item.ExtractionError, &item.RetryableFailure,
			&item.RetryAttemptCount, &item.NextRetryAt, &item.TransactionCount,
			&item.DistinctTickers, &item.Tickers, &total,
		); err != nil {
			return nil, 0, err
		}
		summaries = append(summaries, item)
	}
	return summaries, total, rows.Err()
}

func GetPoliticalDisclosureReport(
	ctx context.Context, source, externalID string,
) (*PoliticalDisclosureReport, error) {
	var report PoliticalDisclosureReport
	var rawMetadata []byte
	err := GetDB().QueryRow(ctx, `
		SELECT source, external_id, chamber, filer_name, COALESCE(office,''),
			report_type, is_amendment, filing_date, document_url, COALESCE(content_hash,''),
			extraction_status, COALESCE(extraction_error,''), retryable_failure,
			retry_attempt_count, next_retry_at, COALESCE(retry_claim_token,''), retry_claimed_at, retry_lease_until, raw_metadata
		FROM political_disclosure_reports
		WHERE source=$1 AND external_id=$2`,
		strings.ToLower(strings.TrimSpace(source)), strings.TrimSpace(externalID),
	).Scan(
		&report.Source, &report.ExternalID, &report.Chamber, &report.FilerName,
		&report.Office, &report.ReportType, &report.IsAmendment, &report.FilingDate,
		&report.DocumentURL, &report.ContentHash, &report.ExtractionStatus,
		&report.ExtractionError, &report.RetryableFailure,
		&report.RetryAttemptCount, &report.NextRetryAt, &report.RetryClaimToken, &report.RetryClaimedAt, &report.RetryLeaseUntil, &rawMetadata,
	)
	if IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	report.RawMetadata = append(json.RawMessage(nil), rawMetadata...)
	rows, err := GetDB().Query(ctx, `
		SELECT ordinal, COALESCE(owner,''), asset_name, COALESCE(ticker,''),
			COALESCE(asset_type,''), transaction_type, transaction_date,
			notification_date, amount_min, amount_max, amount_text,
			COALESCE(comment,''), raw_data
		FROM political_disclosure_transactions
		WHERE report_id=(
			SELECT id FROM political_disclosure_reports
			WHERE source=$1 AND external_id=$2
		)
		ORDER BY ordinal`, report.Source, report.ExternalID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item PoliticalDisclosureTransaction
		if err := rows.Scan(
			&item.Ordinal, &item.Owner, &item.AssetName, &item.Ticker,
			&item.AssetType, &item.TransactionType, &item.TransactionDate,
			&item.NotificationDate, &item.AmountMin, &item.AmountMax,
			&item.AmountText, &item.Comment, &item.RawData,
		); err != nil {
			return nil, err
		}
		report.Transactions = append(report.Transactions, item)
	}
	return &report, rows.Err()
}

// MaxPoliticalDisclosureRetryAttempts bounds automatic retries for transient
// disclosure ingestion failures. Further attempts require an operator reset.
const MaxPoliticalDisclosureRetryAttempts = 3

func politicalDisclosureRetryBackoff(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		return time.Hour
	case attempt == 2:
		return 6 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// PoliticalDisclosureReportTerminal reports whether the report should be
// skipped by this automatic sync. Retryable failures are eligible again only
// once their bounded backoff is due; exhausted failures require an explicit
// operator retry.
func PoliticalDisclosureReportTerminal(
	ctx context.Context, source, externalID string,
) (bool, error) {
	var terminal bool
	err := GetDB().QueryRow(ctx, `
		SELECT extraction_status IN ('completed', 'no_text') OR
			(extraction_status='failed' AND (
				NOT retryable_failure OR next_retry_at IS NULL OR next_retry_at > NOW() OR COALESCE(retry_lease_until > NOW(), FALSE)
			))
		FROM political_disclosure_reports
		WHERE source=$1 AND external_id=$2`, source, externalID,
	).Scan(&terminal)
	if IsNoRows(err) {
		return false, nil
	}
	return terminal, err
}

// RetryPoliticalDisclosureReport explicitly makes an exhausted or
// operator-reviewed failure due for the durable retry queue.
func RetryPoliticalDisclosureReport(
	ctx context.Context, source, externalID string,
) (bool, error) {
	result, err := GetDB().Exec(ctx, `
		UPDATE political_disclosure_reports
		SET extraction_status='failed', extraction_error=NULL,
			retryable_failure=TRUE, retry_attempt_count=0, next_retry_at=NOW(),
			retry_claim_token=NULL, retry_claimed_at=NULL, retry_lease_until=NULL,
			updated_at=NOW()
		WHERE source=$1 AND external_id=$2 AND extraction_status='failed'`,
		strings.ToLower(strings.TrimSpace(source)), strings.TrimSpace(externalID),
	)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() > 0, nil
}

// ClaimDuePoliticalDisclosureRetries leases failed reports that are due for
// automatic retry. Claiming records ownership separately from next_retry_at;
// an attempt is counted only when a failure is persisted.
func ClaimDuePoliticalDisclosureRetries(
	ctx context.Context, source string, now time.Time, limit int, lease time.Duration,
) ([]PoliticalDisclosureReport, error) {
	return ClaimPoliticalDisclosureRetries(ctx, source, now, limit, lease,
		fmt.Sprintf("legacy-%d", now.UnixNano()))
}

func ClaimPoliticalDisclosureRetries(
	ctx context.Context, source string, now time.Time, limit int, lease time.Duration, token string,
) ([]PoliticalDisclosureReport, error) {
	if limit <= 0 {
		return nil, nil
	}
	if lease <= 0 {
		return nil, fmt.Errorf("political disclosure retry lease must be positive")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("political disclosure retry claim token is required")
	}
	rows, err := GetDB().Query(ctx, `
		WITH due AS (
			SELECT id FROM political_disclosure_reports
			WHERE source=$1 AND extraction_status='failed' AND retryable_failure
			  AND next_retry_at IS NOT NULL AND next_retry_at <= $2
			  AND (retry_lease_until IS NULL OR retry_lease_until <= $2)
			ORDER BY next_retry_at, filing_date DESC, id
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		), claimed AS (
			UPDATE political_disclosure_reports r
			SET retry_claim_token=$5, retry_claimed_at=$2,
				retry_lease_until=$2 + ($4 * INTERVAL '1 microsecond'), updated_at=NOW()
			FROM due WHERE r.id=due.id
			RETURNING r.source, r.external_id, r.chamber, r.filer_name,
				r.office, r.report_type, r.is_amendment, r.filing_date,
				r.document_url, COALESCE(r.content_hash,'') AS content_hash, r.extraction_status,
				COALESCE(r.extraction_error,'') AS extraction_error, r.retryable_failure,
				r.retry_attempt_count, r.next_retry_at, r.retry_claim_token,
				r.retry_claimed_at, r.retry_lease_until, r.raw_metadata
		)
		SELECT source, external_id, chamber, filer_name, COALESCE(office,''),
			report_type, is_amendment, filing_date, document_url, content_hash,
			extraction_status, extraction_error, retryable_failure,
			retry_attempt_count, next_retry_at, retry_claim_token, retry_claimed_at,
			retry_lease_until, raw_metadata
		FROM claimed ORDER BY next_retry_at, filing_date DESC, external_id`,
		strings.ToLower(strings.TrimSpace(source)), now.UTC(), limit, lease.Microseconds(), token)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reports []PoliticalDisclosureReport
	for rows.Next() {
		var report PoliticalDisclosureReport
		if err := rows.Scan(&report.Source, &report.ExternalID, &report.Chamber,
			&report.FilerName, &report.Office, &report.ReportType, &report.IsAmendment,
			&report.FilingDate, &report.DocumentURL, &report.ContentHash,
			&report.ExtractionStatus, &report.ExtractionError, &report.RetryableFailure,
			&report.RetryAttemptCount, &report.NextRetryAt, &report.RetryClaimToken,
			&report.RetryClaimedAt, &report.RetryLeaseUntil, &report.RawMetadata); err != nil {
			return nil, err
		}
		report.RawMetadata = append(json.RawMessage(nil), report.RawMetadata...)
		reports = append(reports, report)
	}
	return reports, rows.Err()
}

func RenewPoliticalDisclosureRetryLease(ctx context.Context, source, externalID, token string, now time.Time, lease time.Duration) (bool, error) {
	if lease <= 0 {
		return false, fmt.Errorf("political disclosure retry lease must be positive")
	}
	if strings.TrimSpace(token) == "" {
		return false, fmt.Errorf("political disclosure retry claim token is required")
	}
	result, err := GetDB().Exec(ctx, `UPDATE political_disclosure_reports SET retry_lease_until=$4::timestamptz + ($5 * INTERVAL '1 microsecond'), updated_at=NOW() WHERE source=$1 AND external_id=$2 AND retry_claim_token=$3`, strings.ToLower(strings.TrimSpace(source)), strings.TrimSpace(externalID), token, now.UTC(), lease.Microseconds())
	if err != nil {
		return false, err
	}
	return result.RowsAffected() == 1, nil
}

func ReleasePoliticalDisclosureRetryClaims(ctx context.Context, token string) error {
	_, err := GetDB().Exec(ctx, `UPDATE political_disclosure_reports SET retry_claim_token=NULL, retry_claimed_at=NULL, retry_lease_until=NULL, updated_at=NOW() WHERE retry_claim_token=$1`, token)
	return err
}

// SavePoliticalDisclosureRetryResult refuses a stale worker before it can
// overwrite a newer retry result. The successful save clears the claim.
func SavePoliticalDisclosureRetryResult(ctx context.Context, report PoliticalDisclosureReport, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return ErrPoliticalDisclosureRetryLeaseLost
	}
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var currentToken *string
	err = tx.QueryRow(ctx, `
		SELECT retry_claim_token
		FROM political_disclosure_reports
		WHERE source=$1 AND external_id=$2
		FOR UPDATE`,
		strings.ToLower(strings.TrimSpace(report.Source)), strings.TrimSpace(report.ExternalID),
	).Scan(&currentToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPoliticalDisclosureRetryLeaseLost
	}
	if err != nil {
		return err
	}
	if currentToken == nil || *currentToken != token {
		return ErrPoliticalDisclosureRetryLeaseLost
	}
	if err := savePoliticalDisclosureReportTx(ctx, tx, report, false); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SavePoliticalDisclosureFirstPass stores a freshly discovered report and
// fences completed rows against a concurrent loser. A later sync worker that
// saw the filing before this worker committed must not downgrade it after a
// transient fetch/parse failure.
func SavePoliticalDisclosureFirstPass(
	ctx context.Context, report PoliticalDisclosureReport,
) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := savePoliticalDisclosureReportTx(ctx, tx, report, false); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SavePoliticalDisclosureReport replaces extracted child evidence atomically.
// This is the explicit operator-facing upsert, so it intentionally allows a
// completed row to be re-extracted as failed or no_text when an operator
// re-runs a filing.
func SavePoliticalDisclosureReport(
	ctx context.Context, report PoliticalDisclosureReport,
) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := savePoliticalDisclosureReportTx(ctx, tx, report, true); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func savePoliticalDisclosureReportTx(
	ctx context.Context, tx pgx.Tx, report PoliticalDisclosureReport, allowCompletedDowngrade bool,
) error {
	if strings.TrimSpace(report.ExternalID) == "" ||
		strings.TrimSpace(report.FilerName) == "" {
		return fmt.Errorf("political disclosure report identity is incomplete")
	}
	if len(report.RawMetadata) == 0 {
		report.RawMetadata = json.RawMessage(`{}`)
	}
	retryableFailure := report.ExtractionStatus == "failed" && report.RetryableFailure
	retryAttempts := 0
	var nextRetryAt *time.Time
	if retryableFailure {
		retryAttempts = 1
		next := time.Now().UTC().Add(politicalDisclosureRetryBackoff(retryAttempts))
		nextRetryAt = &next
	}

	var reportID, storedStatus string
	err := tx.QueryRow(ctx, `
		INSERT INTO political_disclosure_reports (
			source, external_id, chamber, filer_name, office, report_type,
			filing_date, document_url, content_hash, extraction_status, is_amendment,
			extraction_error, retryable_failure, retry_attempt_count, next_retry_at, raw_metadata
		) VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,$8,NULLIF($9,''),$10,$11,NULLIF($12,''),$13,$14,$15,$16)
		ON CONFLICT (source, external_id) DO UPDATE SET
			chamber=EXCLUDED.chamber, filer_name=EXCLUDED.filer_name,
			office=EXCLUDED.office, report_type=EXCLUDED.report_type,
			is_amendment=EXCLUDED.is_amendment,
			filing_date=EXCLUDED.filing_date, document_url=EXCLUDED.document_url,
			content_hash=CASE
				WHEN NOT $18 AND political_disclosure_reports.extraction_status='completed' AND EXCLUDED.extraction_status <> 'completed'
					THEN political_disclosure_reports.content_hash
				ELSE EXCLUDED.content_hash END,
			extraction_status=CASE
				WHEN NOT $18 AND political_disclosure_reports.extraction_status='completed' AND EXCLUDED.extraction_status <> 'completed'
					THEN political_disclosure_reports.extraction_status
				ELSE EXCLUDED.extraction_status END,
			extraction_error=CASE
				WHEN NOT $18 AND political_disclosure_reports.extraction_status='completed' AND EXCLUDED.extraction_status <> 'completed'
					THEN political_disclosure_reports.extraction_error
				ELSE EXCLUDED.extraction_error END,
			retry_attempt_count=CASE
				WHEN NOT $18 AND political_disclosure_reports.extraction_status='completed' AND EXCLUDED.extraction_status <> 'completed'
					THEN political_disclosure_reports.retry_attempt_count
				ELSE CASE
					WHEN EXCLUDED.extraction_status='failed' AND EXCLUDED.retryable_failure THEN
						CASE WHEN political_disclosure_reports.extraction_status='failed'
							THEN political_disclosure_reports.retry_attempt_count + 1 ELSE 1 END
					ELSE 0 END
			END,
			retryable_failure=CASE
				WHEN NOT $18 AND political_disclosure_reports.extraction_status='completed' AND EXCLUDED.extraction_status <> 'completed'
					THEN political_disclosure_reports.retryable_failure
				ELSE CASE
					WHEN EXCLUDED.extraction_status='failed' AND EXCLUDED.retryable_failure THEN
						(CASE WHEN political_disclosure_reports.extraction_status='failed'
							THEN political_disclosure_reports.retry_attempt_count + 1 ELSE 1 END) < $17
					ELSE FALSE END
			END,
			next_retry_at=CASE
				WHEN NOT $18 AND political_disclosure_reports.extraction_status='completed' AND EXCLUDED.extraction_status <> 'completed'
					THEN political_disclosure_reports.next_retry_at
				ELSE CASE
					WHEN EXCLUDED.extraction_status='failed' AND EXCLUDED.retryable_failure AND
						(CASE WHEN political_disclosure_reports.extraction_status='failed'
							THEN political_disclosure_reports.retry_attempt_count + 1 ELSE 1 END) < $17
					THEN NOW() + CASE
						WHEN (CASE WHEN political_disclosure_reports.extraction_status='failed'
							THEN political_disclosure_reports.retry_attempt_count + 1 ELSE 1 END) = 1
							THEN INTERVAL '1 hour'
						WHEN (CASE WHEN political_disclosure_reports.extraction_status='failed'
							THEN political_disclosure_reports.retry_attempt_count + 1 ELSE 1 END) = 2
							THEN INTERVAL '6 hours'
						ELSE INTERVAL '24 hours' END
					ELSE NULL END
			END,
			raw_metadata=EXCLUDED.raw_metadata,
			retry_claim_token=NULL, retry_claimed_at=NULL, retry_lease_until=NULL, updated_at=NOW()
		RETURNING id, extraction_status`, report.Source, report.ExternalID, report.Chamber,
		report.FilerName, report.Office, report.ReportType, report.FilingDate,
		report.DocumentURL, report.ContentHash, report.ExtractionStatus, report.IsAmendment,
		report.ExtractionError, retryableFailure, retryAttempts, nextRetryAt,
		report.RawMetadata, MaxPoliticalDisclosureRetryAttempts, allowCompletedDowngrade,
	).Scan(&reportID, &storedStatus)
	if err != nil {
		return fmt.Errorf("upsert political disclosure report: %w", err)
	}
	if storedStatus != "completed" {
		if _, err := tx.Exec(ctx,
			`DELETE FROM political_disclosure_transactions WHERE report_id=$1`, reportID,
		); err != nil {
			return fmt.Errorf("clear political disclosure transactions: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE political_disclosure_candidates
			SET transaction_count=0, evidence='[]'::jsonb,
				promotion_status='superseded', promotion_claim_token=NULL,
				promotion_claimed_at=NULL,
				promotion_reason=$2
			WHERE report_id=$1`, reportID,
			"latest report extraction status is "+storedStatus,
		); err != nil {
			return fmt.Errorf("supersede political disclosure candidates: %w", err)
		}
		return nil
	}
	if !allowCompletedDowngrade && storedStatus != report.ExtractionStatus {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM political_disclosure_transactions WHERE report_id=$1`, reportID,
	); err != nil {
		return fmt.Errorf("replace political disclosure transactions: %w", err)
	}

	type candidateEvidence struct {
		transactions []PoliticalDisclosureTransaction
		latest       time.Time
	}
	byTicker := make(map[string]*candidateEvidence)
	for _, item := range report.Transactions {
		if len(item.RawData) == 0 {
			item.RawData = json.RawMessage(`{}`)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO political_disclosure_transactions (
				report_id, ordinal, owner, asset_name, ticker, asset_type,
				transaction_type, transaction_date, notification_date,
				amount_min, amount_max, amount_text, comment, raw_data
			) VALUES ($1,$2,NULLIF($3,''),$4,NULLIF($5,''),NULLIF($6,''),$7,$8,$9,$10,$11,$12,NULLIF($13,''),$14)`,
			reportID, item.Ordinal, item.Owner, item.AssetName, item.Ticker,
			item.AssetType, item.TransactionType, item.TransactionDate,
			item.NotificationDate, item.AmountMin, item.AmountMax, item.AmountText,
			item.Comment, item.RawData,
		)
		if err != nil {
			return fmt.Errorf("insert political disclosure transaction %d: %w", item.Ordinal, err)
		}
		ticker := strings.ToUpper(strings.TrimSpace(item.Ticker))
		if ticker == "" {
			continue
		}
		group := byTicker[ticker]
		if group == nil {
			group = &candidateEvidence{}
			byTicker[ticker] = group
		}
		group.transactions = append(group.transactions, item)
		if item.TransactionDate.After(group.latest) {
			group.latest = item.TransactionDate
		}
	}

	tickers := make([]string, 0, len(byTicker))
	for ticker := range byTicker {
		tickers = append(tickers, ticker)
	}
	sort.Strings(tickers)
	if _, err := tx.Exec(ctx, `
		UPDATE political_disclosure_candidates
		SET promotion_status='superseded', promotion_claim_token=NULL,
			promotion_claimed_at=NULL, scanner_run_id=NULL, promoted_at=NULL,
			promotion_reason='removed by filing re-extraction'
		WHERE report_id=$1 AND NOT (ticker = ANY($2::text[]))`, reportID, tickers,
	); err != nil {
		return fmt.Errorf("supersede removed political disclosure candidates: %w", err)
	}
	for _, ticker := range tickers {
		group := byTicker[ticker]
		evidence, err := json.Marshal(group.transactions)
		if err != nil {
			return fmt.Errorf("marshal political disclosure evidence: %w", err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO political_disclosure_candidates (
				report_id, ticker, transaction_count, latest_transaction_date, evidence
			) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (report_id, ticker) DO UPDATE SET
				transaction_count=EXCLUDED.transaction_count,
				latest_transaction_date=EXCLUDED.latest_transaction_date,
				evidence=EXCLUDED.evidence,
				promotion_status=CASE
					WHEN political_disclosure_candidates.promotion_status='superseded' THEN 'pending'
					WHEN political_disclosure_candidates.promotion_status='processing' AND
						(political_disclosure_candidates.promotion_claim_token IS NOT NULL OR
						 political_disclosure_candidates.promotion_claimed_at IS NOT NULL)
						THEN 'pending'
					ELSE political_disclosure_candidates.promotion_status END,
				promotion_claim_token=CASE
					WHEN political_disclosure_candidates.promotion_status='processing' AND
						(political_disclosure_candidates.promotion_claim_token IS NOT NULL OR
						 political_disclosure_candidates.promotion_claimed_at IS NOT NULL)
						THEN NULL
					WHEN political_disclosure_candidates.promotion_status='superseded' THEN NULL
					ELSE political_disclosure_candidates.promotion_claim_token END,
				promotion_claimed_at=CASE
					WHEN political_disclosure_candidates.promotion_status='processing' AND
						(political_disclosure_candidates.promotion_claim_token IS NOT NULL OR
						 political_disclosure_candidates.promotion_claimed_at IS NOT NULL)
						THEN NULL
					WHEN political_disclosure_candidates.promotion_status='superseded' THEN NULL
					ELSE political_disclosure_candidates.promotion_claimed_at END,
				scanner_run_id=CASE WHEN political_disclosure_candidates.promotion_status='superseded' THEN NULL ELSE political_disclosure_candidates.scanner_run_id END,
				promoted_at=CASE WHEN political_disclosure_candidates.promotion_status='superseded' THEN NULL ELSE political_disclosure_candidates.promoted_at END,
				promotion_reason=CASE
					WHEN political_disclosure_candidates.promotion_status='processing' AND
						(political_disclosure_candidates.promotion_claim_token IS NOT NULL OR
						 political_disclosure_candidates.promotion_claimed_at IS NOT NULL)
						THEN 'evidence replaced while claim active'
					WHEN political_disclosure_candidates.promotion_status='superseded' THEN NULL
					ELSE political_disclosure_candidates.promotion_reason END`,
			reportID, ticker, len(group.transactions), group.latest, evidence,
		)
		if err != nil {
			return fmt.Errorf("upsert political disclosure candidate %s: %w", ticker, err)
		}
	}
	return nil
}

func ClaimPoliticalDisclosureCandidates(
	ctx context.Context, since, now time.Time, lease time.Duration,
	maxTickers int, token string,
) ([]PoliticalDisclosureCandidate, error) {
	rows, err := GetDB().Query(ctx, `
		WITH eligible AS (
			SELECT c.*, r.source, r.external_id, r.chamber, r.filer_name, r.office,
				r.report_type, r.is_amendment, r.filing_date, r.document_url, r.extraction_status
			FROM political_disclosure_candidates c JOIN political_disclosure_reports r ON r.id=c.report_id
			WHERE r.filing_date >= $1::date AND r.filing_date <= $2::date
			  AND r.extraction_status='completed' AND c.promotion_status <> 'superseded'
		), selected AS (
			SELECT c.ticker
			FROM eligible c
			WHERE (c.promotion_status='pending' OR (
				c.promotion_status='processing' AND
				c.promotion_claimed_at < $2::timestamptz - $3::interval
			  ))
			GROUP BY c.ticker
			ORDER BY MAX(c.filing_date) DESC, c.ticker
			LIMIT $4
		), claimed AS (
			UPDATE political_disclosure_candidates c
			SET promotion_status='processing', promotion_claim_token=$5,
				promotion_claimed_at=$2::timestamptz
			FROM selected s
			WHERE c.ticker=s.ticker
			  AND EXISTS (SELECT 1 FROM eligible e WHERE e.report_id=c.report_id AND e.ticker=c.ticker)
			  AND (c.promotion_status='pending' OR (
				c.promotion_status='processing' AND
				c.promotion_claimed_at < $2::timestamptz - $3::interval
			))
			RETURNING c.*
		)
		SELECT e.report_id, e.external_id, e.ticker, e.source,
			e.chamber, e.filer_name, COALESCE(e.office,''), e.report_type, e.is_amendment, e.filing_date,
			e.document_url, e.extraction_status, e.transaction_count,
			e.latest_transaction_date, e.evidence, COALESCE(c.promotion_claim_token,''),
			(c.report_id IS NOT NULL)
		FROM eligible e LEFT JOIN claimed c ON c.report_id=e.report_id AND c.ticker=e.ticker
		WHERE e.ticker IN (SELECT ticker FROM selected)
		ORDER BY e.ticker, e.filing_date DESC, e.report_id`,
		since, now, lease.String(), maxTickers, token,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PoliticalDisclosureCandidate
	for rows.Next() {
		var candidate PoliticalDisclosureCandidate
		if err := rows.Scan(
			&candidate.ReportID, &candidate.ExternalID, &candidate.Ticker,
			&candidate.Source, &candidate.Chamber, &candidate.FilerName,
			&candidate.Office, &candidate.ReportType, &candidate.IsAmendment, &candidate.FilingDate, &candidate.DocumentURL,
			&candidate.ExtractionStatus,
			&candidate.TransactionCount, &candidate.LatestTransaction,
			&candidate.Evidence, &candidate.PromotionClaimToken, &candidate.Claimed,
		); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

func ReleasePoliticalDisclosurePromotionClaims(
	ctx context.Context, token, reason string,
) error {
	_, err := GetDB().Exec(ctx, `
		UPDATE political_disclosure_candidates
		SET promotion_status='pending', promotion_claim_token=NULL,
			promotion_claimed_at=NULL, promotion_reason=$2
		WHERE promotion_status='processing' AND promotion_claim_token=$1`, token, reason)
	return err
}

func MarkPoliticalDisclosurePromotion(
	ctx context.Context, ticker, token string, reportIDs []string,
	status, scannerRunID, reason string, at time.Time,
) error {
	result, err := GetDB().Exec(ctx, `
		UPDATE political_disclosure_candidates
		SET promotion_status=$3, scanner_run_id=NULLIF($4,'')::uuid,
			promoted_at=CASE WHEN $3='promoted' THEN $6 ELSE promoted_at END,
			promotion_reason=$5, promotion_claim_token=NULL,
			promotion_claimed_at=NULL
		WHERE ticker=$1 AND promotion_status='processing'
		  AND promotion_claim_token=$2
		  AND report_id IN (
			SELECT report_id::uuid FROM unnest($7::text[]) expected(report_id)
		  )
		  AND (
			SELECT COUNT(*) FROM political_disclosure_candidates
			WHERE ticker=$1 AND promotion_status='processing'
			  AND promotion_claim_token=$2
			  AND report_id IN (
				SELECT report_id::uuid FROM unnest($7::text[]) expected(report_id)
			  )
		  ) = $8`, ticker, token, status, scannerRunID, reason, at, reportIDs, len(reportIDs))
	if err != nil {
		return err
	}
	if result.RowsAffected() != int64(len(reportIDs)) {
		return &ScannerSourcePromotionClaimError{Source: "political_disclosure"}
	}
	return nil
}
