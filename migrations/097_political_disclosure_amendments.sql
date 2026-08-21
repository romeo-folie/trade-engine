ALTER TABLE political_disclosure_reports
    ADD COLUMN is_amendment BOOLEAN NOT NULL DEFAULT FALSE;
