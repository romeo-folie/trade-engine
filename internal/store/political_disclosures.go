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
	Source           string
	ExternalID       string
	Chamber          string
	FilerName        string
	Office           string
	ReportType       string
	FilingDate       time.Time
	DocumentURL      string
	ContentHash      string
	ExtractionStatus string
	ExtractionError  string
	RawMetadata      json.RawMessage
	Transactions     []PoliticalDisclosureTransaction
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
	Ticker              string
	Source              string
	FilerName           string
	FilingDate          time.Time
	DocumentURL         string
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
	Source           string
	ExternalID       string
	FilerName        string
	Office           string
	FilingDate       time.Time
	DocumentURL      string
	ExtractionStatus string
	ExtractionError  string
	TransactionCount int
	DistinctTickers  int
	Tickers          []string
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
			COALESCE(r.extraction_error,''), COUNT(t.id)::int,
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
			&item.ExtractionError, &item.TransactionCount,
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
			extraction_status, COALESCE(extraction_error,''), raw_metadata
		FROM political_disclosure_reports
		WHERE source=$1 AND external_id=$2`,
		strings.ToLower(strings.TrimSpace(source)), strings.TrimSpace(externalID),
	).Scan(
		&report.Source, &report.ExternalID, &report.Chamber, &report.FilerName,
		&report.Office, &report.ReportType, &report.FilingDate,
		&report.DocumentURL, &report.ContentHash, &report.ExtractionStatus,
		&report.ExtractionError, &rawMetadata,
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

func PoliticalDisclosureReportCompleted(
	ctx context.Context, source, externalID string,
) (bool, error) {
	var completed bool
	err := GetDB().QueryRow(ctx, `
		SELECT extraction_status IN ('completed', 'no_text')
		FROM political_disclosure_reports
		WHERE source=$1 AND external_id=$2`, source, externalID,
	).Scan(&completed)
	if IsNoRows(err) {
		return false, nil
	}
	return completed, err
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

	var reportID string
	err = tx.QueryRow(ctx, `
		INSERT INTO political_disclosure_reports (
			source, external_id, chamber, filer_name, office, report_type,
			filing_date, document_url, content_hash, extraction_status,
			extraction_error, raw_metadata
		) VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,$8,NULLIF($9,''),$10,NULLIF($11,''),$12)
		ON CONFLICT (source, external_id) DO UPDATE SET
			chamber=EXCLUDED.chamber, filer_name=EXCLUDED.filer_name,
			office=EXCLUDED.office, report_type=EXCLUDED.report_type,
			filing_date=EXCLUDED.filing_date, document_url=EXCLUDED.document_url,
			content_hash=EXCLUDED.content_hash,
			extraction_status=EXCLUDED.extraction_status,
			extraction_error=EXCLUDED.extraction_error,
			raw_metadata=EXCLUDED.raw_metadata, updated_at=NOW()
		RETURNING id`, report.Source, report.ExternalID, report.Chamber,
		report.FilerName, report.Office, report.ReportType, report.FilingDate,
		report.DocumentURL, report.ContentHash, report.ExtractionStatus,
		report.ExtractionError, report.RawMetadata,
	).Scan(&reportID)
	if err != nil {
		return fmt.Errorf("upsert political disclosure report: %w", err)
	}
	if report.ExtractionStatus != "completed" {
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
					ELSE political_disclosure_candidates.promotion_status END`,
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
		SELECT claimed.report_id, claimed.ticker, r.source, r.filer_name,
			r.filing_date, r.document_url, claimed.transaction_count,
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
			&candidate.ReportID, &candidate.Ticker, &candidate.Source,
			&candidate.FilerName, &candidate.FilingDate, &candidate.DocumentURL,
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
