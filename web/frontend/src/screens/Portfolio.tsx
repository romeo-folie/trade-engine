/**
 * Portfolio screen — equity curve, KPI cards, positions table, performance metrics.
 *
 * Fetches live portfolio snapshot from GET /api/node/portfolio every 15s.
 * Trades fetch (GET /api/trades/recent) is intentionally deferred — will be
 * added when the trades table is implemented in a future task.
 * Open positions use mock data until a dedicated endpoint.
 *
 * Task 4 of the web redesign (closes #204).
 */
import { useState, useEffect, useRef } from 'react';
import { TOKENS } from '../styles/tokens';
import KpiCard from '../components/portfolio/KpiCard';
import EquityCurve from '../components/portfolio/EquityCurve';
import PositionsTable from '../components/portfolio/PositionsTable';
import type { Position } from '../components/portfolio/PositionsTable';
import styles from './Portfolio.module.css';

/* ── API response types ─────────────────────────────────────────────────────── */

interface PortfolioSnapshot {
  ID: string;
  TotalValue: number;
  Cash: number;
  Invested: number;
  UnrealizedPnL: number;
  RealizedPnLToday: number;
  OpenPositions: number;
  CreatedAt: string;
}

interface HistoryPoint {
  date: string;
  value: number;
}

interface PerformancePeriod {
  pnl: number;
  pnl_pct: number;
}

interface PerformanceData {
  current_value: number;
  today?: PerformancePeriod;
  week?: PerformancePeriod;
  month?: PerformancePeriod;
}

interface PortfolioStats {
  total_trades: number;
  win_rate: number;
  wins: number;
  losses: number;
  total_pnl: number;
}

/* ── Helpers ────────────────────────────────────────────────────────────────── */

function fmt$(n: number): string {
  return '$' + n.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

function fmtPnl(n: number): string {
  const sign = n > 0 ? '+' : n < 0 ? '-' : '';
  return `${sign}$${Math.abs(n).toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}

function fmtPct(n: number): string {
  const sign = n > 0 ? '+' : '';
  return `${sign}${n.toFixed(2)}%`;
}

function pnlColor(n: number): string {
  return n >= 0 ? TOKENS.emerald : TOKENS.red;
}

/* ── Component ──────────────────────────────────────────────────────────────── */

export default function PortfolioScreen() {
  const [snapshot, setSnapshot] = useState<PortfolioSnapshot | null>(null);
  const [history, setHistory] = useState<HistoryPoint[]>([]);
  const [performance, setPerformance] = useState<PerformanceData | null>(null);
  const [stats, setStats] = useState<PortfolioStats | null>(null);
  const [positions, setPositions] = useState<Position[]>([]);
  const [fetchError, setFetchError] = useState<string | null>(null);

  const fetchControllerRef = useRef<AbortController | null>(null);

  useEffect(() => {
    async function load() {
      fetchControllerRef.current?.abort();
      const controller = new AbortController();
      fetchControllerRef.current = controller;

      try {
        const [portRes, histRes, perfRes, statsRes, riskRes] = await Promise.all([
          fetch('/api/node/portfolio', { signal: controller.signal }),
          fetch('/api/portfolio/history?days=30', { signal: controller.signal }),
          fetch('/api/portfolio/performance', { signal: controller.signal }),
          fetch('/api/portfolio/stats', { signal: controller.signal }),
          fetch('/api/node/risk', { signal: controller.signal }),
        ]);

        if (portRes.ok) {
          const data = await portRes.json();
          if (data && typeof data.TotalValue === 'number') setSnapshot(data);
        }
        if (histRes.ok) {
          const data = await histRes.json();
          if (data && Array.isArray(data.history)) setHistory(data.history);
        }
        if (perfRes.ok) {
          const data = await perfRes.json();
          setPerformance(data);
        }
        if (statsRes.ok) {
          const data = await statsRes.json();
          setStats(data);
        }
        if (riskRes.ok) {
          const data = await riskRes.json();
          if (data && Array.isArray(data.open_trades)) {
            // Map backend trades to frontend positions
            const mapped = data.open_trades.map((t: any) => ({
              ticker: t.Ticker,
              direction: t.Direction.toUpperCase(),
              strategy: t.Strategy,
              entry: t.EntryPrice,
              current: t.EntryPrice + (t.PnL / t.Quantity), // Simplified: current estimate if not in payload
              unrealizedPnl: t.PnL,
              pnlPct: t.PnLPct,
            }));
            setPositions(mapped);
          }
        }
        setFetchError(null);
      } catch (err) {
        if (err instanceof Error && err.name !== 'AbortError') {
          console.warn('[PortfolioScreen] fetch error:', err);
          setFetchError(err.message);
        }
      }
    }

    load();
    const interval = setInterval(load, 15_000);
    return () => {
      fetchControllerRef.current?.abort();
      clearInterval(interval);
    };
  }, []);

  const hasData = snapshot !== null;
  const totalValue = snapshot?.TotalValue ?? 0;
  const cash = snapshot?.Cash ?? 0;
  const invested = snapshot?.Invested ?? 0;
  const unrealizedPnl = snapshot?.UnrealizedPnL ?? 0;
  const dayPnl = snapshot?.RealizedPnLToday ?? 0;

  return (
    <div className={styles.screen}>
      <div className={styles.headerRow}>
        <div className={styles.breadcrumb}>
          <span className={styles.breadcrumbBrand}>~/</span>
          <span className={styles.breadcrumbPage}>PORTFOLIO</span>
        </div>
        <div className={styles.autoRefresh}>
          <span className={styles.greenDot} />
          AUTO-REFRESH 15s
        </div>
      </div>

      <h1 className={styles.pageTitle}>PORTFOLIO</h1>

      {/* ── KPI Cards ─────────────────────────────────────────────────────── */}
      {fetchError && (
        <div className={styles.fetchErrorBanner} role="alert">
          {hasData ? '⚠ Data may be stale' : '⚠ Failed to load portfolio data'}
        </div>
      )}
      <div className={styles.kpiRow}>
        <KpiCard label="TOTAL VALUE" value={hasData ? fmt$(totalValue) : '—'} valueColor="var(--text-primary)" />
        <KpiCard label="CASH" value={hasData ? fmt$(cash) : '—'} valueColor="var(--text-primary)" />
        <KpiCard label="INVESTED" value={hasData ? fmt$(invested) : '—'} valueColor="var(--text-primary)" />
        <KpiCard
          label="UNREALIZED P&L"
          value={hasData ? fmtPnl(unrealizedPnl) : '—'}
          valueColor={hasData ? pnlColor(unrealizedPnl) : undefined}
          prominent
        />
        <KpiCard
          label="DAY P&L"
          value={hasData ? fmtPnl(dayPnl) : '—'}
          valueColor={hasData ? pnlColor(dayPnl) : undefined}
        />
      </div>

      {/* ── Equity Curve ──────────────────────────────────────────────────── */}
      <div className={styles.curveSection}>
        {history.length > 0 ? (
          <EquityCurve data={history} />
        ) : (
          <div className={styles.curvePlaceholder}>
            {fetchError ? 'Unable to load equity data' : 'Loading equity data…'}
          </div>
        )}
      </div>

      {/* ── Open Positions ────────────────────────────────────────────────── */}
      <PositionsTable positions={positions} />

      {/* ── Performance Metrics ───────────────────────────────────────────── */}
      <div className={styles.metricsGrid}>
        <div className={styles.metricsSection}>
          <div className={styles.sectionLabel}>PERIOD PERFORMANCE</div>
          {performance ? (
            <>
              <div className={styles.metricRow}>
                <span>TODAY</span>
                <span className={performance.today ? pnlColor(performance.today.pnl) : undefined}>
                  {performance.today ? `${fmtPnl(performance.today.pnl)} (${fmtPct(performance.today.pnl_pct)})` : '—'}
                </span>
              </div>
              <div className={styles.metricRow}>
                <span>7D</span>
                <span className={performance.week ? pnlColor(performance.week.pnl) : undefined}>
                  {performance.week ? `${fmtPnl(performance.week.pnl)} (${fmtPct(performance.week.pnl_pct)})` : '—'}
                </span>
              </div>
              <div className={styles.metricRow}>
                <span>30D</span>
                <span className={performance.month ? pnlColor(performance.month.pnl) : undefined}>
                  {performance.month ? `${fmtPnl(performance.month.pnl)} (${fmtPct(performance.month.pnl_pct)})` : '—'}
                </span>
              </div>
            </>
          ) : (
            <div className={styles.metricRow} style={{ color: 'var(--text-faint)' }}>— awaiting live data</div>
          )}
        </div>
        <div className={styles.metricsSection}>
          <div className={styles.sectionLabel}>SYSTEM STATS</div>
          {stats ? (
            <>
              <div className={styles.metricRow}>
                <span>TOTAL TRADES</span>
                <span style={{ color: 'var(--text-primary)' }}>{stats.total_trades}</span>
              </div>
              <div className={styles.metricRow}>
                <span>WIN RATE</span>
                <span className={stats.win_rate >= 50 ? styles.positive : styles.negative}>
                  {stats.win_rate.toFixed(1)}%
                </span>
              </div>
              <div className={styles.metricRow}>
                <span>TOTAL P&L</span>
                <span className={pnlColor(stats.total_pnl)}>{fmtPnl(stats.total_pnl)}</span>
              </div>
            </>
          ) : (
            <div className={styles.metricRow} style={{ color: 'var(--text-faint)' }}>— awaiting live data</div>
          )}
        </div>
      </div>
    </div>
  );
}
