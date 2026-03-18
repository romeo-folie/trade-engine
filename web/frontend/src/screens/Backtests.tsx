/**
 * Backtests screen — table of backtest runs with empty state.
 *
 * Fetches from GET /api/backtests/recent?limit=50 on mount.
 * Currently returns empty array (backtests not yet implemented in engine).
 *
 * Task 8 of the web redesign (closes #206).
 */
import { useState, useEffect, useRef } from 'react';
import styles from './Backtests.module.css';

/* ── Types ─────────────────────────────────────────────────────────────────── */

export interface BacktestRun {
  ID: string;
  Strategy: string;
  Period: string;
  Trades: number;
  WinRate: number;
  NetPnL: number;
  Status: string;
}

/* ── Table columns ─────────────────────────────────────────────────────────── */

const COLUMNS = [
  'RUN ID',
  'STRATEGY',
  'PERIOD',
  'TRADES',
  'WIN RATE',
  'NET P&L',
  'STATUS',
];

/* ── Component ─────────────────────────────────────────────────────────────── */

export default function BacktestsScreen() {
  const [backtests, setBacktests] = useState<BacktestRun[]>([]);
  const [isLoading, setIsLoading] = useState(true);
  const [fetchError, setFetchError] = useState<string | null>(null);
  const fetchControllerRef = useRef<AbortController | null>(null);

  useEffect(() => {
    async function load() {
      fetchControllerRef.current?.abort();
      const controller = new AbortController();
      fetchControllerRef.current = controller;

      try {
        const res = await fetch('/api/backtests/recent?limit=50', { signal: controller.signal });
        if (!res.ok) {
          setFetchError(`${res.status} ${res.statusText}`);
          return;
        }
        const data = await res.json();
        if (data && Array.isArray(data.backtests)) {
          setBacktests(data.backtests);
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

    load();
    return () => { fetchControllerRef.current?.abort(); };
  }, []);

  return (
    <div className={styles.screen}>
      <div className={styles.sectionLabel}>BACKTESTS</div>

      {fetchError && (
        <div className={styles.fetchErrorBanner} role="alert">
          {backtests.length > 0 ? '⚠ Data may be stale' : '⚠ Failed to load backtests'}
        </div>
      )}

      {isLoading && backtests.length === 0 && (
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
                {backtests.map(bt => (
                  <tr key={bt.ID}>
                    <td>{bt.ID}</td>
                    <td>{bt.Strategy}</td>
                    <td>{bt.Period}</td>
                    <td>{bt.Trades}</td>
                    <td>{(bt.WinRate * 100).toFixed(1)}%</td>
                    <td>{bt.NetPnL >= 0 ? '+' : ''}{bt.NetPnL.toFixed(2)}%</td>
                    <td>{bt.Status}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          {backtests.length === 0 && (
            <div className={styles.emptyState}>
              <div className={styles.emptyTitle}>NO BACKTEST RUNS YET</div>
              <div className={styles.emptySub}>
                Backtests will appear here once runs are completed
              </div>
            </div>
          )}
        </>
      )}
    </div>
  );
}
