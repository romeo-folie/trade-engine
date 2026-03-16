-- no-transaction
-- 015_relationship_types.sql
ALTER TYPE relationship_type ADD VALUE IF NOT EXISTS 'invests_in';
ALTER TYPE relationship_type ADD VALUE IF NOT EXISTS 'invested_in_by';
