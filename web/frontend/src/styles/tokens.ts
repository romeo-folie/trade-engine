/**
 * Design tokens — JS mirror of tokens.css.
 *
 * tokens.css is the authoritative source of truth.
 * This file provides the same values for JS runtime logic
 * (inline styles, dynamic class selection, canvas drawing).
 * When tokens.css changes, update this file to match.
 */
export const TOKENS = {
  // Accents — matches tokens.css
  acidGreen:   '#39FF14',
  amber:       '#F59E0B',
  red:         '#EF4444',
  emerald:     '#22C55E',
  blue:        '#0EA5E9',   // --blue
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
