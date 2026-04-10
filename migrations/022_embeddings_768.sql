-- Migration 022: resize embedding columns from vector(1536) to vector(768).
--
-- Needed when switching embedding provider from OpenAI (1536-dim) to
-- ollama/nomic-embed-text (768-dim). Existing embeddings are nullified —
-- the embedding worker will recompute them on the next run.

-- analyses
UPDATE analyses SET embedding = NULL WHERE embedding IS NOT NULL;
ALTER TABLE analyses ALTER COLUMN embedding TYPE vector(768);

-- news_items
UPDATE news_items SET embedding = NULL WHERE embedding IS NOT NULL;
ALTER TABLE news_items ALTER COLUMN embedding TYPE vector(768);

-- entities (knowledge graph)
UPDATE entities SET embedding = NULL WHERE embedding IS NOT NULL;
ALTER TABLE entities ALTER COLUMN embedding TYPE vector(768);
