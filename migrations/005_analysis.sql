-- 005_analysis.sql
-- LLM analysis records: one row per model call, per ticker, per tick.

CREATE TABLE analyses (
    id                    UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    ticker                TEXT         NOT NULL,
    strategy              TEXT         NOT NULL,
    action                signal_action NOT NULL,
    confidence            NUMERIC(4,3) NOT NULL,
    size_pct              NUMERIC(5,2),
    stop_loss             NUMERIC(20,8),
    take_profit           NUMERIC(20,8),
    reasoning             TEXT,
    raw_response          TEXT,
    raw_prompt            TEXT,
    prompt_hash           TEXT         NOT NULL,
    model_provider        TEXT         NOT NULL,
    model_name            TEXT         NOT NULL,
    token_count_input     INTEGER,
    token_count_output    INTEGER,
    latency_ms            INTEGER,
    indicators            JSONB,
    -- Prediction market specific
    estimated_probability NUMERIC(4,3),
    market_probability    NUMERIC(4,3),
    edge                  NUMERIC(4,3),
    -- Embedding for semantic similarity search
    embedding             vector(1536),
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_analyses_ticker_created    ON analyses (ticker, created_at DESC);
CREATE INDEX idx_analyses_prompt_hash       ON analyses (prompt_hash);
CREATE INDEX idx_analyses_model             ON analyses (model_provider, model_name);
