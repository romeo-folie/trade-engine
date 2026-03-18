/** Format a number as a USD price string (e.g. 83241.50 → "$83,241.50") */
export function fmtPrice(n: number): string {
  if (!Number.isFinite(n)) return '—';
  return '$' + n.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

/** Format an ISO datetime string to a short local date+time (e.g. "Mar 18, 14:23") */
export function fmtDate(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  return d.toLocaleString('en-US', {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false,
  });
}
