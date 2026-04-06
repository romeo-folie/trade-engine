# ORB Dashboard — Web UI Specification

## Overview

A dedicated ORB screen in the web frontend that provides real-time visibility into Opening Range Breakout operations across all modes (equities, forex, CFD). The screen replaces the need for TUI or terminal access during unattended cron runs.

---

## Backend: New REST Endpoints

All endpoints follow existing patterns in `internal/web/handlers.go`. Responses use `json.NewEncoder(w).Encode()` with `Content-Type: application/json`.

### `GET /api/orb/sessions?limit=10`

Returns recent ORB sessions, newest first.

```json
{
  "sessions": [
    {
      "id": "uuid",
      "date": "2026-04-05",
      "status": "done",
      "asset_class": "forex",
      "candidates": [
        {
          "gap": {
            "ticker": "EUR/USD",
            "gap_pct": 0,
            "pre_market_vol": 0,
            "pre_market_last": 1.0923,
            "prior_close": 1.0910
          },
          "score": {
            "action": "trade",
            "conviction": 1.0,
            "catalyst_type": "",
            "catalyst_quality": "",
            "reasoning": "",
            "risks": []
          }
        }
      ],
      "selected": ["EUR/USD", "GBP/USD"],
      "created_at": "2026-04-05T07:30:00Z",
      "updated_at": "2026-04-05T10:30:00Z"
    }
  ]
}
```

**Data source:** `store.GetORBSessionByDate()` or a new `GetRecentORBSessions(ctx, limit)` query on `orb_sessions` table ordered by `date DESC`.

### `GET /api/orb/session/today`

Shortcut for the current day's session. Returns the same shape as a single element from `/api/orb/sessions`. Returns `404` if no session exists for today.

**Data source:** `store.GetORBSessionByDate(ctx, db, today)`.

### `GET /api/orb/trades?session_id=<uuid>`

Returns all trades for a session.

```json
{
  "trades": [
    {
      "id": "uuid",
      "session_id": "uuid",
      "ticker": "EUR/USD",
      "side": "buy",
      "entry_stop": 1.0935,
      "stop_loss": 1.0910,
      "take_profit": 1.0985,
      "qty": 20000,
      "order_id": "IBKR-12345",
      "status": "filled",
      "fill_price": 1.0936,
      "pnl": 42.00,
      "asset_class": "forex",
      "conid": "12087792",
      "created_at": "2026-04-05T08:16:00Z",
      "closed_at": "2026-04-05T09:45:00Z"
    }
  ]
}
```

**Data source:** `store.GetORBTrades(ctx, db, sessionID)`.

### `GET /api/orb/trades/recent?limit=20`

Returns recent ORB trades across all sessions. Useful for the trades table without knowing session IDs.

**Data source:** New query on `orb_trades` ordered by `created_at DESC LIMIT $1`.

### `GET /api/orb/performance?window=30d`

Returns aggregated ORB performance metrics from the feedback pipeline.

```json
{
  "instruments": [
    {
      "ticker": "EUR/USD",
      "session": "london",
      "window": "30d",
      "trade_count": 18,
      "win_rate": 0.61,
      "avg_pnl_pct": 0.32,
      "wins": 11,
      "losses": 7
    },
    {
      "ticker": "XAU/USD",
      "session": "london",
      "window": "30d",
      "trade_count": 12,
      "win_rate": 0.58,
      "avg_pnl_pct": 0.45,
      "wins": 7,
      "losses": 5
    }
  ],
  "overall": {
    "trade_count": 30,
    "win_rate": 0.60,
    "avg_pnl_pct": 0.37
  }
}
```

**Data source:** `store.GetLatestFeedbackInsights(ctx, "orb_session", window, date)`. Win/loss counts derived from `trade_count * win_rate`.

### `GET /api/orb/flags?resolved=false&limit=20`

Returns active feedback flags relevant to ORB.

```json
{
  "flags": [
    {
      "id": "uuid",
      "date": "2026-04-05",
      "scope": "orb_session",
      "scope_key": "EUR/USD:london",
      "severity": "warning",
      "message": "EUR/USD London ORB: win rate dropped to 30% over last 12 trades (threshold: 35%)",
      "metric": "win_rate",
      "metric_value": 0.30,
      "threshold": 0.35,
      "resolved": false,
      "created_at": "2026-04-05T18:00:00Z"
    }
  ]
}
```

**Data source:** `store.GetActiveFlags(ctx, limit)` filtered to `scope = "orb_session"` or `scope = "instrument"`.

### `POST /api/orb/flags/:id/resolve`

Marks a flag as resolved.

```json
{"status": "ok"}
```

**Data source:** `store.ResolveFlag(ctx, id)`.

---

## Backend: WebSocket Events

Add ORB-specific events to the WebSocket protocol so the frontend receives live state updates without polling.

### New Event Types

Add to `internal/events/types.go`:

```go
ORBStateChanged    Type = "orb.state_changed"
ORBCandidatesReady Type = "orb.candidates_ready"
ORBRangeLocked     Type = "orb.range_locked"
ORBTradeUpdate     Type = "orb.trade_update"
```

### Event Payloads

**ORBStateChanged:**
```json
{
  "type": "orb.state_changed",
  "payload": {
    "state": "range_watch",
    "session_id": "uuid",
    "date": "2026-04-05",
    "asset_class": "forex"
  }
}
```

**ORBCandidatesReady:**
```json
{
  "type": "orb.candidates_ready",
  "payload": {
    "session_id": "uuid",
    "candidates": [...],
    "selected": ["EUR/USD", "GBP/USD"]
  }
}
```

**ORBRangeLocked:**
```json
{
  "type": "orb.range_locked",
  "payload": {
    "ticker": "EUR/USD",
    "range_high": 1.0935,
    "range_low": 1.0910,
    "range_minutes": 15
  }
}
```

**ORBTradeUpdate:**
```json
{
  "type": "orb.trade_update",
  "payload": {
    "trade_id": "uuid",
    "ticker": "EUR/USD",
    "status": "filled",
    "fill_price": 1.0936,
    "pnl": 42.00
  }
}
```

### Publishing Points

In `internal/orb/scheduler.go`, publish these events at state transitions:
- `tickIdle → tickScanning`: publish `ORBStateChanged`
- After `ScanCandidates` completes: publish `ORBCandidatesReady`
- When `RangeBuffer.locked` becomes true: publish `ORBRangeLocked`
- After `PlaceBracket`: publish `ORBTradeUpdate` (status=pending)
- After order fill/cancel: publish `ORBTradeUpdate` (status=filled/cancelled)

### WebSocket Protocol Mapping

In `internal/web/protocol.go`, add ORB events to `mapEventEdge()`:
```go
case "orb.state_changed":     return "orb_scheduler", "orb_session"
case "orb.candidates_ready":  return "orb_scanner", "orb_scheduler"
case "orb.range_locked":      return "market_data", "orb_scheduler"
case "orb.trade_update":      return "orb_scheduler", "executor"
```

---

## Frontend: ORB Screen

### Route

Add to `web/frontend/src/router.tsx`:
```
/orb → ORBScreen
```

Add "ORB" entry to the sidebar navigation in Shell component.

### Screen Layout

```
┌─────────────────────────────────────────────────────────────┐
│  ORB Dashboard                              [Session: Today]│
├──────────┬──────────┬──────────┬──────────┬─────────────────┤
│  STATUS  │  TRADES  │ WIN RATE │  AVG PNL │   SESSION TIME  │
│  ● Done  │    4     │   62%    │  +0.32%  │  London 08:00   │
├──────────┴──────────┴──────────┴──────────┴─────────────────┤
│                                                             │
│  SESSION TIMELINE                                           │
│  ○ Idle → ● Scanning → ● CandidateReady → ● RangeWatch    │
│  → ● Executing → ● Done                                    │
│                                                             │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  CANDIDATES                                                 │
│  ┌─────────┬────────┬────────────┬──────────┬─────────────┐ │
│  │ Ticker  │ Gap %  │ Conviction │ Catalyst │ Reasoning   │ │
│  ├─────────┼────────┼────────────┼──────────┼─────────────┤ │
│  │ EUR/USD │  —     │ 100%       │ session  │ Pre-config  │ │
│  │ GBP/USD │  —     │ 100%       │ session  │ Pre-config  │ │
│  │ XAU/USD │  —     │ 100%       │ session  │ Pre-config  │ │
│  └─────────┴────────┴────────────┴──────────┴─────────────┘ │
│                                                             │
│  For equities ORB, gap %, conviction, and catalyst come     │
│  from the LLM scan. For forex/CFD, pairs are pre-configured │
│  so conviction is always 100%.                              │
│                                                             │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  OPENING RANGES                                             │
│  ┌─────────┬──────────┬──────────┬───────┬────────────────┐ │
│  │ Ticker  │ High     │ Low      │ Width │ Locked At      │ │
│  ├─────────┼──────────┼──────────┼───────┼────────────────┤ │
│  │ EUR/USD │ 1.09350  │ 1.09100  │ 25pip │ 08:15 UTC      │ │
│  │ GBP/USD │ 1.27200  │ 1.26950  │ 25pip │ 08:15 UTC      │ │
│  │ XAU/USD │ 2342.50  │ 2338.00  │ $4.50 │ 08:15 UTC      │ │
│  └─────────┴──────────┴──────────┴───────┴────────────────┘ │
│                                                             │
│  Appears during StateRangeWatch and after. Shows range      │
│  accumulation progress (e.g. "12/15 bars") while building.  │
│                                                             │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  TRADES                                                     │
│  ┌─────────┬──────┬─────────┬─────────┬─────────┬────┬────┐│
│  │ Ticker  │ Side │ Entry   │ Stop    │ Target  │Qty │ PnL││
│  ├─────────┼──────┼─────────┼─────────┼─────────┼────┼────┤│
│  │ EUR/USD │ BUY  │ 1.09370 │ 1.09080 │ 1.09950 │20k │+$42││
│  │ GBP/USD │ BUY  │ 1.27220 │ 1.26930 │ 1.27800 │20k │ -  ││
│  │ XAU/USD │ BUY  │ 2343.00 │ 2337.50 │ 2354.00 │ 1  │ -  ││
│  └─────────┴──────┴─────────┴─────────┴─────────┴────┴────┘│
│                                                             │
│  Status badges: pending (amber), filled (green),            │
│  cancelled (gray), expired (gray)                           │
│  PnL: green for profit, red for loss, "—" for open/pending  │
│                                                             │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  PERFORMANCE (30d rolling)                                  │
│  ┌─────────────────┬────────┬──────────┬────┬──────────────┐│
│  │ Instrument      │ W / L  │ Win Rate │Avg │ Streak       ││
│  ├─────────────────┼────────┼──────────┼────┼──────────────┤│
│  │ EUR/USD London  │ 11 / 7 │ 61%      │+.3%│ W W L W W    ││
│  │ GBP/USD London  │  5 / 3 │ 63%      │+.4%│ W L W W L    ││
│  │ XAU/USD London  │  7 / 5 │ 58%      │+.5%│ L L W W W    ││
│  │ EUR/USD NY      │  8 / 4 │ 67%      │+.2%│ W W W L W    ││
│  └─────────────────┴────────┴──────────┴────┴──────────────┘│
│                                                             │
│  Data from feedback_insights table (scope=orb_session).     │
│  Window toggle: 7d | 30d                                    │
│                                                             │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  ALERTS                                                     │
│  ┌──────┬──────────────────────────────────────┬───────────┐│
│  │ [!]  │ EUR/USD London: win rate 30% (< 35%) │ [Resolve] ││
│  │ [i]  │ Arbiter override rate at 55% (> 50%) │ [Resolve] ││
│  └──────┴──────────────────────────────────────┴───────────┘│
│                                                             │
│  Unresolved feedback_flags for ORB scopes. Resolve button   │
│  calls POST /api/orb/flags/:id/resolve.                     │
│                                                             │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  SESSION HISTORY                                            │
│  ┌────────────┬──────────┬───────┬──────┬──────┬──────────┐ │
│  │ Date       │ Session  │ Asset │ W/L  │ PnL  │ Status   │ │
│  ├────────────┼──────────┼───────┼──────┼──────┼──────────┤ │
│  │ 2026-04-05 │ london   │ forex │ 2/1  │ +$62 │ done     │ │
│  │ 2026-04-05 │ newyork  │ forex │ 1/1  │ -$18 │ done     │ │
│  │ 2026-04-04 │ london   │ forex │ 3/0  │+$120 │ done     │ │
│  │ 2026-04-04 │ —        │equity │ 0/0  │  $0  │ skipped  │ │
│  └────────────┴──────────┴───────┴──────┴──────┴──────────┘ │
│                                                             │
│  Paginated list of past ORB sessions from /api/orb/sessions.│
│  Click a row to expand and show that session's trades.      │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

### KPI Cards (Top Row)

| Card | Value Source | Color Logic |
|------|-------------|-------------|
| Status | `session.status` via WS `orb.state_changed` or REST | Scanning=indigo, CandidateReady=amber, RangeWatch=amber, Executing=red, Done=emerald, Skipped=textDim |
| Trades | Count of `trades[]` for today's session | textPrimary |
| Win Rate | `performance.overall.win_rate` (30d) | >= 50% emerald, < 50% red |
| Avg PnL | `performance.overall.avg_pnl_pct` (30d) | > 0 emerald, < 0 red |
| Session Time | Derived from `forex_orb.session` config or session date/time | textMuted |

### Session Timeline

A horizontal stepper showing ORB state progression:

```
○ Idle → ● Scanning → ● CandidateReady → ○ RangeWatch → ○ Executing → ○ Done
```

- Completed states: filled circle, acidGreen
- Current state: filled circle, pulsing amber
- Future states: hollow circle, textDim

Update source: WebSocket `orb.state_changed` events or poll `/api/orb/session/today`.

### Candidates Table

| Column | Source |
|--------|--------|
| Ticker | `candidate.gap.ticker` |
| Gap % | `candidate.gap.gap_pct` — show "—" for forex (always 0) |
| Conviction | `candidate.score.conviction` — percentage format |
| Catalyst | `candidate.score.catalyst_type` — show "session" for forex |
| Reasoning | `candidate.score.reasoning` — truncated, expand on hover |

Selected candidates (in `session.selected`) highlighted with acidGreen left border.

### Opening Ranges Table

This data is not currently persisted — it lives in the scheduler's in-memory `RangeBuffer`. Two options:

**Option A (recommended):** Surface via WebSocket `orb.range_locked` events. The frontend accumulates these in local state. Data is ephemeral (lost on page refresh during active session), but ranges only matter during live sessions.

**Option B:** Persist range data to a new `orb_ranges` table and serve via REST. More reliable but adds DB complexity for transient data.

### Trades Table

Columns map directly to `ORBTrade` fields from `/api/orb/trades?session_id=...`.

| Column | Field | Format |
|--------|-------|--------|
| Ticker | `ticker` | plain |
| Side | `side` | BUY=emerald, SELL=red |
| Entry | `entry_stop` | price format |
| Stop | `stop_loss` | price format |
| Target | `take_profit` | price format |
| Qty | `qty` | number, "20k" for forex |
| Status | `status` | badge: pending=amber, filled=emerald, cancelled=textDim |
| PnL | `pnl` | "$X.XX" green/red, "—" if null |

Live updates via WebSocket `orb.trade_update` events.

### Performance Table

Data from `/api/orb/performance?window=30d`. Window toggle (7d / 30d) re-fetches.

The "Streak" column shows the last 5 trade outcomes as colored dots or letters (W/L). This requires a new query that returns the last N ORB trade outcomes for each instrument, or the performance endpoint includes a `recent_outcomes: ["W","W","L","W","W"]` array.

### Alerts Panel

Data from `/api/orb/flags?resolved=false`. Each flag shows:
- Severity icon: `[!!!]` critical (red), `[!]` warning (amber), `[i]` info (indigo)
- Message text
- Resolve button → `POST /api/orb/flags/:id/resolve` → remove from list

### Session History

Paginated table from `/api/orb/sessions?limit=10`. Click to expand inline, showing that session's trades fetched from `/api/orb/trades?session_id=...`.

---

## Frontend: Component Breakdown

Follow existing patterns from Portfolio.tsx and Trades.tsx.

### New Files

```
web/frontend/src/
  screens/
    ORB.tsx              — Main screen component
    ORB.module.css       — Screen styles
  components/orb/
    SessionTimeline.tsx  — State stepper
    CandidatesTable.tsx  — Candidates grid
    RangesTable.tsx      — Opening ranges display
    ORBTradesTable.tsx   — Trades grid with status badges
    PerformanceTable.tsx — Rolling performance with streak dots
    AlertsPanel.tsx      — Feedback flags with resolve action
    SessionHistory.tsx   — Expandable past sessions list
```

### Data Fetching

```typescript
// ORB.tsx
const [session, setSession] = useState<ORBSession | null>(null);
const [trades, setTrades] = useState<ORBTrade[]>([]);
const [performance, setPerformance] = useState<ORBPerformance | null>(null);
const [flags, setFlags] = useState<FeedbackFlag[]>([]);
const [window, setWindow] = useState<'7d' | '30d'>('30d');

// Initial fetch + 15s poll
useEffect(() => {
  const load = async () => {
    const [sess, perf, fl] = await Promise.all([
      fetch('/api/orb/session/today').then(r => r.ok ? r.json() : null),
      fetch(`/api/orb/performance?window=${window}`).then(r => r.json()),
      fetch('/api/orb/flags?resolved=false').then(r => r.json()),
    ]);
    setSession(sess);
    setPerformance(perf);
    setFlags(fl.flags);
    if (sess?.id) {
      const tr = await fetch(`/api/orb/trades?session_id=${sess.id}`).then(r => r.json());
      setTrades(tr.trades);
    }
  };
  load();
  const interval = setInterval(load, 15_000);
  return () => clearInterval(interval);
}, [window]);
```

### WebSocket Integration

Subscribe to ORB events via the existing `useEngineSocket()` hook. Add ORB event types to `web/frontend/src/types/protocol.ts`:

```typescript
export type ORBEventType =
  | 'orb.state_changed'
  | 'orb.candidates_ready'
  | 'orb.range_locked'
  | 'orb.trade_update';
```

In the socket hook's event handler, dispatch ORB events to update local state without waiting for the next poll cycle.

---

## Frontend: TypeScript Types

Add to `web/frontend/src/types/` or inline in the ORB screen:

```typescript
interface ORBSession {
  id: string;
  date: string;
  status: 'idle' | 'scanning' | 'candidate_ready' | 'range_watch' | 'executing' | 'done' | 'skipped';
  asset_class: string;
  candidates: ScoredCandidate[];
  selected: string[];
  created_at: string;
  updated_at: string;
}

interface ScoredCandidate {
  gap: {
    ticker: string;
    gap_pct: number;
    pre_market_vol: number;
    pre_market_last: number;
    prior_close: number;
  };
  score: {
    action: string;
    conviction: number;
    catalyst_type: string;
    catalyst_quality: string;
    reasoning: string;
    risks: string[];
  };
}

interface ORBTrade {
  id: string;
  session_id: string;
  ticker: string;
  side: 'buy' | 'sell';
  entry_stop: number;
  stop_loss: number;
  take_profit: number;
  qty: number;
  order_id: string;
  status: 'pending' | 'filled' | 'cancelled' | 'expired';
  fill_price: number;
  pnl: number | null;
  asset_class: string;
  conid: string;
  created_at: string;
  closed_at: string | null;
}

interface ORBPerformance {
  instruments: InstrumentPerformance[];
  overall: {
    trade_count: number;
    win_rate: number;
    avg_pnl_pct: number;
  };
}

interface InstrumentPerformance {
  ticker: string;
  session: string;
  window: string;
  trade_count: number;
  win_rate: number;
  avg_pnl_pct: number;
  wins: number;
  losses: number;
  recent_outcomes?: ('W' | 'L')[];
}

interface FeedbackFlag {
  id: string;
  date: string;
  scope: string;
  scope_key: string;
  severity: 'info' | 'warning' | 'critical';
  message: string;
  metric: string;
  metric_value: number;
  threshold: number;
  resolved: boolean;
  created_at: string;
}
```

---

## Styling Guidelines

Follow existing design system from `web/frontend/src/styles/tokens.ts`:

| Element | Token |
|---------|-------|
| Background | `canvasBg` / `panelBg` |
| Cards | `cardBg` with `border` |
| KPI values | `textPrimary`, `JetBrains Mono` |
| Labels | `textMuted`, `Space Grotesk` |
| Status: active | `acidGreen` (#39FF14) |
| Status: positive | `emerald` (#22C55E) |
| Status: negative | `red` |
| Status: warning | `amber` |
| Status: idle | `textDim` |
| Borders | Use `CornerBrackets` or `ChamferedBox` components |

No new dependencies needed. Reuse existing `KpiCard`, `DataTable`, `CornerCard` components.

---

## Implementation Priority

1. **REST endpoints** (backend) — unblocks all frontend work
2. **ORB screen with polling** — functional dashboard with 15s refresh
3. **WebSocket events** — real-time updates without polling lag
4. **Performance + alerts panels** — feedback pipeline visualization
5. **Session history with expand** — historical review

Steps 1-2 are the MVP. Steps 3-5 are polish.
