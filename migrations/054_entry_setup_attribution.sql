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
