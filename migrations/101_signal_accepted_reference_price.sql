-- 101: preserve the original accepted BUY price through execution repricing.
ALTER TABLE signals
    ADD COLUMN IF NOT EXISTS accepted_reference_price DOUBLE PRECISION;

UPDATE signals
SET accepted_reference_price = signal_price
WHERE accepted_reference_price IS NULL AND signal_price > 0;

COMMENT ON COLUMN signals.accepted_reference_price IS
    'Immutable positive signal price accepted at generation; execution reprices must not change stale-move baseline.';
