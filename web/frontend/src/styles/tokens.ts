/**
 * Design tokens (JS mirror of tokens.css).
 *
 * Single source of truth for colours used in JS runtime logic
 * (inline styles, dynamic class selection, canvas drawing).
 * Keep in sync with tokens.css — any change there must be reflected here.
 */
export const TOKENS = {
  // Accents
  acidGreen:   '#39FF14',
  amber:       '#F59E0B',
  red:         '#EF4444',
  emerald:     '#22C55E',
  blue:        '#0EA5E9',
  indigo:      '#6366F1',
  violet:      '#8B5CF6',
  sky:         '#0EA5E9',
  slate:       '#4A5568',

  // Surfaces
  border:      '#2A2A35',
  borderMuted: '#1A1A22',
  cardBg:      '#111115',

  // Text
  textPrimary: '#F0F0F0',
  textMuted:   '#9CA3AF',
  textDim:     '#6B7280',
} as const;
