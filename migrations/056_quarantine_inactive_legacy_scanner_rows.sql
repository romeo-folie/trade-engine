-- Forward remediation for environments that applied the reviewed first form of
-- migration 055, which quarantined only rows active at that instant. Use 055's
-- durable application timestamp as the legacy boundary: current active state is
-- insufficient because an old row may since have been reactivated and a new row
-- may since have expired.

UPDATE watchlist
SET active = FALSE,
    pullback_requalification_required = TRUE,
    updated_at = NOW()
WHERE platform = 'stock'
  AND source = 'scanner'
  AND pullback_requalification_required = FALSE
  AND created_at <= (
      SELECT applied_at
      FROM schema_migrations
      WHERE filename = '055_scanner_pullback_requalification.sql'
  );
