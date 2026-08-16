/**
 * Backtests screen — table of backtest runs with slide-in detail panel.
 *
 * Fetches from GET /api/backtests/runs?limit=50 on mount.
 * Row click opens detail panel with equity curve + trade list.
 *
 * Task 8 of the web redesign (closes #206).
 */
import { useState, useEffect, useRef, useCallback } from 'react';
import { fmtDate } from '../utils/format';
import BacktestDetailPanel from '../components/backtests/BacktestDetailPanel';
import styles from './Backtests.module.css';

/* ── Types ─────────────────────────────────────────────────────────────────── */

export interface BacktestRun {
  ID: string;
  Label: string;
  Ticker: string;
  Strategy: string;
  ModelProvider: string;
  ModelName: string;
  FromDate: string;
  ToDate: string;
  TotalReturnPct: number | null;
  WinRatePct: number | null;
  MaxDrawdownPct: number | null;
  SharpeRatio: number | null;
  TotalTrades: number;
  CreatedAt: string;
  /** Parsed config snapshot — contains initial_cash used to seed the equity curve. */
  ConfigSnapshot: { initial_cash?: number } | null;
}

export interface BacktestTrade {
  ID: string;
  Ticker: string;
  Direction: string;
  EntryTS: string;
  ExitTS: string | null;
  EntryPrice: number;
  ExitPrice: number | null;
  PnL: number | null;
  PnLPct: number | null;
  ExitReason: string;
}

/* ── Helpers ───────────────────────────────────────────────────────────────── */

function fmtPct(v: number | null): string {
  if (v == null) return '—';
  return `${v >= 0 ? '+' : ''}${v.toFixed(2)}%`;
}

/** MaxDrawdownPct is stored as a positive magnitude — always render as a loss. */
function fmtDrawdown(v: number | null): string {
  if (v == null) return '—';
  return `-${Math.abs(v).toFixed(2)}%`;
}

function fmtSharpe(v: number | null): string {
  if (v == null) return '—';
  return v.toFixed(2);
}

/* ── Table columns ─────────────────────────────────────────────────────────── */

const COLUMNS = [
  'LABEL', 'TICKER', 'STRATEGY', 'MODEL',
  'RETURN%', 'WIN RATE', 'MAX DD', 'SHARPE', 'TRADES', 'DATE',
];

/* ── Component ─────────────────────────────────────────────────────────────── */

export default function BacktestsScreen() {
  const [runs, setRuns] = useState<BacktestRun[]>([]);
  const [isLoading, setIsLoading] = useState(true);
  const [fetchError, setFetchError] = useState<string | null>(null);
  const [selected, setSelected] = useState<BacktestRun | null>(null);
  const fetchControllerRef = useRef<AbortController | null>(null);

  useEffect(() => {
    async function load() {
      fetchControllerRef.current?.abort();
      const controller = new AbortController();
      fetchControllerRef.current = controller;

      try {
        const res = await fetch('/api/backtests/runs?limit=50', { signal: controller.signal });
        if (!res.ok) {
          setFetchError(`${res.status} ${res.statusText}`);
          return;
        }
        const data = await res.json();
        if (data && Array.isArray(data.runs)) {
          setRuns(data.runs);
          setFetchError(null);
        } else {
          setFetchError('Unexpected response shape');
        }
      } catch (err) {
        if (err instanceof Error && err.name !== 'AbortError') {
          console.warn('[BacktestsScreen] fetch error:', err);
          setFetchError(err.message);
        }
      } finally {
        setIsLoading(false);
      }
    }

    void load();
    return () => { fetchControllerRef.current?.abort(); };
  }, []);

  const handleClose = useCallback(() => setSelected(null), []);

  return (
    <div className={styles.screen}>
      <div className={styles.headerRow}>
        <div className={styles.breadcrumb}>
          <span className={styles.breadcrumbBrand}>~/</span>
          <span className={styles.breadcrumbPage}>BACKTESTS</span>
        </div>
      </div>

      <h1 className={styles.pageTitle}>BACKTESTS</h1>

      {fetchError && (
        <div className={styles.fetchErrorBanner} role="alert">
          {runs.length > 0 ? '⚠ Data may be stale' : '⚠ Failed to load backtests'}
        </div>
      )}

      {isLoading && runs.length === 0 && (
        <div className={styles.loadingBanner} role="status" aria-label="Loading backtests">
          Loading…
        </div>
      )}

      {!isLoading && (
        <>
          <div className={styles.tableWrap}>
            <table className={styles.table} aria-label="Backtest runs">
              <thead>
                <tr>
                  {COLUMNS.map(col => (
                    <th key={col}>{col}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {runs.map(run => (
                  <tr
                    key={run.ID}
                    className={`${styles.row} ${selected?.ID === run.ID ? styles.rowSelected : ''}`}
                    onClick={() => setSelected(run)}
                    tabIndex={0}
                    onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setSelected(run); } }}
                  >
                    <td className={styles.mono}>{run.Label}</td>
                    <td className={styles.mono}>{run.Ticker}</td>
                    <td>{run.Strategy}</td>
                    <td className={styles.dimText}>{run.ModelProvider}/{run.ModelName}</td>
                    <td className={run.TotalReturnPct != null && run.TotalReturnPct >= 0 ? styles.emerald : styles.red}>
                      {fmtPct(run.TotalReturnPct)}
                    </td>
                    <td>{fmtPct(run.WinRatePct)}</td>
                    <td className={styles.red}>{fmtDrawdown(run.MaxDrawdownPct)}</td>
                    <td>{fmtSharpe(run.SharpeRatio)}</td>
                    <td>{run.TotalTrades}</td>
                    <td className={styles.dimText}>{fmtDate(run.CreatedAt)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          {runs.length === 0 && (
            <div className={styles.emptyState}>
              <div className={styles.emptyTitle}>NO BACKTEST RUNS YET</div>
              <div className={styles.emptySub}>
                Backtests will appear here once runs are completed
              </div>
            </div>
          )}
        </>
      )}

      {selected && (
        <BacktestDetailPanel run={selected} onClose={handleClose} />
      )}
    </div>
  );
}
