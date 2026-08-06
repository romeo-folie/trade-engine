-- Repair trade stops truncated by the trailing-stop manager.
--
-- UpdateTradeTrailing wrote its float64 parameters through a bare
-- `NULLIF($1, 0)`, which made Postgres infer the placeholders as `integer`
-- from the literal 0. pgx then encoded the stop into an int4 and the cents
-- were dropped, so every trailing write landed as a whole dollar:
-- trades.stop_loss 8.50 -> 8, high_water_mark 9.13 -> 9.
--
-- Sub-dollar stops failed a second way: 0.50 encoded to integer 0, and the
-- outer `NULLIF(..., 0)` reads 0 as the "leave this column alone" sentinel, so
-- the write was dropped entirely and the row kept whatever it held before. Such
-- a row is NOT the truncation of its leg, so it needs its own predicate — but
-- under $1 no trailing write could ever have landed, which makes the broker leg
-- unambiguously authoritative there.
--
-- The broker's resting stop leg kept the true price, so orders.stop_price is
-- the recoverable source of truth for stop_loss. Restore it only under those
-- two signatures — both specific to this bug — so a stop deliberately set to a
-- round number, or one already corrected, is left alone.
--
-- high_water_mark is deliberately NOT repaired: nothing external records it,
-- and a truncated mark is a lower bound that ratchets back up on the next new
-- high. It self-heals; a fabricated value would not.
UPDATE trades t
SET stop_loss = leg.stop_price
FROM (
    SELECT DISTINCT ON (o.trade_id) o.trade_id, o.stop_price
    FROM orders o
    WHERE o.is_stop_loss = TRUE
      AND o.order_type IN ('stop', 'stop_limit')
      AND o.status IN ('pending', 'partial')
      AND o.stop_price IS NOT NULL
    ORDER BY o.trade_id, o.created_at DESC
) AS leg
WHERE t.id = leg.trade_id
  AND t.status = 'open'
  AND t.stop_loss IS NOT NULL
  AND leg.stop_price > 0
  -- A repair may only RAISE a stop. Both corruption modes lose value on the
  -- way down (truncation floors it; a dropped sub-$1 write leaves the older,
  -- lower ratchet behind), so the leg is always at or above the stored value
  -- when this bug is the cause. A leg BELOW the stored stop is some other
  -- divergence — a stale leg awaiting replacement, say — and rewriting it
  -- would loosen live protection, which a trailing stop must never do.
  AND leg.stop_price > t.stop_loss
  AND (
        -- Cents dropped by the int4 encoding.
        t.stop_loss = TRUNC(leg.stop_price)
        -- Sub-$1 leg: every trailing write for it collided with the 0
        -- sentinel and was discarded, so the stored value is stale by
        -- construction and the leg is the only surviving truth.
     OR TRUNC(leg.stop_price) = 0
  );
