# PRD: Trade Engine Web Dashboard Redesign

**Status:** Draft  
**Date:** 2026-03-16  
**Author:** Devin  
**Repo:** romeo-folie/trade-engine  
**Existing frontend:** `web/frontend/`

---

## 1. Background

The Phase 14 web dashboard (PR #191, PR #197) shipped a functional pipeline visualisation using React Flow with a dark theme and animated edges. This redesign replaces the visual layer wholesale while preserving all infrastructure: WebSocket hook (`useEngineSocket`), protocol types, the Go web server, and React/Vite as the build tool.

The goal is a terminal-aesthetic, production-grade monitoring interface that matches the character of the system it visualises: an LLM-powered algorithmic trading engine running real money.

---

## 2. Design Language

### Core aesthetic
- **Background:** near-black (`#07060f` or equivalent)
- **Primary accent:** acid green (`#00ff41` / `#39ff14`) — monochrome terminal green
- **Typography:** monospace throughout (JetBrains Mono, self-hosted via `@font-face` in `index.html`)
- **Visual motif:** chamfered corners on all cards/panels (CSS `clip-path: polygon()`)
- **Grid:** scanline overlay at low opacity (CSS repeating-linear-gradient)
- **No rounded corners.** No drop shadows. Hard edges only.

### Colour palette (full)
| Token | Value | Usage |
|---|---|---|
| `--bg` | `#07060f` | Canvas background |
| `--surface` | `#0d0c17` | Panel/card fill |
| `--border` | `#1a1830` | Panel borders |
| `--accent` | `#00ff41` | Active state, pulse rings, highlights |
| `--accent-dim` | `#00c030` | Secondary accent |
| `--text-primary` | `#e8e8f0` | Primary text |
| `--text-secondary` | `#6b6b8a` | Labels, metadata |
| `--red` | `#ef4444` | Error, closed trades |
| `--amber` | `#f59e0b` | Warnings, queued signals |
| `--blue` | `#00b4ff` | Market data events |
| `--purple` | `#a855f7` | Analysis/LLM events |
| `--green` | `#22c55e` | Executed trades |

### Chamfer mixin (reference implementation)
```css
/* 8px chamfer — used on all cards */
clip-path: polygon(
  8px 0%, calc(100% - 8px) 0%,
  100% 8px, 100% calc(100% - 8px),
  calc(100% - 8px) 100%, 8px 100%,
  0% calc(100% - 8px), 0% 8px
);
```

---

## 3. Layout (8 screens / panels)

The dashboard is a single-page application with a persistent left sidebar for navigation and a main content area that swaps between screens.

### 3.1 Global chrome
- **Top bar:** `[TRADE ENGINE]` wordmark (monospace, acid green) + connection status badge + current time (user timezone, live) + event/s rate
- **Left sidebar (64px wide, collapsed):** icon-only navigation. Icons: Pipeline, Portfolio, Signals, Trades, Analysis, Backtests, Config, System
- **Sidebar expands to 200px on hover**, revealing text labels
- Active nav item: accent-coloured left border + dimmed background

### 3.2 Screen 1 — Pipeline (default)
The hub-and-spoke graph. Replaces the existing React Flow three-column layout.

**Graph layout:**
- Hub: single central node `ENGINE` in the middle of the canvas
- Spokes: all pipeline nodes arranged radially around the hub
- Node kinds map to radial distance: `source` nodes outermost, `processing` mid-ring, `output` closest to hub
- Edges: straight lines from hub to each node, with animated particles travelling outward (source→hub) and inward (hub→output)

**Node card design:**
- Chamfered rectangle
- Ticker or node label in monospace caps
- Status indicator: pulsing dot (green = active <5s, amber = idle <30s, grey = stale)
- Event count + last event timestamp
- Click → opens drill-down panel (right side overlay, same as current)

**Event feed:**
- Vertical ticker tape on the right edge of the canvas (not a separate panel)
- Each row: `[timestamp] [event_type] [source→target]` in monospace, colour-coded by event type
- Max 50 rows, newest at top, fades out at bottom

### 3.3 Screen 2 — Portfolio
- Equity curve: full-width SVG line chart, accent green line, glow effect, x-axis = time, y-axis = USD value
- Three KPI cards below chart: Total P&L, Open Positions, Win Rate
- Positions table: Ticker | Entry Price | Current Price | P&L | Duration — sortable columns
- All numbers monospace, colour-coded (green positive, red negative)

### 3.4 Screen 3 — Signals
- Table: Signal ID | Ticker | Action (BUY/SELL) | Confidence | Status | Timestamp
- Status badges: QUEUED (amber), EXECUTED (green), BLOCKED (grey), EXPIRED (grey)
- Filter bar: ticker search + status filter dropdown
- Row click → signal detail side panel

### 3.5 Screen 4 — Trades
- Table: Trade ID | Ticker | Side | Entry | Exit | P&L | Duration | Status
- Summary row at top: total trades, win%, avg P&L
- Filter: date range picker (simple, text-based: `from: 2026-03-01 to: 2026-03-16`)
- Export button: copies CSV to clipboard

### 3.6 Screen 5 — Analysis
- Card grid (2 columns): one card per recent analysis
- Each card: Ticker | Timestamp | Model used | Recommendation | Confidence score | Summary excerpt
- Expand card → full analysis text in a modal/drawer
- Colour-coded recommendation chip: BUY (green), SELL (red), HOLD (amber)

### 3.7 Screen 6 — Backtests
- Table of past backtest runs: ID | Ticker | Period | Total Return | Sharpe | Max Drawdown | Status
- Run detail panel: equity curve + trade list for selected run

### 3.8 Screen 7 — Config (read-only)
- Displays current engine configuration as formatted YAML (syntax-highlighted, monospace)
- Section headers for each config block
- Read-only — no editing in v1

### 3.9 Screen 8 — System
- Node status grid: one card per pipeline node, showing health metrics
- Log tail: last 100 log lines from engine (if server exposes endpoint)
- Uptime, memory, goroutine count (if available from `/api/status`)

---

## 4. Data Sources (existing, unchanged)

| Data | Source |
|---|---|
| Pipeline nodes + events | WebSocket `/ws` → `useEngineSocket` |
| Snapshot | WebSocket `snapshot` message |
| Portfolio / trades / signals | REST API (existing `/api/*` routes) |
| Analysis | REST API |
| Backtests | REST API |
| Config | REST API `/api/config` (read-only) |
| System status | REST API `/api/status` |

The redesign does not add new backend endpoints unless a screen requires data that has no existing route. Any new endpoint requirements are flagged per task.

---

## 5. Technology

| Concern | Choice |
|---|---|
| Build tool | Vite (existing) |
| Framework | React 18 (existing) |
| Router | React Router v6 |
| Graph | `@xyflow/react` (existing) — layout changes only, not replaced |
| Animation | `framer-motion` (existing) + CSS keyframes |
| Charts | Recharts (new dep) — lightweight, composable |
| Styling | CSS Modules + CSS custom properties (design tokens) — no Tailwind, no CSS-in-JS |
| Icons | Lucide React (new dep) — consistent, tree-shakeable |
| Types | TypeScript strict (existing) |

No new backend dependencies.

---

## 6. Non-Goals (v1)

- Real-time portfolio P&L streaming (polling via REST is fine)
- Dark/light mode toggle
- Mobile responsive layout
- Config editing
- User auth / multi-user

---

## 7. Success Criteria

- All 8 screens render without errors
- Pipeline graph matches hub-and-spoke layout spec
- Design tokens applied consistently across all components (no hardcoded colours)
- WebSocket connection + particle animations preserved from Phase 14
- `tsc --noEmit` passes clean
- `vite build` produces a clean bundle (no warnings about circular deps or missing modules)
- Router v6 navigation between all 8 screens works

---

## 8. Out of Scope Changes

The following existing files are **infrastructure, not UI** and must not be modified unless a task explicitly requires it:
- `web/frontend/src/hooks/useEngineSocket.ts`
- `web/frontend/src/types/protocol.ts`
- `vite.config.ts`
- `tsconfig.json`
- Go web server code (`internal/web/`) — **exception:** `internal/web/server.go` was modified in Task 1 to add SPA history-mode fallback routing, which is required for the React Router v6 scaffold
