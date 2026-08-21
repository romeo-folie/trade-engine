ALTER TABLE political_disclosure_reports DROP CONSTRAINT IF EXISTS political_disclosure_reports_source_check;
ALTER TABLE political_disclosure_reports ADD CONSTRAINT political_disclosure_reports_source_check CHECK (source IN ('house', 'senate', 'oge'));
ALTER TABLE political_disclosure_reports DROP CONSTRAINT IF EXISTS political_disclosure_reports_chamber_check;
ALTER TABLE political_disclosure_reports ADD CONSTRAINT political_disclosure_reports_chamber_check CHECK (chamber IN ('house', 'senate', 'executive'));
