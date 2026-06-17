-- 033_engine_state.up.sql
-- Small key/value table for engine runtime state that must be readable across
-- process boundaries.
--
-- The engine loop (cmd/te run) and the TUI dashboard (te dashboard) are separate
-- processes. The engine computes its next scheduled run cycle in-memory, but the
-- in-process web server and the out-of-process TUI both need to surface "when does
-- the next cycle begin?". A tiny DB-backed KV table is the single source of truth
-- both readers share. This is deliberately NOT config_overrides, which carries
-- user-facing config semantics; runtime state does not belong there.

CREATE TABLE IF NOT EXISTS engine_state (
    key        TEXT        PRIMARY KEY,
    value      TEXT        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
