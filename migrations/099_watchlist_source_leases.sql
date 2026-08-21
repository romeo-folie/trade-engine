-- Scanner residency is justified by one or more independently expiring source
-- observations. watchlist.expires_at remains a compatibility projection of
-- the latest lease for existing readers and operational tooling.
CREATE TABLE watchlist_source_leases (
    watchlist_id          UUID        NOT NULL REFERENCES watchlist(id) ON DELETE CASCADE,
    source                TEXT        NOT NULL,
    evidence_fingerprint  TEXT        NOT NULL,
    qualified_at          TIMESTAMPTZ NOT NULL,
    observed_at           TIMESTAMPTZ NOT NULL,
    effective_at          TIMESTAMPTZ NOT NULL,
    expires_at            TIMESTAMPTZ NOT NULL,
    evidence              JSONB       NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (watchlist_id, source),
    CONSTRAINT watchlist_source_leases_source_nonempty CHECK (btrim(source) <> ''),
    CONSTRAINT watchlist_source_leases_evidence_object CHECK (jsonb_typeof(evidence) = 'object')
);

CREATE INDEX idx_watchlist_source_leases_live
    ON watchlist_source_leases (watchlist_id, expires_at);

ALTER TABLE watchlist
    ADD COLUMN IF NOT EXISTS last_rediscovered_at TIMESTAMPTZ;

-- Existing scanner rows predate source leases. Give each attributed source a
-- conservative synthetic lease ending at the row's existing expiry. Rows with
-- no structured attribution retain a single legacy_scanner lease. This keeps
-- migration from extending the life of any legacy candidate.
WITH legacy_sources AS (
    SELECT w.id AS watchlist_id,
           lower(trim(source.value)) AS source,
           COALESCE(w.discovery_context->'evidence_by_source'->lower(trim(source.value)), '{}'::jsonb) AS evidence,
           w.created_at,
           w.updated_at,
           w.expires_at
    FROM watchlist w
    CROSS JOIN LATERAL jsonb_array_elements_text(
        CASE WHEN jsonb_typeof(w.discovery_context->'sources') = 'array'
             THEN w.discovery_context->'sources' ELSE '[]'::jsonb END
    ) AS source(value)
    WHERE w.source = 'scanner' AND w.expires_at IS NOT NULL
), unattributed AS (
    SELECT w.id AS watchlist_id,
           'legacy_scanner'::text AS source,
           '{}'::jsonb AS evidence,
           w.created_at,
           w.updated_at,
           w.expires_at
    FROM watchlist w
    WHERE w.source = 'scanner' AND w.expires_at IS NOT NULL
      AND NOT EXISTS (SELECT 1 FROM legacy_sources s WHERE s.watchlist_id = w.id)
)
INSERT INTO watchlist_source_leases (
    watchlist_id, source, evidence_fingerprint, qualified_at, observed_at,
    effective_at, expires_at, evidence
)
SELECT watchlist_id, source,
       'legacy:' || md5(evidence::text || ':' || expires_at::text),
       created_at, updated_at, created_at, expires_at, evidence
FROM (
    SELECT * FROM legacy_sources
    UNION ALL
    SELECT * FROM unattributed
) legacy
ON CONFLICT (watchlist_id, source) DO NOTHING;

COMMENT ON TABLE watchlist_source_leases IS
    'Current qualifying evidence and independent residency lease for each scanner discovery source.';
