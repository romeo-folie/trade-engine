-- Repair trade-less futures sessions left with the 079 default owner. Before an
-- order exists, the durable root evidence lives in selected/candidates rather
-- than orb_trades. Only assign an owner when every persisted symbol belongs to
-- one known futures family; mixed, unknown, empty, and uniqueness-collision
-- sessions remain unchanged for explicit `te orb repair-trades` repair.

WITH symbol_evidence AS (
    SELECT s.id AS session_id, UPPER(BTRIM(t.ticker)) AS symbol
    FROM orb_sessions s
    JOIN orb_trades t ON t.session_id = s.id
    WHERE s.session_name = 'futures'

    UNION

    SELECT s.id, UPPER(BTRIM(selected_symbol))
    FROM orb_sessions s
    CROSS JOIN LATERAL unnest(COALESCE(s.selected, ARRAY[]::TEXT[])) AS selected_symbol
    WHERE s.session_name = 'futures'

    UNION

    SELECT s.id,
           UPPER(BTRIM(COALESCE(candidate #>> '{gap,Ticker}', candidate #>> '{gap,ticker}', '')))
    FROM orb_sessions s
    CROSS JOIN LATERAL jsonb_array_elements(
        CASE WHEN jsonb_typeof(s.candidates) = 'array' THEN s.candidates ELSE '[]'::JSONB END
    ) AS candidate
    WHERE s.session_name = 'futures'
), inferred AS (
    SELECT session_id,
           CASE
               WHEN BOOL_AND(symbol = ANY (ARRAY['GC','MGC','SI','SIL']))
                   THEN 'futures_metals_orb'
               WHEN BOOL_AND(symbol = ANY (ARRAY['ES','MES','NQ','MNQ','YM','MYM','RTY','M2K']))
                   THEN 'futures_index_orb'
           END AS owner_scope
    FROM symbol_evidence
    GROUP BY session_id
), repairable AS (
    SELECT s.id, i.owner_scope
    FROM orb_sessions s
    JOIN inferred i ON i.session_id = s.id AND i.owner_scope IS NOT NULL
    WHERE s.session_name = 'futures'
      AND s.owner_scope = 'equity_orb'
      AND NOT EXISTS (
          SELECT 1
          FROM orb_sessions sibling
          WHERE sibling.date = s.date
            AND sibling.session_name = s.session_name
            AND sibling.owner_scope = i.owner_scope
            AND sibling.id <> s.id
      )
)
UPDATE orb_sessions s
SET asset_class = 'futures', owner_scope = r.owner_scope
FROM repairable r
WHERE s.id = r.id;
