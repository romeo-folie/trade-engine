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

// Keep native disclosure controls keyboard-reachable inside the modal focus
// trap. <summary> is interactive even though it is not one of the traditional
// form controls covered by many focusable-element selectors.
export const ANALYSIS_MODAL_FOCUSABLE_SELECTOR =
  'button, summary, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])';

interface DecisionAnalyst {
  role?: string;
  description?: string;
}

export function decisionSummary(
  persistedReasoning: string,
  arbiterReasoning?: string,
  analysts: DecisionAnalyst[] = [],
): string {
  if (arbiterReasoning?.trim()) return arbiterReasoning.trim();

  const riskManager = analysts.find(
    analyst => analyst.role?.toLowerCase() === 'risk_manager' && analyst.description?.trim(),
  );
  if (riskManager?.description) return riskManager.description.trim();

  // Specialist persistence contains one line per role followed by a resolution
  // marker. Surface only the final risk-manager rationale here; the complete
  // role explanations remain available in the collapsed analyst cards.
  const riskLine = persistedReasoning
    .split('\n')
    .map(line => line.trim())
    .find(line => line.startsWith('[risk_manager]'));
  if (riskLine) {
    const withoutPrefix = riskLine.replace(/^\[risk_manager\]\s+\S+\s+[—-]\s+/, '');
    return withoutPrefix.replace(/\s+\(conf\s+[\d.]+\)$/, '').trim();
  }

  return persistedReasoning;
}
