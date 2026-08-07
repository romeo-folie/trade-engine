export interface BacktestCurveTrade {
  EntryTS: string;
  ExitTS: string | null;
  PnL: number | null;
}

/** Build a realized-P&L equity curve in exit order without mutating API data. */
export function buildBacktestEquityCurve(
  trades: BacktestCurveTrade[],
  initialCash: number,
  startDate: string,
): { date: string; value: number }[] {
  if (trades.length === 0) return [];

  const realizedTrades = [...trades].sort((a, b) =>
    (a.ExitTS ?? a.EntryTS).localeCompare(b.ExitTS ?? b.EntryTS),
  );
  let equity = initialCash;
  const points = [{ date: startDate.slice(0, 10), value: equity }];
  for (const trade of realizedTrades) {
    if (trade.PnL != null) equity += trade.PnL;
    points.push({
      date: (trade.ExitTS ?? trade.EntryTS).slice(0, 10),
      value: Math.round(equity * 100) / 100,
    });
  }
  return points;
}
