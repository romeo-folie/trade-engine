-- ORB session and trade tracking tables for Phase 15

CREATE TABLE IF NOT EXISTS orb_sessions (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  date        DATE NOT NULL UNIQUE,
  status      TEXT NOT NULL DEFAULT 'pending'
              CHECK (status IN ('pending','scanning','candidate_ready','range_watch','executing','done','skipped')),
  candidates  JSONB,
  selected    TEXT[],
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS orb_trades (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  session_id  UUID NOT NULL REFERENCES orb_sessions(id) ON DELETE CASCADE,
  ticker      TEXT NOT NULL,
  side        TEXT NOT NULL CHECK (side IN ('buy','sell')),
  entry_stop  NUMERIC,
  stop_loss   NUMERIC,
  take_profit NUMERIC,
  qty         INT,
  order_id    TEXT,
  status      TEXT NOT NULL DEFAULT 'pending'
              CHECK (status IN ('pending','filled','cancelled','expired')),
  fill_price  NUMERIC,
  pnl         NUMERIC,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  closed_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_orb_sessions_date ON orb_sessions(date);
CREATE INDEX IF NOT EXISTS idx_orb_trades_session ON orb_trades(session_id);
