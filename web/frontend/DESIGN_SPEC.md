# TradeEngine GUI — Frontend Implementation Spec
**Author:** Desire (Designer)  
**Date:** 2026-03-16  
**For:** Devin (Developer)  
**Repo:** `romeo-folie/trade-engine`  
**Frontend path:** `~/superteam-ops/projects/trade-engine/web/frontend/`  
**Stack:** React 18 + TypeScript + Vite + `@xyflow/react` + `framer-motion`

---

## Overview

This document is your complete implementation guide for the TradeEngine GUI. Every design decision is documented here. Do not guess — if something isn't covered, check the session log (linked at the bottom) or ask Romeo.

The existing frontend has a partial implementation of the pipeline canvas. Your job is to bring the full UI to spec across all 8 screens, with pixel-accurate implementation of every component.

---

## Design System

### Colour Tokens

```ts
// tokens.ts — use these everywhere, no raw hex values in components

export const colors = {
  // Base
  canvas:       '#0A0A0D',  // main canvas background
  panelBg:      '#0D0D11',  // slide-in panel background
  cardBg:       '#111115',  // card / node background
  cardBgAlt:    '#141418',  // slightly brighter card (Arbiter, Synthesis)
  border:       '#2A2A35',  // default border
  borderMuted:  '#1A1A22',  // nearly invisible border / dot grid

  // Text
  textPrimary:  '#F0F0F0',  // main readable text
  textMuted:    '#9CA3AF',  // secondary labels
  textDim:      '#6B7280',  // section headers, timestamps
  textFaint:    '#4B5563',  // very muted (timestamps in logs)

  // Accents
  acidGreen:    '#39FF14',  // primary active/live accent — THE green
  emerald:      '#22C55E',  // positive P&L, LONG direction, [ OK ] status
  amber:        '#F59E0B',  // STANDBY, WAIT, PAPER mode, warnings
  red:          '#EF4444',  // negative P&L, SHORT direction, blocked, [ ERR ]
  indigo:       '#6366F1',  // Technical analyst
  violet:       '#8B5CF6',  // Sentiment analyst
  slate:        '#4A5568',  // Signal source node

  // Analyst type colours (bracket colour per analyst)
  analystTechnical:  '#6366F1',
  analystMomentum:   '#F59E0B',
  analystSentiment:  '#8B5CF6',
  analystMacro:      '#0EA5E9',
}
```

### Typography

**Two fonts only. No exceptions. No system fonts.**

```css
/* Load via Google Fonts or local install */
font-family: 'JetBrains Mono', monospace;   /* all data, values, labels, code */
font-family: 'Space Grotesk', sans-serif;   /* names, titles, headings */
```

| Use | Font | Size | Weight | Colour |
|---|---|---|---|---|
| Section header label | JetBrains Mono | 12px | 400 | `#6B7280` uppercase |
| Node/component name | Space Grotesk | 13px | 500 | `#F0F0F0` |
| Data value (price, score) | JetBrains Mono | 13px | 400 | `#F0F0F0` |
| Verdict (LONG/SHORT) | JetBrains Mono | 16-18px | 700 | emerald/red/amber |
| Timestamp | JetBrains Mono | 11px | 400 | `#4B5563` |
| Muted label | JetBrains Mono | 10px | 400 | `#6B7280` |
| Top bar system name | JetBrains Mono | 14px | 700 | `#39FF14` |
| Large stat value | Space Grotesk | 28px | 700 | `#F0F0F0` or accent |

### Shape Language

**Chamfer = top-right corner cut at 45°, ~12px. All other corners: 4px radius.**

This is achieved with `clip-path` in CSS:

```css
/* Chamfered card — 12px chamfer top-right */
.chamfered {
  clip-path: polygon(
    0 0,
    calc(100% - 12px) 0,
    100% 12px,
    100% 100%,
    0 100%
  );
  border-radius: 4px 0 4px 4px; /* bottom corners still rounded */
}

/* Chamfered badge (small) — 6px chamfer */
.chamfered-sm {
  clip-path: polygon(
    0 0,
    calc(100% - 6px) 0,
    100% 6px,
    100% 100%,
    0 100%
  );
}
```

**Panels that open from the right:** chamfer on TOP-LEFT only (the exposed edge). The right edge is the screen edge — no chamfer there.

```css
.panel-from-right {
  clip-path: polygon(
    12px 0,
    100% 0,
    100% 100%,
    0 100%,
    0 12px
  );
}
```

### Corner Brackets

The `⌐` (top-left) and `¬` (bottom-right) bracket pair is used to frame card content as a type/direction indicator. **This replaces left color border strips entirely — never use left color strips.**

Rules:
- Brackets hug the content — `¬` closes at the last line of text, not at a fixed box height
- Bracket colour = analyst type colour (see colour tokens above)
- Default opacity: 60%. Active: 100%.
- Size: ~14px, JetBrains Mono or similar monospace

```tsx
// CornerBrackets.tsx
interface Props {
  color: string
  opacity?: number
  children: React.ReactNode
}

export function CornerBrackets({ color, opacity = 0.6, children }: Props) {
  return (
    <div style={{ position: 'relative', display: 'inline-block' }}>
      <span style={{
        position: 'absolute', top: 0, left: 0,
        color, opacity, fontFamily: 'JetBrains Mono', fontSize: 14,
        lineHeight: 1, userSelect: 'none'
      }}>⌐</span>
      <div style={{ padding: '2px 10px' }}>{children}</div>
      <span style={{
        position: 'absolute', bottom: 0, right: 0,
        color, opacity, fontFamily: 'JetBrains Mono', fontSize: 14,
        lineHeight: 1, userSelect: 'none'
      }}>¬</span>
    </div>
  )
}
```

### Active State / Glow

```css
/* Acid green active border with glow */
.active-border {
  border: 1px solid #39FF14;
  box-shadow: 0 0 8px rgba(57, 255, 20, 0.4);
}

/* Phosphor glow on acid green text */
.glow-text {
  color: #39FF14;
  text-shadow: 0 0 6px rgba(57, 255, 20, 0.3);
}

/* Dot grid canvas background */
.canvas-bg {
  background-color: #0A0A0D;
  background-image: radial-gradient(circle, #1A1A22 1px, transparent 1px);
  background-size: 20px 20px;
}
```

### Banned Patterns

These are explicitly forbidden. Do not use them:

- ❌ **Left color border strips** on cards (thick vertical bar flush to left card edge)
- ❌ **Bordered sidebars** (visible border rail running full panel height)
- ❌ **Rounded pill cards** — use chamfered shapes only
- ❌ **Navy blue** anywhere
- ❌ **System fonts** — JetBrains Mono and Space Grotesk only
- ❌ **Gradients on data fields**

---

## Screens

### 1. Boot Screen

**Route/view:** Initial loading state before engine connects.  
**Background:** Pure `#000000` — no canvas dot grid, no textures.  
**Font:** JetBrains Mono exclusively.

Content block: left-aligned, vertically centred, ~600px wide column.

```
Color assignments:
"[ OK ]" tag:    #22C55E emerald
"[ WAIT ]" tag:  #F59E0B amber
"[ ERR ]" tag:   #EF4444 red
Line text:       #39FF14 acid green (with text-shadow glow)
Horizontal rules: #1A1A22
Title line:      #39FF14 slightly brighter
Log timestamps:  #4B5563
[INFO] tag:      #39FF14
[WARN] tag:      #F59E0B
[ERR] tag:       #EF4444
Log message:     #9CA3AF
Numeric values:  #F0F0F0
```

Blinking cursor after last line: `animation: blink 1s step-end infinite`.

Optional: CRT scanline overlay at 3% opacity.

**Visual reference:** See approved Pencil screenshot `Boot Screen — TradeEngine.png`

---

### 2. Canvas — Hub & Spoke Pipeline

**Route/view:** Main view. Already partially implemented in `PipelineGraph.tsx`.  
**Background:** Canvas dot grid (`#0A0A0D` + `#1A1A22` dots, 20px grid, 1px dots, 20% opacity).

**Top bar (40px):**
- Left: "TRADEENGINE" JetBrains Mono bold acid green + current view label
- Right: clock, market status badge (LIVE = emerald / CLOSED = red), connection pulse dot
- Background: `#0A0A0D` with 1px bottom border `#1A1A22`

**Left sidebar (48px icon rail):**
- Collapsed by default, icons only
- Active icon: acid green
- Background: `#0D0D11`

**Bottom status bar (32px):**
- JetBrains Mono 11px, `#6B7280`
- Last signal | Last trade | System uptime | Mode badge

**Pipeline nodes (use `@xyflow/react`):**

| Node | Width | Height | Left strip colour |
|---|---|---|---|
| SIGNAL SOURCE | 220px | 120px | `#4A5568` slate |
| ARBITER | 280px | 140px | `#39FF14` acid green |
| TECHNICAL | 220px | 120px | *(no left strip)* |
| MOMENTUM | 220px | 120px | *(no left strip)* |
| SENTIMENT | 220px | 120px | *(no left strip)* |
| MACRO | 220px | 120px | *(no left strip — 35% opacity when bypassed)* |
| SYNTHESIS | 220px | 120px | `#F59E0B` amber |
| EXECUTION | 220px | 120px | Status-driven |

**Note on left strips:** SIGNAL SOURCE, ARBITER, and SYNTHESIS retain their left strips — they're non-analyst structural nodes. Analyst nodes (Technical, Momentum, Sentiment, Macro) use corner bracket colour instead of left strips.

**Edges:** Bezier curves via `@xyflow/react`. Active: `#39FF14` 1px animated dash. Inactive: `#1A1A22`.

**Visual reference:** `FINAL COMPOSITION — Hub & Spoke.png`

---

### 3. Signal Feed Panel

**Type:** Slide-in panel from right, 380px wide.  
**Panel bg:** `#0D0D11`. Chamfer: top-left only. No border rail on right edge.

**Header:**
- "SIGNAL FEED" section label
- Filter chips: ALL / PENDING / APPROVED / EXECUTED / BLOCKED / EXPIRED
- Chip style: chamfered, active = acid green bg + black text, inactive = `#111115` bg + `#6B7280` text

**Signal entry rows:**
- Corner bracket notation (`⌐ ¬`) — colour = signal direction (emerald for BUY, red for SELL)
- Internal padding: **min 16px horizontal, 12px vertical** — text must be visually inside the brackets
- Row layout: direction badge | ticker | strategy tag | analyst tags | confidence | timestamp
- Right-click context menu: Pin / Override / Suppress source app

**Footer stats:** `68% LONG / 21% SHORT / 11% NEUT` — JetBrains Mono 11px muted

**Visual reference:** `Signal Feed Panel — Context View.png`

---

### 4. Analyst Detail Panel

**Type:** Slide-in panel from right, 480px wide. Opens when a signal row is selected.  
**Panel bg:** `#0D0D11`. Chamfer: top-left only.

**Header (56px):**
- Ticker (Space Grotesk 18px bold), direction badge (chamfered, acid green for LONG)
- Timestamp (JetBrains Mono 11px `#4B5563`)
- Strategy label (JetBrains Mono 11px `#6B7280`)
- Confidence score right-aligned

**Routed Analysts section:**
- Label: "ROUTED: N of 4 analysts" — JetBrains Mono 10px `#4B5563`
- Only show analysts that were actually routed — do not show unrouted analysts
- Each analyst card:
  - Corner bracket colour = analyst type colour
  - Analyst name left, VERDICT right on same baseline
  - Confidence bar: max 60% width, left-aligned, colour = analyst bracket colour
  - One-line reasoning in 10px muted gray

**MACRO analyst (when bypassed):** Entire card at 35% opacity. "BYPASSED" instead of verdict.

**Synthesis card (bg `#141418`):**
- Vote weight bars (one per active analyst)
- Final direction large in acid green
- Weighted confidence in JetBrains Mono

**Execution status:**
- ARMED in amber with pulsing dot
- Entry / SL / TP in monospace

**Footer:** OVERRIDE (red outlined chamfered) | CLOSE (muted outlined chamfered)

**Panel height terminates at content. No dead zone below footer.**

**Visual reference:** `Analyst Detail Panel.png`

---

### 5. Trade Execution Log

**Type:** Full panel view.

**Header:**
- "EXECUTION LOG" section label
- Summary chips: TOTAL TRADES | WIN RATE | NET P&L (acid green)
- Filter chips: ALL / LONG / SHORT / OPEN / CLOSED

**Table columns (left to right):**
TIMESTAMP | PAIR | DIRECTION | STRATEGY | ENTRY | EXIT | P&L | MODEL | STATUS

```
TIMESTAMP:  JetBrains Mono 11px #4B5563
PAIR:       JetBrains Mono 13px bold #F0F0F0
DIRECTION:  chamfered badge — BUY #22C55E, SELL #EF4444
STRATEGY:   JetBrains Mono 11px #9CA3AF (TREND/MEAN_REV/MOMENTUM/NEWS_SENT)
ENTRY:      JetBrains Mono 12px #F0F0F0
EXIT:       JetBrains Mono 12px #F0F0F0 ("OPEN" in #F59E0B if live)
P&L:        JetBrains Mono 13px bold — positive #22C55E, negative #EF4444
MODEL:      JetBrains Mono 10px #6B7280 (e.g. "gpt-4o", "claude-sonnet")
STATUS:     chamfered badge — CLOSED #4B5563 / OPEN #F59E0B pulsing / CANCELLED #EF4444
```

**Row zebra:** alternating `#0A0A0D` and `#0D0D11` — very subtle, no grid lines.

**Expanded row:**
- Background `#141418`
- Corner brackets tight to content (close at last text line — not a fixed box)
- Shows: analyst votes, ensemble result, signal trigger, fill price, slippage, quantity, signal ID

**Footer:**
- EXPORT CSV: acid green outline + text, chamfered
- Pagination: JetBrains Mono muted right-aligned
- **No dead zone below footer**

**Visual reference:** `Trade Execution Log.png`

---

### 6. Settings Panel

**Type:** Full panel view.  
**Layout:** Two-column scrollable layout.

**Left column — ENGINE & RISK:**

*Engine section:*
- Mode: three-way toggle MANUAL | PAPER | AUTO
  - PAPER = gold `#F59E0B` bg, black text. AUTO = red warning dot.
- Tick interval: segmented selector 5m | 15m | 1h | 4h | 1d (acid green active)
- Dry Run: toggle — when ON shows "NO ORDERS PLACED" in amber
- Initial Capital: numeric input JetBrains Mono

*Risk Thresholds:*
All 8 fields from `config.yaml`. Label left, value right. JetBrains Mono.
- `max_drawdown_pct` label in red — safety-critical visual treatment
- Fields: confidence_threshold, min_risk_reward, max_position_pct, max_open_positions, daily_loss_limit_pct, max_drawdown_pct, cooldown_after_loss_min, signal_expiry_min

*Strategies:*
- Trend Following | Mean Reversion | Momentum | News Sentiment
- Each: toggle ON/OFF + expandable inline params (chevron)
- Expanded shows relevant config fields (EMA period, ADX min, RSI range etc.)

**Right column — MODELS, COST & NOTIFICATIONS:**

*Model Roles:*
- 5 rows: analysts (×3) + arbiter + classifier
- Role name left, assigned model right
- Swap button: outlined chamfered muted

*Cost Profile:*
- BUDGET | BALANCED | PRO segmented selector
- Daily + weekly token budget inputs
- Auto-degrade toggle

*Notifications:*
- Telegram toggle
- Notify-on checkboxes (chamfered tick boxes, acid green when checked)
- Batch enabled toggle + window input
- Min confidence for alert + include HOLDs toggle

*System Actions:*
- RESTART ENGINE: amber outlined chamfered full-width
- RESET TO DEFAULTS: red outlined chamfered full-width
- Version string muted

**Footer (full width):**
- SAVE CHANGES: acid green fill, black text, chamfered
- DISCARD: muted outlined chamfered
- **Footer follows content. No dead zone.**

**Visual reference:** `Settings Panel.png`

---

### 7. Portfolio Overview

**Type:** Full panel view — the emotional centrepiece.

**Header stat cards (5, side by side, chamfered `#111115`):**
- TOTAL VALUE | CASH | INVESTED | UNREALIZED P&L | DAY P&L
- UNREALIZED P&L: most prominent — acid green border, 28px Space Grotesk bold
- Positive values: `#22C55E`. Negative: `#EF4444`.
- Labels: JetBrains Mono 10px uppercase `#6B7280`

**Equity curve (~200px tall, full width):**
- Line: `#39FF14` with `text-shadow`/`drop-shadow` glow effect
- Fill below line: `rgba(57, 255, 20, 0.08)`
- Background: `#0A0A0D`
- Grid lines: `#1A1A22` — no visible axes
- Date labels: JetBrains Mono 10px `#4B5563`
- Current value: acid green dot at rightmost point
- Peak: dashed line with "#PEAK" label muted

**Open Positions table:**
Columns: TICKER | DIRECTION | STRATEGY | ENTRY | CURRENT | UNREALIZED P&L | P&L% | STATUS
- Mix of positive and negative unrealized P&L
- STATUS: OPEN in amber with pulsing dot

**Performance Metrics (two columns):**

Left — Period Stats (Today / Week / Month / All Time): label muted, value green/red

Right — System Stats: Win Rate, Total Trades, Avg Hold, Max Drawdown (red), Sharpe, Best/Worst trade

**Visual reference:** `Portfolio Overview.png`

---

## Existing Code to Understand First

Before writing any new code, read these files:

```
web/frontend/src/App.tsx
web/frontend/src/components/PipelineGraph.tsx
web/frontend/src/components/PipelineNode.tsx
web/frontend/src/components/AnimatedEdge.tsx
web/frontend/src/components/DrillDownPanel.tsx
web/frontend/src/components/StatusBar.tsx
web/frontend/src/hooks/useEngineSocket.ts
```

The WebSocket hook (`useEngineSocket.ts`) is how live data flows in from the Go backend. Use it for real-time portfolio state, open signals, and trade updates. Do not build a separate data layer.

---

## Implementation Priority Order

1. Design tokens file (`tokens.ts`) + shared component primitives (chamfered card, corner brackets, badge)
2. Bring existing PipelineGraph/PipelineNode up to full spec (colours, chamfer, edge animation)
3. Signal Feed Panel
4. Analyst Detail Panel
5. Trade Execution Log
6. Boot Screen (can be done at any point — it's standalone)
7. Settings Panel
8. Portfolio Overview

---

## Known Implementation Notes

- **Signal Feed card padding:** Text must sit min 16px inside card borders — corner brackets need to visually contain the text, not sit on the same plane
- **Equity curve glow:** Use SVG `filter: drop-shadow` or Canvas API for the acid green line glow
- **Pulsing OPEN status dot:** CSS `@keyframes pulse` on `box-shadow`
- **EXECUTION flash animation:** 1-second acid green border flash on state change → LIVE. CSS `@keyframes` on `border-color` + `box-shadow`
- **Corner bracket height:** Use `position: relative` container with absolutely positioned `⌐` and `¬` — the container height is driven by content, so brackets naturally hug it

---

## Visual References (Pencil.dev screenshots — approved)

All screenshots were approved by Romeo during the design session on 2026-03-16.

- `Boot Screen — TradeEngine.png`
- `FINAL COMPOSITION — Hub & Spoke.png`
- `Signal Feed Panel — Context View.png`
- `node-components.png`
- `Analyst Detail Panel.png`
- `Trade Execution Log.png`
- `Settings Panel.png`
- `Portfolio Overview.png`

Romeo will share these files directly with you when briefing you on this task.

---

## Design Session Log

Full session log with all design decisions, correction history, and rule evolution:  
`~/superteam-ops/outbox/desire-session-logs-tradeengine-20260316.md`

Read this if you need to understand *why* a decision was made, not just what it is.

---

*— Desire (Designer) | 2026-03-16*
