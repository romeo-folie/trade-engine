-- Retain the broker's authoritative first-fill timestamp across recovery.
ALTER TABLE stock_order_intents
    ADD COLUMN IF NOT EXISTS filled_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS discovery_residency_renewed_at TIMESTAMPTZ;

-- Existing intents predate the durable renewal handshake and must not all be
-- replayed solely to apply this new ancillary behavior.
UPDATE stock_order_intents
SET discovery_residency_renewed_at = NOW()
WHERE discovery_residency_renewed_at IS NULL
  AND status IN ('filled','cancelled','rejected');
