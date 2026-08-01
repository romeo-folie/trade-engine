-- Preserve the policy intent and broker execution time for live equity exits.
-- Existing rows intentionally remain NULL: inferring historical reasons or fill
-- times from order flags / database update timestamps would manufacture audit
-- data that the broker never supplied.
ALTER TABLE trades
    ADD COLUMN IF NOT EXISTS exit_reason TEXT,
    ADD COLUMN IF NOT EXISTS exit_evidence JSONB,
    ADD COLUMN IF NOT EXISTS exit_filled_at TIMESTAMPTZ;

ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS filled_at TIMESTAMPTZ;
