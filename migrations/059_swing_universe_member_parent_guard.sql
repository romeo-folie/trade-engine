-- Reapply the member immutability guard for databases that installed migration
-- 058 while this feature branch was under review.
CREATE OR REPLACE FUNCTION guard_swing_universe_member_mutation()
RETURNS TRIGGER AS $$
DECLARE
    parent_status TEXT;
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.universe_version_id IS DISTINCT FROM OLD.universe_version_id THEN
        RAISE EXCEPTION 'swing universe member parent is immutable';
    END IF;

    SELECT status INTO parent_status
    FROM swing_universe_versions
    WHERE id = CASE
        WHEN TG_OP = 'DELETE' THEN OLD.universe_version_id
        ELSE NEW.universe_version_id
    END;
    IF parent_status IS DISTINCT FROM 'building' THEN
        RAISE EXCEPTION 'swing universe version is immutable after building';
    END IF;
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;
