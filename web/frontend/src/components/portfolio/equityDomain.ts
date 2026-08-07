/**
 * Fit an equity chart to its visible values instead of Recharts' zero-anchored
 * numeric default. The padding keeps extrema clear of the chart frame and also
 * gives a genuinely flat series a visible, non-zero domain.
 */
export function paddedEquityDomain(values: number[]): [number, number] | undefined {
  const finiteValues = values.filter(Number.isFinite);
  if (finiteValues.length === 0) return undefined;

  const min = Math.min(...finiteValues);
  const max = Math.max(...finiteValues);
  const padding = max === min
    ? Math.abs(max) * 0.01 || 1
    : (max - min) * 0.08;

  return [min - padding, max + padding];
}
