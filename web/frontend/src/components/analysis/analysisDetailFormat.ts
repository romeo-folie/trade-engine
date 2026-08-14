export function titleCase(value: string): string {
  return value.replace(/_/g, ' ').replace(/\b\w/g, char => char.toUpperCase());
}

export function formatIndicator(value: number | undefined, digits = 2): string {
  // IndicatorSet uses zero as its unavailable sentinel when there was not
  // enough candle history to compute a value.
  if (value == null || !Number.isFinite(value) || value === 0) return 'Unavailable';
  return value.toLocaleString('en-US', {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  });
}

export function shouldShowEntryChecks(action: string | undefined, checkCount: number): boolean {
  return action?.toUpperCase() === 'BUY' && checkCount > 0;
}
