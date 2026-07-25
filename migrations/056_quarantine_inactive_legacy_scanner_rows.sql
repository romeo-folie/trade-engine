-- Forward remediation for environments that applied the reviewed first form of
-- migration 055, which quarantined only active scanner rows. Cover inactive and
-- expired legacy rows before they can be reactivated by a later scan.

UPDATE watchlist
SET active = FALSE,
    pullback_requalification_required = TRUE,
    updated_at = NOW()
WHERE platform = 'stock'
  AND source = 'scanner'
  AND pullback_requalification_required = FALSE;
