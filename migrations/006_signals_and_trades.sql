-- 006_signals_and_trades.sql
-- Signals (intent to trade), trades (executed positions), and orders (broker records).

CREATE TABLE signals (
    id                   UUID          PRIMARY KEY DEFAULT uuid_generate_v4(),
    ticker               TEXT          NOT NULL,
    action               signal_action NOT NULL,
    strategy             TEXT          NOT NULL,
    signal_price         NUMERIC(20,8) NOT NULL,
    stop_loss            NUMERIC(20,8),
    take_profit          NUMERIC(20,8),
    confidence           NUMERIC(4,3)  NOT NULL,
    size_pct             NUMERIC(5,2),
    status               signal_status NOT NULL DEFAULT 'pending',
    block_reason         TEXT,
    analysis_ids         UUID[],
    expires_at           TIMESTAMPTZ,
    stale_threshold_pct  NUMERIC(5,2)  NOT NULL DEFAULT 2.0,
    -- Prediction market fields
    platform             TEXT,
    contract_id          TEXT,
    created_at           TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ   NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_signals_ticker_status  ON signals (ticker, status);
CREATE INDEX idx_signals_created        ON signals (created_at DESC);
CREATE INDEX idx_signals_status         ON signals (status) WHERE status IN ('pending', 'approved');

CREATE TABLE trades (
    id             UUID            PRIMARY KEY DEFAULT uuid_generate_v4(),
    signal_id      UUID            REFERENCES signals(id),
    ticker         TEXT            NOT NULL,
    direction      trade_direction NOT NULL,
    strategy       TEXT            NOT NULL,
    entry_price    NUMERIC(20,8)   NOT NULL,
    exit_price     NUMERIC(20,8),
    quantity       NUMERIC(20,8)   NOT NULL,
    stop_loss      NUMERIC(20,8),
    take_profit    NUMERIC(20,8),
    pnl            NUMERIC(20,8),
    pnl_pct        NUMERIC(10,4),
    status         trade_status    NOT NULL DEFAULT 'open',
    model_provider TEXT,
    model_name     TEXT,
    -- Prediction market fields
    platform       TEXT,
    contract_id    TEXT,
    opened_at      TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    closed_at      TIMESTAMPTZ,
    created_at     TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_trades_ticker_status ON trades (ticker, status);
CREATE INDEX idx_trades_opened_at     ON trades (opened_at DESC);
CREATE INDEX idx_trades_status        ON trades (status) WHERE status = 'open';

CREATE TABLE orders (
    id               UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    trade_id         UUID         REFERENCES trades(id),
    broker_order_id  TEXT,
    ticker           TEXT         NOT NULL,
    side             order_side   NOT NULL,
    order_type       order_type   NOT NULL,
    quantity         NUMERIC(20,8) NOT NULL,
    limit_price      NUMERIC(20,8),
    stop_price       NUMERIC(20,8),
    filled_price     NUMERIC(20,8),
    filled_qty       NUMERIC(20,8),
    status           order_status NOT NULL DEFAULT 'pending',
    is_stop_loss     BOOLEAN      NOT NULL DEFAULT FALSE,
    is_take_profit   BOOLEAN      NOT NULL DEFAULT FALSE,
    platform         TEXT,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_orders_trade_id ON orders (trade_id);
CREATE INDEX idx_orders_status   ON orders (status) WHERE status IN ('pending', 'partial');
