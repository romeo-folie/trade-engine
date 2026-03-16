-- 015_relationship_types.sql
-- Extend relationship_type enum with investment relationship types.
-- These are valid semantic relationships the LLM correctly identifies.

ALTER TYPE relationship_type ADD VALUE IF NOT EXISTS 'invests_in';
ALTER TYPE relationship_type ADD VALUE IF NOT EXISTS 'invested_in_by';
