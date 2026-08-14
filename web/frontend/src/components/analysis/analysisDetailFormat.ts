export function titleCase(value: string): string {
  return value.replace(/_/g, ' ').replace(/\b\w/g, char => char.toUpperCase());
}

export function formatIndicator(value: number | undefined, digits = 2): string {
  if (value == null || !Number.isFinite(value)) return 'Unavailable';
  return value.toLocaleString('en-US', {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  });
}

export function shouldShowEntryChecks(action: string | undefined, checkCount: number): boolean {
  return action?.toUpperCase() === 'BUY' && checkCount > 0;
}
