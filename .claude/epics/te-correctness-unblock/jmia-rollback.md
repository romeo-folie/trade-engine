# JMIA consolidation — rollback values

Captured 2026-05-10 immediately before `--apply`.

## Pre-apply state

- 8 open JMIA rows, summing to 4870.32 shares.
- Broker (Alpaca paper) reported 8194.538963 shares @ avg_entry 6.566272.
- Canonical row chosen: `b47400e5-bc58-4d6e-98a0-64f9f11f81c6` (Mar 31, only row with linked `orders`).
- Pre-collapse canonical values: `quantity=570.25846`, `entry_price=6.61`.

## Rollback SQL (paste into psql if needed)

```sql
-- partial rollback (status + pointer):
UPDATE trades SET status='open', consolidated_into=NULL
WHERE consolidated_into = 'b47400e5-bc58-4d6e-98a0-64f9f11f81c6';

-- restore canonical row's original quantity / entry_price:
UPDATE trades SET quantity=570.25846, entry_price=6.61
WHERE id = 'b47400e5-bc58-4d6e-98a0-64f9f11f81c6';
```

## Post-apply expected

- `SELECT count(*) FROM trades WHERE ticker='JMIA' AND status='open'` → 1
- `SELECT quantity, entry_price FROM trades WHERE id='b47400e5-...'` → 8194.538963 / 6.566272
- 7 rows with `status='consolidated', consolidated_into='b47400e5-...'`.
