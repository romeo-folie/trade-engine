/**
 * BacktestDetailPanel — slide-in panel from right showing run stats + trade list.
 *
 * Fetches trades via /api/backtests/runs/{id}/trades on open.
 * Includes equity curve (mock data since no history endpoint), stats, and trade table.
 */
import { useState, useEffect, useRef, useCallback } from 'react';
import { fmtPrice } from '../../utils/format';
import EquityCurve from '../portfolio/EquityCurve';
import type { BacktestRun, BacktestTrade } from '../../screens/Backtests';
import styles from './BacktestDetailPanel.module.css';

interface BacktestDetailPanelProps {
  run: BacktestRun;
  onClose: () => void;
}

function fmtPct(v: number | null): string {
  if (v == null) return '—';
  return `${v >= 0 ? '+' : ''}${v.toFixed(2)}%`;
}

/** Generate mock equity curve from trades for visualization. */
function mockEquityCurve(trades: BacktestTrade[]): { date: string; value: number }[] {
  if (trades.length === 0) return [];
  let equity = 10000;
  const points: { date: string; value: number }[] = [
    { date: trades[0].EntryTS.slice(0, 10), value: equity },
  ];
  for (const t of trades) {
    if (t.PnLPct != null) {
      equity *= (1 + t.PnLPct / 100);
    }
    const dateStr = (t.ExitTS ?? t.EntryTS).slice(0, 10);
    points.push({ date: dateStr, value: Math.round(equity * 100) / 100 });
  }
  return points;
}

export default function BacktestDetailPanel({ run, onClose }: BacktestDetailPanelProps) {
  const [trades, setTrades] = useState<BacktestTrade[]>([]);
  const [tradesLoading, setTradesLoading] = useState(true);
  const [tradesError, setTradesError] = useState<string | null>(null);
  const panelRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const controller = new AbortController();
    setTradesLoading(true);
    setTradesError(null);

    fetch(`/api/backtests/runs/${run.ID}/trades`, { signal: controller.signal })
      .then(res => {
        if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
        return res.json();
      })
      .then(data => {
        setTrades(Array.isArray(data.trades) ? data.trades : []);
      })
      .catch(err => {
        if (err instanceof Error && err.name !== 'AbortError') {
          setTradesError(err.message);
        }
      })
      .finally(() => setTradesLoading(false));

    return () => controller.abort();
  }, [run.ID]);

  const handleKey = useCallback((e: KeyboardEvent) => {
    if (e.key === 'Escape') onClose();
    if (e.key === 'Tab' && panelRef.current) {
      const focusable = panelRef.current.querySelectorAll<HTMLElement>(
        'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
      );
      if (focusable.length === 0) return;
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (e.shiftKey) {
        if (document.activeElement === first) { e.preventDefault(); last?.focus(); }
      } else {
        if (document.activeElement === last) { e.preventDefault(); first?.focus(); }
      }
    }
  }, [onClose]);

  useEffect(() => {
    document.addEventListener('keydown', handleKey);
    panelRef.current?.focus();
    return () => document.removeEventListener('keydown', handleKey);
  }, [handleKey]);

  const equityData = mockEquityCurve(trades);

  return (
    <>
      <div className={styles.backdrop} onClick={onClose} />
      <div
        className={`${styles.panel} chamfer-panel-right`}
        ref={panelRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-label={`Backtest detail for ${run.Label}`}
      >
        {/* Header */}
        <div className={styles.header}>
          <div className={styles.headerLeft}>
            <span className={styles.title}>{run.Label}</span>
            <span className={styles.headerMeta}>
              {run.Ticker} · {run.Strategy} · {run.ModelProvider}/{run.ModelName}
            </span>
          </div>
          <button type="button" className={`${styles.closeBtn} chamfer-sm`} onClick={onClose} aria-label="Close panel">
            ESC
          </button>
        </div>

        {/* Body */}
        <div className={styles.body}>
          {/* Stats grid */}
          <div className={styles.statsGrid}>
            <div className={`${styles.statCard} chamfer-sm`}>
              <span className={styles.statLabel}>Return</span>
              <span className={`${styles.statValue} ${run.TotalReturnPct != null && run.TotalReturnPct >= 0 ? styles.emerald : styles.red}`}>
                {fmtPct(run.TotalReturnPct)}
              </span>
            </div>
            <div className={`${styles.statCard} chamfer-sm`}>
              <span className={styles.statLabel}>Win Rate</span>
              <span className={styles.statValue}>{fmtPct(run.WinRatePct)}</span>
            </div>
            <div className={`${styles.statCard} chamfer-sm`}>
              <span className={styles.statLabel}>Max DD</span>
              <span className={`${styles.statValue} ${styles.red}`}>{fmtPct(run.MaxDrawdownPct)}</span>
            </div>
            <div className={`${styles.statCard} chamfer-sm`}>
              <span className={styles.statLabel}>Sharpe</span>
              <span className={styles.statValue}>{run.SharpeRatio != null ? run.SharpeRatio.toFixed(2) : '—'}</span>
            </div>
          </div>

          {/* Equity curve */}
          {equityData.length > 1 && (
            <div>
              <div className={styles.sectionLabel}>EQUITY CURVE</div>
              <div className={styles.chartWrap}>
                <EquityCurve data={equityData} />
              </div>
            </div>
          )}

          {/* Trades */}
          <div>
            <div className={styles.sectionLabel}>
              TRADES ({tradesLoading ? '…' : trades.length})
            </div>

            {tradesError && (
              <div className={styles.tradesError}>Failed to load trades</div>
            )}

            {!tradesLoading && trades.length > 0 && (
              <div className={styles.tradeTableWrap}>
                <table className={styles.tradeTable}>
                  <thead>
                    <tr>
                      <th>DIR</th>
                      <th>ENTRY</th>
                      <th>EXIT</th>
                      <th>P&L</th>
                      <th>REASON</th>
                    </tr>
                  </thead>
                  <tbody>
                    {trades.map(t => (
                      <tr key={t.ID}>
                        <td className={t.Direction.toLowerCase() === 'buy' ? styles.emerald : styles.red}>
                          {t.Direction.toUpperCase()}
                        </td>
                        <td>{fmtPrice(t.EntryPrice)}</td>
                        <td>{t.ExitPrice != null ? fmtPrice(t.ExitPrice) : '—'}</td>
                        <td className={t.PnLPct != null && t.PnLPct >= 0 ? styles.emerald : styles.red}>
                          {fmtPct(t.PnLPct)}
                        </td>
                        <td className={styles.dimText}>{t.ExitReason || '—'}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}

            {!tradesLoading && trades.length === 0 && !tradesError && (
              <div className={styles.noTrades}>No trades recorded</div>
            )}
          </div>
        </div>

        {/* Footer */}
        <div className={styles.footer}>
          <button type="button" className={`${styles.btnClose} chamfer-sm`} onClick={onClose}>
            CLOSE
          </button>
        </div>
      </div>
    </>
  );
}
