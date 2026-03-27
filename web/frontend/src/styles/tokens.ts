/**
 * tokens.ts — design spec colour and typography tokens.
 * As per DESIGN_SPEC.md: "use these everywhere, no raw hex values in components"
 */

export const colors = {
  // Base
  canvas:       '#050507',  // main canvas background (near-black)
  panelBg:      '#0B0B10',  // slide-in panel background
  cardBg:       '#131319',  // card / node background
  cardBgAlt:    '#16161D',  // slightly brighter card
  border:       '#1A1A22',  // default border (darker for distinction)
  borderMuted:  '#121218',  // nearly invisible border

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
  accentGlow:   'rgba(57, 255, 20, 0.15)',
  accentGlowLg: 'rgba(57, 255, 20, 0.08)',
  accentGlowStrong: 'rgba(57, 255, 20, 0.50)',

  // Analyst type colours (bracket colour per analyst)
  analystTechnical:  '#6366F1',
  analystMomentum:   '#F59E0B',
  analystSentiment:  '#8B5CF6',
  analystMacro:      '#0EA5E9',
};

// Legacy alias to maintain compatibility with older components
export const TOKENS = colors;

export const typography = {
  mono: '"JetBrains Mono", monospace',
  sans: '"Space Grotesk", sans-serif',
  display: '"Orbitron", sans-serif',
};
