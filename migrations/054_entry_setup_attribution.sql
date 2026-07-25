-- Durable entry-path attribution for notifications and setup-level performance.

ALTER TABLE signals
    ADD COLUMN entry_setup TEXT NOT NULL DEFAULT 'standard',
    ADD COLUMN entry_setup_evidence TEXT;

UPDATE signals
SET entry_setup = 'pullback_strength'
WHERE pullback_strength = TRUE;

ALTER TABLE trades
    ADD COLUMN entry_setup TEXT NOT NULL DEFAULT 'standard',
    ADD COLUMN entry_setup_evidence TEXT;

UPDATE trades t
SET entry_setup = s.entry_setup,
    entry_setup_evidence = s.entry_setup_evidence
FROM signals s
WHERE t.signal_id = s.id;

CREATE INDEX idx_trades_entry_setup_opened_at
    ON trades (entry_setup, opened_at DESC);

-- Before this release, watchlist rows did not retain whether they passed the
-- optional pullback universe. There is therefore no reliable way to distinguish
-- a qualified row created while the feature was enabled from a standard row
-- created while it was disabled. Deactivate these unknown in-flight rows so the
-- next scanner cycle re-screens them and reactivates each one with the correct
-- additive-path strategy tags. This also prevents an old qualified row from
-- reaching the standard BUY path during rollout.
UPDATE watchlist
SET active = FALSE,
    updated_at = NOW()
WHERE platform = 'stock'
  AND source = 'scanner'
  AND active = TRUE
  AND NOT ('pullback_strength' = ANY(strategies));
