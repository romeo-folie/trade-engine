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
  acidGreen:      '#39FF14',
  accentGlow:     'rgba(57, 255, 20, 0.15)',  // --accent-glow
  accentGlowLg:   'rgba(57, 255, 20, 0.08)',  // --accent-glow-lg
  amber:       '#F59E0B',
  red:         '#EF4444',
  emerald:     '#22C55E',
  blue:        '#0EA5E9',   // --blue
  cyan:        '#00F5FF',   // particle default / neon cyan (used as fallback)
  indigo:      '#6366F1',
  violet:      '#8B5CF6',
  slate:       '#4A5568',

  // Surfaces — matches tokens.css
  canvasBg:    '#0D1117',   // --canvas-bg / --bg
  panelBg:     '#0D1117',   // --panel-bg
  cardBg:      '#111820',   // --card-bg
  border:      '#1E2D2D',   // --border
  borderMuted: '#151E1E',   // --border-muted

  // Text — matches tokens.css
  textPrimary: '#F0F0F0',   // --text-primary
  textMuted:   '#9CA3AF',   // --text-muted
  textDim:     '#6B7280',   // --text-dim
  textFaint:   '#4B5563',   // --text-faint
} as const;
