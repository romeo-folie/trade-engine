-- Quarantine scanner rows created before pullback-universe qualification was
-- persisted. This is deliberately a forward migration separate from 054:
-- review/test environments may already have applied an earlier form of 054.

ALTER TABLE watchlist
    ADD COLUMN IF NOT EXISTS pullback_requalification_required BOOLEAN NOT NULL DEFAULT FALSE;

-- There is no reliable durable evidence that a legacy row was created while
-- the old exclusive pullback universe was enabled. Require every active legacy
-- scanner row to complete a definitive point-in-time universe check before it
-- can be reactivated as either pullback-qualified or standard. Provider/history
-- failures leave the marker set and the row inactive.
UPDATE watchlist
SET active = FALSE,
    pullback_requalification_required = TRUE,
    updated_at = NOW()
WHERE platform = 'stock'
  AND source = 'scanner'
  AND active = TRUE;
