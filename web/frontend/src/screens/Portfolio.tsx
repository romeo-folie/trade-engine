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
import { useState, useEffect, useMemo, useRef } from 'react';
import { TOKENS } from '../styles/tokens';
import KpiCard from '../components/portfolio/KpiCard';
import EquityCurve from '../components/portfolio/EquityCurve';
import PositionsTable from '../components/portfolio/PositionsTable';
import styles from './Portfolio.module.css';

/* ── API response types ─────────────────────────────────────────────────────── */

interface PortfolioSnapshot {
  ID: string;
  TotalValue: number;
  Cash: number;
  Invested: number;
  UnrealizedPnL: number;
  DayPnL: number;
  OpenPositions: number;
  CreatedAt: string;
}

// Trade interface deferred — will be added when trades table is rendered

/* ── Mock data ──────────────────────────────────────────────────────────────── */

// TODO: replace with /api/positions when backend endpoint is available

// TODO: replace with real equity history from /api/portfolio/history when available
function generateMockEquityData(currentValue: number): { date: string; value: number }[] {
  const points: { date: string; value: number }[] = [];
  const now = new Date();
  const startValue = currentValue * 0.95;
  for (let i = 16; i >= 0; i--) {
    const d = new Date(now);
    d.setDate(d.getDate() - i);
    const progress = (16 - i) / 16;
    const noise = (Math.sin(i * 1.7) * 0.015) + (Math.cos(i * 0.8) * 0.008);
    const value = startValue + (currentValue - startValue) * progress + currentValue * noise;
    points.push({
      date: `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`,
      value: Math.round(value * 100) / 100,
    });
  }
  return points;
}

// TODO: derive from DailySummary endpoint when available

// TODO: derive from trades data when sufficient history is available

/* ── Helpers ────────────────────────────────────────────────────────────────── */

function fmt$(n: number): string {
  return '$' + n.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

function fmtPnl(n: number): string {
  const sign = n > 0 ? '+' : n < 0 ? '-' : '';
  return `${sign}$${Math.abs(n).toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}

function pnlColor(n: number): string {
  return n >= 0 ? TOKENS.emerald : TOKENS.red;
}

/* ── Component ──────────────────────────────────────────────────────────────── */

interface DailySummary {
  ID: string;
  Date: string;
  StartingValue: number;
  EndingValue: number;
  PnL: number;
  PnLPct: number;
  TradesOpened: number;
  TradesClosed: number;
  Wins: number;
  Losses: number;
}

interface PortfolioStats {
  WinRatePct: number;
  ProfitFactor: number;
  TotalTrades: number;
  BestDayPnl: number;
  WorstDayPnl: number;
}

/* ── Component ──────────────────────────────────────────────────────────────── */

export default function PortfolioScreen() {
  const [snapshot, setSnapshot] = useState<PortfolioSnapshot | null>(null);
  const [summaries, setSummaries] = useState<DailySummary[]>([]);
  const [stats, setStats] = useState<PortfolioStats | null>(null);
  const [positions, setPositions] = useState<any[]>([]);
  const [fetchError, setFetchError] = useState<string | null>(null);
  // Per-tick AbortController ref — aborts previous in-flight request before starting new one
  const fetchControllerRef = useRef<AbortController | null>(null);

  useEffect(() => {
    async function load() {
      // Abort any prior in-flight request before starting a new one
      fetchControllerRef.current?.abort();
      const controller = new AbortController();
      fetchControllerRef.current = controller;

      try {
        // Fetch snapshot
        const resSn = await fetch('/api/node/portfolio', { signal: controller.signal });
        if (resSn.ok) {
          const data = await resSn.json();
          if (data && typeof data.TotalValue === 'number') {
            setSnapshot(data);
            setFetchError(null);
          }
        }

        // Fetch daily summaries (period performance)
        const resSum = await fetch('/api/portfolio/summaries?limit=7', { signal: controller.signal });
        if (resSum.ok) {
          const data = await resSum.json();
          setSummaries(data.summaries || []);
        }

        // Fetch stats
        const resStats = await fetch('/api/portfolio/stats', { signal: controller.signal });
        if (resStats.ok) {
          const statsData = await resStats.json();
          setStats(statsData);
        }

        // Fetch positions
        const resPos = await fetch('/api/portfolio/positions', { signal: controller.signal });
        if (resPos.ok) {
          const posData = await resPos.json();
          setPositions(posData.positions || []);
        }
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

  // Use real snapshot data only — no hardcoded fallbacks for financial values.
  const hasData = snapshot !== null;
  const totalValue = snapshot?.TotalValue ?? 0;
  const cash = snapshot?.Cash ?? 0;
  const invested = snapshot?.Invested ?? 0;
  const unrealizedPnl = snapshot?.UnrealizedPnL ?? 0;
  const dayPnl = snapshot?.DayPnL ?? 0;

  const equityData = useMemo(() => {
    if (!hasData) return [];
    // If we have real historical summaries, use them. Otherwise fallback to mock for the chart.
    if (summaries.length > 1) {
      return [...summaries].reverse().map(s => ({
        date: s.Date.split('T')[0],
        value: s.EndingValue
      }));
    }
    return generateMockEquityData(totalValue);
  }, [totalValue, summaries, hasData]);

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
        {hasData ? (
          <EquityCurve data={equityData} />
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
          <div className={styles.sectionLabel}>PERIOD PERFORMANCE (7D)</div>
          <div className={styles.metricList}>
            {summaries.length === 0 ? (
              <div className={styles.metricRow} style={{ color: 'var(--text-faint)' }}>— awaiting live data</div>
            ) : (
              summaries.map(s => (
                <div key={s.ID} className={styles.metricRow}>
                  <span className={styles.metricLabel}>{s.Date.split('T')[0]}</span>
                  <span className={styles.metricValue} style={{ color: pnlColor(s.PnL) }}>
                    {fmtPnl(s.PnL)} ({s.PnLPct.toFixed(2)}%)
                  </span>
                </div>
              ))
            )}
          </div>
        </div>
        <div className={styles.metricsSection}>
          <div className={styles.sectionLabel}>SYSTEM STATS</div>
          <div className={styles.metricList}>
            {!stats ? (
              <div className={styles.metricRow} style={{ color: 'var(--text-faint)' }}>— awaiting live data</div>
            ) : (
              <>
                <div className={styles.metricRow}>
                  <span className={styles.metricLabel}>WIN RATE</span>
                  <span className={styles.metricValue}>{stats.WinRatePct.toFixed(1)}%</span>
                </div>
                <div className={styles.metricRow}>
                  <span className={styles.metricLabel}>PROFIT FACTOR</span>
                  <span className={styles.metricValue}>{stats.ProfitFactor.toFixed(2)}</span>
                </div>
                <div className={styles.metricRow}>
                  <span className={styles.metricLabel}>TOTAL TRADES</span>
                  <span className={styles.metricValue}>{stats.TotalTrades}</span>
                </div>
                <div className={styles.metricRow}>
                  <span className={styles.metricLabel}>BEST DAY</span>
                  <span className={styles.metricValue} style={{ color: TOKENS.emerald }}>{fmtPnl(stats.BestDayPnl)}</span>
                </div>
                <div className={styles.metricRow}>
                  <span className={styles.metricLabel}>WORST DAY</span>
                  <span className={styles.metricValue} style={{ color: TOKENS.red }}>{fmtPnl(stats.WorstDayPnl)}</span>
                </div>
              </>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
