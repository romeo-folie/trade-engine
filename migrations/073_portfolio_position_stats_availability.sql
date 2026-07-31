-- Distinguish a legitimately flat broker book from a poller snapshot whose
-- best-effort positions request failed. Existing engine/poller rows predate
-- this distinction and retain their prior complete-snapshot semantics.
ALTER TABLE portfolio_snapshots
  ADD COLUMN IF NOT EXISTS position_stats_available BOOLEAN NOT NULL DEFAULT TRUE;

-- Historical backfill rows contain equity only; their position fields have
-- always been placeholders. Old poller/engine rows remain true because their
-- completeness cannot be reconstructed reliably.
UPDATE portfolio_snapshots
SET position_stats_available = FALSE
WHERE source = 'backfill';
