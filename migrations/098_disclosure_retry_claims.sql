ALTER TABLE political_disclosure_reports
    ADD COLUMN retry_claim_token TEXT,
    ADD COLUMN retry_claimed_at TIMESTAMPTZ,
    ADD COLUMN retry_lease_until TIMESTAMPTZ;

ALTER TABLE political_disclosure_reports
    ADD CONSTRAINT political_disclosure_reports_retry_claim_coherence CHECK (
        (retry_claim_token IS NULL AND retry_claimed_at IS NULL AND retry_lease_until IS NULL) OR
        (retry_claim_token IS NOT NULL AND retry_claimed_at IS NOT NULL AND retry_lease_until IS NOT NULL)
    );

CREATE INDEX idx_political_disclosure_reports_retry_lease
    ON political_disclosure_reports (source, retry_lease_until)
    WHERE retry_claim_token IS NOT NULL;

CREATE INDEX idx_political_disclosure_reports_due_retry_source
    ON political_disclosure_reports (source, next_retry_at)
    WHERE extraction_status = 'failed' AND retryable_failure;
