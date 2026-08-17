package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type PoliticalDisclosureReport struct {
	Source            string
	ExternalID        string
	Chamber           string
	FilerName         string
	Office            string
	ReportType        string
	FilingDate        time.Time
	DocumentURL       string
	ContentHash       string
	ExtractionStatus  string
	ExtractionError   string
	RetryableFailure  bool
	RetryAttemptCount int
	NextRetryAt       *time.Time
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
	FilingDate          time.Time
	DocumentURL         string
	ExtractionStatus    string
	TransactionCount    int
	LatestTransaction   time.Time
	Evidence            json.RawMessage
	PromotionClaimToken string
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
		SELECT r.source, r.external_id, r.filer_name, COALESCE(r.office,''),
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
			&item.Source, &item.ExternalID, &item.FilerName, &item.Office,
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
			report_type, filing_date, document_url, COALESCE(content_hash,''),
			extraction_status, COALESCE(extraction_error,''), retryable_failure,
			retry_attempt_count, next_retry_at, raw_metadata
		FROM political_disclosure_reports
		WHERE source=$1 AND external_id=$2`,
		strings.ToLower(strings.TrimSpace(source)), strings.TrimSpace(externalID),
	).Scan(
		&report.Source, &report.ExternalID, &report.Chamber, &report.FilerName,
		&report.Office, &report.ReportType, &report.FilingDate,
		&report.DocumentURL, &report.ContentHash, &report.ExtractionStatus,
		&report.ExtractionError, &report.RetryableFailure,
		&report.RetryAttemptCount, &report.NextRetryAt, &rawMetadata,
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
				NOT retryable_failure OR next_retry_at IS NULL OR next_retry_at > NOW()
			))
		FROM political_disclosure_reports
		WHERE source=$1 AND external_id=$2`, source, externalID,
	).Scan(&terminal)
	if IsNoRows(err) {
		return false, nil
	}
	return terminal, err
}

// RetryPoliticalDisclosureReport explicitly re-queues an exhausted or
// operator-reviewed report. The next sync that sees the filing will ingest it.
func RetryPoliticalDisclosureReport(
	ctx context.Context, source, externalID string,
) (bool, error) {
	result, err := GetDB().Exec(ctx, `
		UPDATE political_disclosure_reports
		SET extraction_status='pending', extraction_error=NULL,
			retryable_failure=FALSE, retry_attempt_count=0, next_retry_at=NULL,
			updated_at=NOW()
		WHERE source=$1 AND external_id=$2 AND extraction_status='failed'`,
		strings.ToLower(strings.TrimSpace(source)), strings.TrimSpace(externalID),
	)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() > 0, nil
}

// SavePoliticalDisclosureReport replaces extracted child evidence atomically.
// Existing terminal promotion outcomes are retained when an upstream filing is
// re-extracted; removed tickers are marked superseded for auditability.
func SavePoliticalDisclosureReport(
	ctx context.Context, report PoliticalDisclosureReport,
) error {
	if strings.TrimSpace(report.ExternalID) == "" ||
		strings.TrimSpace(report.FilerName) == "" {
		return fmt.Errorf("political disclosure report identity is incomplete")
	}
	if len(report.RawMetadata) == 0 {
		report.RawMetadata = json.RawMessage(`{}`)
	}
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	retryableFailure := report.ExtractionStatus == "failed" && report.RetryableFailure
	retryAttempts := 0
	var nextRetryAt *time.Time
	if retryableFailure {
		retryAttempts = 1
		next := time.Now().UTC().Add(politicalDisclosureRetryBackoff(retryAttempts))
		nextRetryAt = &next
	}

	var reportID string
	err = tx.QueryRow(ctx, `
		INSERT INTO political_disclosure_reports (
			source, external_id, chamber, filer_name, office, report_type,
			filing_date, document_url, content_hash, extraction_status,
			extraction_error, retryable_failure, retry_attempt_count, next_retry_at, raw_metadata
		) VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,$8,NULLIF($9,''),$10,NULLIF($11,''),$12,$13,$14,$15)
		ON CONFLICT (source, external_id) DO UPDATE SET
			chamber=EXCLUDED.chamber, filer_name=EXCLUDED.filer_name,
			office=EXCLUDED.office, report_type=EXCLUDED.report_type,
			filing_date=EXCLUDED.filing_date, document_url=EXCLUDED.document_url,
			content_hash=EXCLUDED.content_hash,
			extraction_status=EXCLUDED.extraction_status,
			extraction_error=EXCLUDED.extraction_error,
			retry_attempt_count=CASE
				WHEN EXCLUDED.extraction_status='failed' AND EXCLUDED.retryable_failure THEN
					CASE WHEN political_disclosure_reports.extraction_status='failed'
						THEN political_disclosure_reports.retry_attempt_count + 1 ELSE 1 END
				ELSE 0 END,
			retryable_failure=CASE
				WHEN EXCLUDED.extraction_status='failed' AND EXCLUDED.retryable_failure THEN
					(CASE WHEN political_disclosure_reports.extraction_status='failed'
						THEN political_disclosure_reports.retry_attempt_count + 1 ELSE 1 END) < $16
				ELSE FALSE END,
			next_retry_at=CASE
				WHEN EXCLUDED.extraction_status='failed' AND EXCLUDED.retryable_failure AND
					(CASE WHEN political_disclosure_reports.extraction_status='failed'
						THEN political_disclosure_reports.retry_attempt_count + 1 ELSE 1 END) < $16
				THEN NOW() + CASE
					WHEN (CASE WHEN political_disclosure_reports.extraction_status='failed'
						THEN political_disclosure_reports.retry_attempt_count + 1 ELSE 1 END) = 1
						THEN INTERVAL '1 hour'
					WHEN (CASE WHEN political_disclosure_reports.extraction_status='failed'
						THEN political_disclosure_reports.retry_attempt_count + 1 ELSE 1 END) = 2
						THEN INTERVAL '6 hours'
					ELSE INTERVAL '24 hours' END
				ELSE NULL END,
			raw_metadata=EXCLUDED.raw_metadata, updated_at=NOW()
		RETURNING id`, report.Source, report.ExternalID, report.Chamber,
		report.FilerName, report.Office, report.ReportType, report.FilingDate,
		report.DocumentURL, report.ContentHash, report.ExtractionStatus,
		report.ExtractionError, retryableFailure, retryAttempts, nextRetryAt,
		report.RawMetadata, MaxPoliticalDisclosureRetryAttempts,
	).Scan(&reportID)
	if err != nil {
		return fmt.Errorf("upsert political disclosure report: %w", err)
	}
	if report.ExtractionStatus != "completed" {
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
			"latest report extraction status is "+report.ExtractionStatus,
		); err != nil {
			return fmt.Errorf("supersede political disclosure candidates: %w", err)
		}
		return tx.Commit(ctx)
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
		SET promotion_status='superseded', promotion_reason='removed by filing re-extraction'
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
					ELSE political_disclosure_candidates.promotion_status END,
				promotion_claim_token=CASE WHEN political_disclosure_candidates.promotion_status='superseded' THEN NULL ELSE political_disclosure_candidates.promotion_claim_token END,
				promotion_claimed_at=CASE WHEN political_disclosure_candidates.promotion_status='superseded' THEN NULL ELSE political_disclosure_candidates.promotion_claimed_at END,
				scanner_run_id=CASE WHEN political_disclosure_candidates.promotion_status='superseded' THEN NULL ELSE political_disclosure_candidates.scanner_run_id END,
				promoted_at=CASE WHEN political_disclosure_candidates.promotion_status='superseded' THEN NULL ELSE political_disclosure_candidates.promoted_at END,
				promotion_reason=CASE WHEN political_disclosure_candidates.promotion_status='superseded' THEN NULL ELSE political_disclosure_candidates.promotion_reason END`,
			reportID, ticker, len(group.transactions), group.latest, evidence,
		)
		if err != nil {
			return fmt.Errorf("upsert political disclosure candidate %s: %w", ticker, err)
		}
	}
	return tx.Commit(ctx)
}

func ClaimPoliticalDisclosureCandidates(
	ctx context.Context, since, now time.Time, lease time.Duration,
	maxTickers int, token string,
) ([]PoliticalDisclosureCandidate, error) {
	rows, err := GetDB().Query(ctx, `
		WITH selected AS (
			SELECT c.ticker
			FROM political_disclosure_candidates c
			JOIN political_disclosure_reports r ON r.id=c.report_id
			WHERE r.filing_date >= $1::date
			  AND (c.promotion_status='pending' OR (
				c.promotion_status='processing' AND
				c.promotion_claimed_at < $2::timestamptz - $3::interval
			  ))
			GROUP BY c.ticker
			ORDER BY MAX(r.filing_date) DESC, c.ticker
			LIMIT $4
		), claimed AS (
			UPDATE political_disclosure_candidates c
			SET promotion_status='processing', promotion_claim_token=$5,
				promotion_claimed_at=$2::timestamptz
			FROM selected s, political_disclosure_reports r
			WHERE c.ticker=s.ticker AND c.report_id=r.id
			  AND r.filing_date >= $1::date
			  AND (c.promotion_status='pending' OR (
				c.promotion_status='processing' AND
				c.promotion_claimed_at < $2::timestamptz - $3::interval
			))
			RETURNING c.*
		)
		SELECT claimed.report_id, r.external_id, claimed.ticker, r.source,
			r.chamber, r.filer_name, COALESCE(r.office,''), r.report_type, r.filing_date,
			r.document_url, r.extraction_status, claimed.transaction_count,
			claimed.latest_transaction_date, claimed.evidence,
			COALESCE(claimed.promotion_claim_token,'')
		FROM claimed
		JOIN political_disclosure_reports r ON r.id=claimed.report_id
		ORDER BY claimed.ticker, r.filing_date DESC, claimed.report_id`,
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
			&candidate.Office, &candidate.ReportType, &candidate.FilingDate, &candidate.DocumentURL,
			&candidate.ExtractionStatus,
			&candidate.TransactionCount, &candidate.LatestTransaction,
			&candidate.Evidence, &candidate.PromotionClaimToken,
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
	ctx context.Context, ticker, token, status, scannerRunID, reason string,
	at time.Time,
) error {
	result, err := GetDB().Exec(ctx, `
		UPDATE political_disclosure_candidates
		SET promotion_status=$3, scanner_run_id=NULLIF($4,'')::uuid,
			promoted_at=CASE WHEN $3='promoted' THEN $6 ELSE promoted_at END,
			promotion_reason=$5, promotion_claim_token=NULL,
			promotion_claimed_at=NULL
		WHERE ticker=$1 AND promotion_status='processing'
		  AND promotion_claim_token=$2`, ticker, token, status, scannerRunID, reason, at)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return &ScannerSourcePromotionClaimError{Source: "political_disclosure"}
	}
	return nil
}
