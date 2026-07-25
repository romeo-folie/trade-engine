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

-- Before this release, enabling pullback_strength made the scanner universe
-- exclusive, but watchlist rows did not retain that qualification. Preserve
-- those active in-flight candidates across the additive rollout. The tag is
-- inert when the feature remains disabled, and newly scanned standard
-- candidates after this migration are deliberately left untagged.
UPDATE watchlist
SET strategies = array_append(strategies, 'pullback_strength'),
    updated_at = NOW()
WHERE platform = 'stock'
  AND source = 'scanner'
  AND active = TRUE
  AND NOT ('pullback_strength' = ANY(strategies));
