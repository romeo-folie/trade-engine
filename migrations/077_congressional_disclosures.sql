CREATE TABLE political_disclosure_reports (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    source TEXT NOT NULL CHECK (source IN ('house', 'senate')),
    external_id TEXT NOT NULL,
    chamber TEXT NOT NULL CHECK (chamber IN ('house', 'senate')),
    filer_name TEXT NOT NULL,
    office TEXT,
    report_type TEXT NOT NULL DEFAULT 'periodic_transaction',
    filing_date DATE NOT NULL,
    document_url TEXT NOT NULL,
    content_hash TEXT,
    extraction_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (extraction_status IN ('pending', 'completed', 'failed', 'no_text')),
    extraction_error TEXT,
    raw_metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (source, external_id)
);

CREATE INDEX idx_political_disclosure_reports_filing_date
    ON political_disclosure_reports (filing_date DESC, source);

CREATE TABLE political_disclosure_transactions (
    id BIGSERIAL PRIMARY KEY,
    report_id UUID NOT NULL REFERENCES political_disclosure_reports(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    owner TEXT,
    asset_name TEXT NOT NULL,
    ticker TEXT,
    asset_type TEXT,
    transaction_type TEXT NOT NULL,
    transaction_date DATE NOT NULL,
    notification_date DATE,
    amount_min BIGINT,
    amount_max BIGINT,
    amount_text TEXT NOT NULL,
    comment TEXT,
    raw_data JSONB NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE (report_id, ordinal)
);

CREATE INDEX idx_political_disclosure_transactions_ticker_date
    ON political_disclosure_transactions (ticker, transaction_date DESC)
    WHERE ticker IS NOT NULL;

CREATE TABLE political_disclosure_candidates (
    report_id UUID NOT NULL REFERENCES political_disclosure_reports(id) ON DELETE CASCADE,
    ticker TEXT NOT NULL,
    transaction_count INTEGER NOT NULL,
    latest_transaction_date DATE NOT NULL,
    evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    promotion_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (promotion_status IN (
            'pending', 'processing', 'promoted', 'rejected',
            'active_watchlist', 'open_position', 'cap_suppressed', 'failed', 'superseded'
        )),
    promotion_claim_token TEXT,
    promotion_claimed_at TIMESTAMPTZ,
    scanner_run_id UUID REFERENCES scanner_runs(id),
    promoted_at TIMESTAMPTZ,
    promotion_reason TEXT,
    PRIMARY KEY (report_id, ticker)
);

CREATE INDEX idx_political_disclosure_candidates_pending
    ON political_disclosure_candidates (latest_transaction_date DESC, ticker)
    WHERE promotion_status = 'pending';

ALTER TABLE scanner_candidate_snapshots
    ADD COLUMN political_disclosure_evidence TEXT;

COMMENT ON TABLE political_disclosure_reports IS
	'Official House Clerk, Senate eFD, and OGE executive disclosure observations.';
COMMENT ON TABLE political_disclosure_candidates IS
    'Ticker-level disclosure evidence that may enter only the ordinary scanner review funnel.';
