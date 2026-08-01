-- Preserve the policy that owns a broker-resident exit leg. This lets a fill
-- reconciled later retain its original reason instead of inferring from flags.
ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS exit_reason TEXT,
    ADD COLUMN IF NOT EXISTS exit_evidence JSONB;
