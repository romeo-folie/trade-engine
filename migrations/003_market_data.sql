-- 003_market_data.sql
-- Market price data, prediction market snapshots, and news items.

CREATE TABLE candles (
    id          UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    ticker      TEXT        NOT NULL,
    timeframe   timeframe   NOT NULL,
    ts          TIMESTAMPTZ NOT NULL,
    open        NUMERIC(20,8),
    high        NUMERIC(20,8),
    low         NUMERIC(20,8),
    close       NUMERIC(20,8),
    volume      BIGINT,
    vwap        NUMERIC(20,8),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT  candles_unique UNIQUE (ticker, timeframe, ts)
);

CREATE INDEX idx_candles_ticker_tf_ts ON candles (ticker, timeframe, ts DESC);

CREATE TABLE prediction_snapshots (
    id                  UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    platform            TEXT        NOT NULL,
    contract_id         TEXT        NOT NULL,
    contract_title      TEXT        NOT NULL,
    price               NUMERIC(10,6) NOT NULL,
    yes_price           NUMERIC(10,6),
    no_price            NUMERIC(10,6),
    volume_24h          NUMERIC(20,2),
    open_interest       NUMERIC(20,2),
    bid                 NUMERIC(10,6),
    ask                 NUMERIC(10,6),
    resolution_deadline TIMESTAMPTZ,
    resolved            BOOLEAN     NOT NULL DEFAULT FALSE,
    resolution_value    BOOLEAN,
    categories          TEXT[],
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT prediction_snapshots_unique UNIQUE (platform, contract_id, created_at)
);

CREATE INDEX idx_prediction_snapshots_platform_contract ON prediction_snapshots (platform, contract_id, created_at DESC);

CREATE TABLE news_items (
    id              UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    external_id     TEXT        UNIQUE,
    headline        TEXT        NOT NULL,
    summary         TEXT,
    source          TEXT,
    url             TEXT        UNIQUE,
    published_at    TIMESTAMPTZ NOT NULL,
    tickers         TEXT[],
    sentiment_score NUMERIC(4,3),
    embedding       vector(1536),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_news_items_published_at ON news_items (published_at DESC);
CREATE INDEX idx_news_items_tickers ON news_items USING GIN (tickers);
