# Epic: Trade Engine Web Dashboard Redesign

**PRD:** `.claude/prds/web-redesign.md`  
**Branch convention:** `redesign/web-<task-slug>`  
**Base:** always branch from fresh `origin/main`  
**PR rule:** one PR per task; wait for merge before starting next task

---

## Implementation Order

Tasks are sequential. Each task is a sub-issue under this epic.

### Task 1 — Design system foundation
**Scope:** Install new deps, set up CSS custom properties (design tokens), global reset, scanline overlay, chamfer utility, typography (JetBrains Mono), router v6 scaffold (8 route stubs).
**Files touched:** `package.json`, `src/styles/tokens.css`, `src/styles/global.css`, `src/styles/chamfer.css`, `src/router.tsx`, `src/main.tsx`, `src/App.tsx`
**No visual output yet** — sets up the foundation every other task builds on.

### Task 2 — App shell: top bar + sidebar navigation
**Scope:** Persistent chrome. Top bar with wordmark, connection status badge, live clock (user TZ), events/s. Collapsible left sidebar with 8 nav items (icon-only → expand on hover). Active route highlighting.
**Files touched:** `src/components/shell/TopBar.tsx`, `src/components/shell/Sidebar.tsx`, `src/components/shell/Shell.tsx`, CSS modules for each.
**Depends on:** Task 1 (tokens, router)

### Task 3 — Pipeline screen (hub-and-spoke graph)
**Scope:** Replace three-column React Flow layout with radial hub-and-spoke. `ENGINE` hub centre, nodes radially arranged by kind. Animated particle edges. Right-edge event feed ticker. Node drill-down panel (right overlay). Preserve all existing WebSocket data wiring.
**Files touched:** `src/screens/Pipeline.tsx`, `src/components/pipeline/HubGraph.tsx`, `src/components/pipeline/PipelineNodeCard.tsx`, `src/components/pipeline/EventFeed.tsx`, `src/components/pipeline/DrillDownPanel.tsx`
**Replaces:** `PipelineGraph.tsx`, `PipelineNode.tsx`, `AnimatedEdge.tsx`, `DrillDownPanel.tsx`, `StatusBar.tsx`
**Depends on:** Task 2

### Task 4 — Portfolio screen
**Scope:** Equity curve (Recharts LineChart, accent green glow), three KPI cards, positions table (sortable). REST polling from `/api/portfolio` (if available) or mock data with TODO comment if endpoint missing.
**Files touched:** `src/screens/Portfolio.tsx`, `src/components/portfolio/EquityCurve.tsx`, `src/components/portfolio/KpiCard.tsx`, `src/components/portfolio/PositionsTable.tsx`
**Depends on:** Task 2

### Task 5 — Signals + Trades screens
**Scope:** Both table-based screens. Signals: filter bar, status badges, row click → side panel. Trades: summary row, date filter, CSV clipboard export. Share common table primitives.
**Files touched:** `src/screens/Signals.tsx`, `src/screens/Trades.tsx`, `src/components/table/DataTable.tsx`, `src/components/table/FilterBar.tsx`, `src/components/signals/SignalDetailPanel.tsx`
**Depends on:** Task 2

### Task 6 — Analysis + Backtests screens
**Scope:** Analysis card grid (2-col) with expand modal. Backtests table + run detail panel with equity curve. Reuse `DataTable` and `EquityCurve` from prior tasks.
**Files touched:** `src/screens/Analysis.tsx`, `src/screens/Backtests.tsx`, `src/components/analysis/AnalysisCard.tsx`, `src/components/analysis/AnalysisModal.tsx`, `src/components/backtests/BacktestDetail.tsx`
**Depends on:** Tasks 4, 5

### Task 7 — Config + System screens + final polish
**Scope:** Config screen (read-only YAML display, syntax-highlighted). System screen (node health grid, log tail, uptime). Final polish pass: pixel-check chamfer consistency, scanline overlay, token audit (no hardcoded colours), `tsc --noEmit` clean, `vite build` clean.
**Files touched:** `src/screens/Config.tsx`, `src/screens/System.tsx`, cross-cutting CSS fixes.
**Depends on:** Tasks 2–6

---

## Deletion list (after Task 3 lands)

These files are replaced by the redesign and must be deleted:
- `src/components/PipelineGraph.tsx`
- `src/components/PipelineNode.tsx`
- `src/components/AnimatedEdge.tsx`
- `src/components/DrillDownPanel.tsx`
- `src/components/StatusBar.tsx`
- `src/App.tsx` (replaced by Shell + router)

**Do not delete until Task 3 PR is merged.**
