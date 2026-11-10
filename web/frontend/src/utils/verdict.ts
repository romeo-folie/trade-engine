/**
 * Verdict vocabulary shared by the signal and analysis surfaces.
 *
 * The API renames BUY→LONG and passes every other engine action through
 * unchanged. Since #599, SELL is a long exit, SHORT opens a short, and COVER
 * buys a short back, so these surfaces have to handle more than three values:
 *
 *   LONG          — open (or add to) a long
 *   SHORT         — open a short
 *   SELL          — exit a long
 *   COVER         — buy back a short
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
  // SELL is a long exit, never a short (#599); SHORT and COVER pass through.
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
    // Exits: a SELL closes a long (red, like the side it sells into) and a
    // COVER buys a short back (accent, the buying side).
    case 'SELL':
      return 'var(--red)';
    case 'COVER':
      return 'var(--accent)';
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

/**
 * Consensus verdict across analyst votes, mirroring the API's
 * deriveEnsembleVerdict (internal/web/types.go) exactly, so the live pipeline
 * and the signal detail can never disagree on the same votes. Votes are raw
 * engine actions or API verdicts; they are counted per label, never pooled
 * (SELL and SHORT are distinct). Ties resolve by the API's precedence: HOLD,
 * LONG tied with a bearish verdict → HOLD, then LONG, SHORT, SELL, COVER,
 * SKIP, TRADE, WAIT, ADD.
 */
export function consensusVerdict(votes: ReadonlyArray<string | null | undefined>): string {
  const counts = new Map<string, number>();
  for (const v of votes) {
    const label = verdictLabel(v);
    counts.set(label, (counts.get(label) ?? 0) + 1);
  }
  const max = Math.max(0, ...counts.values());
  if (max === 0) return 'NEUTRAL';
  const leads = (label: string) => counts.get(label) === max;
  if (leads('HOLD')) return 'HOLD';
  if (leads('LONG') && (leads('SHORT') || leads('SELL'))) return 'HOLD';
  for (const label of ['LONG', 'SHORT', 'SELL', 'COVER', 'SKIP', 'TRADE', 'WAIT', 'ADD']) {
    if (leads(label)) return label;
  }
  return 'NEUTRAL';
}
