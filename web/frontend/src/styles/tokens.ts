/**
 * Design tokens — curated JS subset of tokens.css.
 *
 * tokens.css is the authoritative source of truth for all design tokens.
 * This file is a hand-maintained subset covering only the values needed by
 * JS runtime logic (canvas drawing, inline styles, dynamic class selection).
 * It does NOT mirror every CSS variable — see tokens.css for the full set.
 *
 * When adding/changing a colour in tokens.css that is used in JS, update here too.
 */
export const TOKENS = {
  // Accents — matches tokens.css
  acidGreen:   '#39FF14',
  amber:       '#F59E0B',
  red:         '#EF4444',
  emerald:     '#22C55E',
  blue:        '#0EA5E9',   // --blue
  cyan:        '#00F5FF',   // particle default / neon cyan (used as fallback)
  indigo:      '#6366F1',
  violet:      '#8B5CF6',
  slate:       '#4A5568',

  // Surfaces — matches tokens.css
  canvasBg:    '#0A0A0D',   // --canvas-bg / --bg
  panelBg:     '#0D0D11',   // --panel-bg / --surface
  cardBg:      '#111115',   // --card-bg
  border:      '#2A2A35',   // --border
  borderMuted: '#1A1A22',   // --border-muted

  // Text — matches tokens.css
  textPrimary: '#F0F0F0',   // --text-primary
  textMuted:   '#9CA3AF',   // --text-muted
  textDim:     '#6B7280',   // --text-dim
  textFaint:   '#4B5563',   // --text-faint
} as const;
