-- 043: first-tick deferral marker (entry-quality gates).
-- Distinguishes signals the engine itself deferred for first-tick
-- confirmation from signals awaiting manual approval, so the deferred
-- execution sweep can never auto-execute a manual-mode pending signal
-- after a mode switch or restart.
ALTER TABLE signals ADD COLUMN IF NOT EXISTS deferred BOOLEAN NOT NULL DEFAULT FALSE;
