ALTER TABLE political_disclosure_reports
    ADD COLUMN retryable_failure BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN retry_attempt_count INTEGER NOT NULL DEFAULT 0
        CHECK (retry_attempt_count >= 0),
    ADD COLUMN next_retry_at TIMESTAMPTZ;

CREATE INDEX idx_political_disclosure_reports_due_retry
    ON political_disclosure_reports (next_retry_at)
    WHERE extraction_status = 'failed' AND retryable_failure;
