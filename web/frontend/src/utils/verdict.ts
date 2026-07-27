/**
 * Verdict vocabulary shared by the signal and analysis surfaces.
 *
 * The API renames the directional actions (BUY→LONG, SELL→SHORT) and passes
 * every other engine action through unchanged, so these surfaces have to handle
 * more than three values:
 *
 *   LONG  / SHORT — directional conviction
 *   TRADE         — ORB candidate cleared the gate
 *   WAIT          — real catalyst, poor entry timing; re-evaluate next tick
 *   ADD           — scanner wants the ticker on the watchlist
 *   HOLD          — drop this ticker; genuine disengagement
 *   SKIP          — ORB candidate was filtered out
 *   NEUTRAL       — no classifiable action (an analyst that errored)
 *
 * Both panels previously folded everything non-directional into a single
 * NEUTRAL label in one shared amber, so "drop this ticker" and "re-evaluate
 * next tick" were indistinguishable at a glance.
 */

/** Normalises a raw engine action or an API verdict to its display label. */
export function verdictLabel(value: string | null | undefined): string {
  const v = (value ?? '').trim().toUpperCase();
  if (v === 'BUY' || v === 'LONG') return 'LONG';
  if (v === 'SELL' || v === 'SHORT') return 'SHORT';
  return v || 'NEUTRAL';
}

/**
 * Colour by what the verdict asks the operator to do, not by its wording:
 * act now (accent / red), keep watching (amber), or stand down (dim).
 */
export function verdictColor(value: string | null | undefined): string {
  switch (verdictLabel(value)) {
    case 'LONG':
      return 'var(--accent)';
    case 'SHORT':
      return 'var(--red)';
    // ORB's TRADE is a "go" like a directional call, just without a side.
    case 'TRADE':
      return 'var(--accent)';
    // Still live, just not yet — the token palette already reserves amber for WAIT.
    case 'WAIT':
    case 'ADD':
      return 'var(--amber)';
    // HOLD, SKIP and NEUTRAL all mean "nothing to do here".
    default:
      return 'var(--text-dim)';
  }
}
