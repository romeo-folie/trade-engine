# Design Reference — TradeEngine Web UI

Source: `~/Desktop/trade-engine-ui/ui.pen` (Pencil.dev)

All PNGs are exported from the live .pen file via the Pencil MCP.
To refresh: `mcporter call pencil.export_nodes filePath="~/Desktop/trade-engine-ui/ui.pen" nodeIds=[...] outputDir=".claude/designs"`

## Screens

| File | Pencil Node | Screen | Task | Notes |
|------|-------------|--------|------|-------|
| `pipeline-main.png` | `kwjo3` | Hub & Spoke Pipeline Canvas | Task 3 | Source of truth |
| `signal-feed-panel.png` | `zu5LO` | Signal Feed Panel (sidebar) | Task 3 | Source of truth |
| `1jS1W.png` | `1jS1W` | Signals Screen (full page) | Task 5 | Updated 2026-03-18 |
| `hUwqt.png` | `hUwqt` | Trade Execution Log | Task 5 | Updated 2026-03-18 |
| `Vln6C.png` | `Vln6C` | Analyst Detail Panel | Task 5 | Updated 2026-03-18 |
| `portfolio-overview.png` | `F2s5p` | Portfolio Overview | Task 4 | Source of truth |
| `boot-screen.png` | `3Uf0J` | Boot Screen | Task 7 | Source of truth |
| `settings-panel.png` | `Of5uA` | Settings Panel | Task 7 | Source of truth |
| `component-sheet.png` | `fwN61` | Component Sheet / Node Library | Reference | Source of truth |

## Live Access

Pencil MCP is connected (`mcporter list pencil`). For fresh screenshots of any node:
```
mcporter call pencil.get_screenshot filePath="~/Desktop/trade-engine-ui/ui.pen" nodeId=<ID>
```

## Key Design Tokens (from designs)

- **Background**: `#0A0E17` (very dark navy-black, slight blue tint)
- **Accent (green)**: `#00FF66` / `#39FF14` (acid green)
- **Secondary**: Amber `#F59E0B`, Red `#EF4444`, Purple `#9966FF`
- **Text primary**: `#E0E0E0`
- **Text muted**: `#6B7280`
- **Typography**: JetBrains Mono (monospace throughout)
- **Card borders**: 1px thin green (`#00FF66` at low opacity)
- **Corner motif**: L-shaped bracket accents on cards (top-left + bottom-right)
- **Background grid**: Very faint subtle scan-line / grid texture
