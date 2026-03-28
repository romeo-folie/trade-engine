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
  RealizedPnLToday: number;
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

export default function PortfolioScreen() {
  const [snapshot, setSnapshot] = useState<PortfolioSnapshot | null>(null);
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
        const res = await fetch('/api/node/portfolio', { signal: controller.signal });
        if (res.ok) {
          const data = await res.json();
          if (data && typeof data.TotalValue === 'number') {
            setSnapshot(data);
            setFetchError(null);
          } else {
            console.warn('[PortfolioScreen] unexpected payload shape:', data);
            setFetchError('Unexpected response shape from /api/node/portfolio');
          }
        } else {
          console.warn('[PortfolioScreen] non-2xx response:', res.status, res.statusText);
          setFetchError(`API error: ${res.status} ${res.statusText}`);
        }
      } catch (err) {
        if (err instanceof Error && err.name !== 'AbortError') {
          console.warn('[PortfolioScreen] snapshot fetch error:', err);
          setFetchError(err.message);
        }
      }
      // Trades fetch deferred until trades table is rendered in a future task
    }

    load();
    const interval = setInterval(load, 15_000);
    return () => {
      fetchControllerRef.current?.abort();
      clearInterval(interval);
    };
  }, []);

  // Use real snapshot data only — no hardcoded fallbacks for financial values.
  // Display placeholder dashes until the first successful fetch.
  const hasData = snapshot !== null;
  const totalValue = snapshot?.TotalValue ?? 0;
  const cash = snapshot?.Cash ?? 0;
  const invested = snapshot?.Invested ?? 0;
  const unrealizedPnl = snapshot?.UnrealizedPnL ?? 0;
  const dayPnl = snapshot?.RealizedPnLToday ?? 0;

  const equityData = useMemo(
    () => (totalValue !== null ? generateMockEquityData(totalValue) : []),
    [totalValue],
  );

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
      <PositionsTable positions={[]} />

      {/* ── Performance Metrics ───────────────────────────────────────────── */}
      {/* Period and system stats require a dedicated summary endpoint.
          Placeholder displayed until live data is wired. */}
      <div className={styles.metricsGrid}>
        <div className={styles.metricsSection}>
          <div className={styles.sectionLabel}>PERIOD PERFORMANCE</div>
          <div className={styles.metricRow} style={{ color: 'var(--text-faint)' }}>
            — awaiting live data
          </div>
        </div>
        <div className={styles.metricsSection}>
          <div className={styles.sectionLabel}>SYSTEM STATS</div>
          <div className={styles.metricRow} style={{ color: 'var(--text-faint)' }}>
            — awaiting live data
          </div>
        </div>
      </div>
    </div>
  );
}
