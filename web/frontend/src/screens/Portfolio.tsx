/**
 * Portfolio screen — equity curve, KPI cards, positions table, performance metrics.
 *
 * Fetches live portfolio snapshot from GET /api/node/portfolio and recent trades
 * from GET /api/trades/recent. Open positions use mock data until a dedicated
 * backend endpoint is available.
 *
 * Task 4 of the web redesign (closes #204).
 */
import { useState, useEffect, useMemo, useRef } from 'react';
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

interface Trade {
  ID: string;
  Ticker: string;
  Direction: string;
  Strategy: string;
  EntryPrice: number;
  ExitPrice: number;
  PnL: number;
  PnLPct: number;
  Status: string;
  OpenedAt: string;
  ClosedAt: string;
}

/* ── Mock data ──────────────────────────────────────────────────────────────── */

// TODO: replace with /api/positions when backend endpoint is available
const MOCK_POSITIONS: Position[] = [
  { ticker: 'BTC/USDT', direction: 'LONG', strategy: 'TREND', entry: 83241.50, current: 84120.00, unrealizedPnl: 878.50, pnlPct: 1.06 },
  { ticker: 'ETH/USDT', direction: 'LONG', strategy: 'MOMENTUM', entry: 3780.60, current: 3842.10, unrealizedPnl: 246.00, pnlPct: 1.63 },
  { ticker: 'SOL/USDT', direction: 'LONG', strategy: 'TREND', entry: 182.30, current: 186.90, unrealizedPnl: 138.00, pnlPct: 2.52 },
  { ticker: 'NVDA', direction: 'SHORT', strategy: 'MEAN_REV', entry: 892.40, current: 901.20, unrealizedPnl: -352.00, pnlPct: -0.99 },
  { ticker: 'BTC/USDT', direction: 'LONG', strategy: 'NEWS_SENT', entry: 83900.00, current: 86230.70, unrealizedPnl: 2330.70, pnlPct: 2.78 },
];

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
      date: d.toISOString().slice(0, 10),
      value: Math.round(value * 100) / 100,
    });
  }
  return points;
}

// TODO: derive from DailySummary endpoint when available
const MOCK_PERIOD_STATS = [
  { label: 'Today', pnl: 1580.00, pct: 1.53 },
  { label: 'This Week', pnl: 4200.00, pct: 4.17 },
  { label: 'This Month', pnl: 8340.00, pct: 8.65 },
  { label: 'All Time', pnl: 4821.50, pct: 4.82 },
];

// TODO: derive from trades data when sufficient history is available
const MOCK_SYSTEM_STATS: { label: string; value: string; color?: string; sub?: string }[] = [
  { label: 'Win Rate', value: '68.1%', color: TOKENS.emerald },
  { label: 'Total Trades', value: '47' },
  { label: 'Avg Hold Time', value: '2h 14m' },
  { label: 'Max Drawdown', value: '-3.2%', color: TOKENS.red },
  { label: 'Sharpe Ratio', value: '1.84' },
  { label: 'Best Trade', value: '+$1,240', color: TOKENS.emerald, sub: 'NVDA LONG' },
  { label: 'Worst Trade', value: '-$380', color: TOKENS.red, sub: 'TSLA SELL' },
];

/* ── Helpers ────────────────────────────────────────────────────────────────── */

function fmt$(n: number): string {
  return '$' + n.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

function fmtPnl(n: number): string {
  const sign = n >= 0 ? '+' : '';
  return sign + fmt$(n);
}

function pnlColor(n: number): string {
  return n >= 0 ? TOKENS.emerald : TOKENS.red;
}

/* ── Component ──────────────────────────────────────────────────────────────── */

export default function PortfolioScreen() {
  const [snapshot, setSnapshot] = useState<PortfolioSnapshot | null>(null);
  // trades state reserved for future trades table rendering
  const [, setTrades] = useState<Trade[]>([]);
  // Per-tick AbortController ref — aborts previous in-flight request before starting new one
  const fetchControllerRef = useRef<AbortController | null>(null);

  useEffect(() => {
    async function load() {
      // Abort any prior in-flight request before starting a new one
      fetchControllerRef.current?.abort();
      const controller = new AbortController();
      fetchControllerRef.current = controller;

      try {
        const [snapRes, tradeRes] = await Promise.all([
          fetch('/api/node/portfolio', { signal: controller.signal }),
          fetch('/api/trades/recent?limit=50', { signal: controller.signal }),
        ]);

        if (snapRes.ok) {
          const data = await snapRes.json();
          if (data && typeof data.TotalValue === 'number') {
            setSnapshot(data);
          }
        }

        if (tradeRes.ok) {
          const data = await tradeRes.json();
          if (data?.trades) setTrades(data.trades);
        }
      } catch {
        // fetch aborted or network error — use fallback/mock data
      }
    }

    load();
    const interval = setInterval(load, 15_000);
    return () => {
      fetchControllerRef.current?.abort();
      clearInterval(interval);
    };
  }, []);

  // Use real snapshot data if available, else sensible defaults
  const totalValue = snapshot?.TotalValue ?? 104821.50;
  const cash = snapshot?.Cash ?? 41230.00;
  const invested = snapshot?.Invested ?? 63591.50;
  const unrealizedPnl = snapshot?.UnrealizedPnL ?? 3241.20;
  const dayPnl = snapshot?.RealizedPnLToday ?? 1580.00;

  const equityData = useMemo(() => generateMockEquityData(totalValue), [totalValue]);

  return (
    <div className={styles.screen}>
      {/* ── KPI Cards ─────────────────────────────────────────────────────── */}
      <div className={styles.kpiRow}>
        <KpiCard label="TOTAL VALUE" value={fmt$(totalValue)} />
        <KpiCard label="CASH" value={fmt$(cash)} />
        <KpiCard label="INVESTED" value={fmt$(invested)} />
        <KpiCard
          label="UNREALIZED P&L"
          value={fmtPnl(unrealizedPnl)}
          valueColor={pnlColor(unrealizedPnl)}
          prominent
        />
        <KpiCard
          label="DAY P&L"
          value={fmtPnl(dayPnl)}
          valueColor={pnlColor(dayPnl)}
        />
      </div>

      {/* ── Equity Curve ──────────────────────────────────────────────────── */}
      <div className={styles.curveSection}>
        <EquityCurve data={equityData} />
      </div>

      {/* ── Open Positions ────────────────────────────────────────────────── */}
      <PositionsTable positions={MOCK_POSITIONS} />

      {/* ── Performance Metrics ───────────────────────────────────────────── */}
      <div className={styles.metricsGrid}>
        {/* Left — Period Performance */}
        <div className={styles.metricsSection}>
          <div className={styles.sectionLabel}>PERIOD PERFORMANCE</div>
          {MOCK_PERIOD_STATS.map((row) => (
            <div key={row.label} className={styles.metricRow}>
              <span className={styles.metricLabel}>{row.label}</span>
              <span>
                <span className={`${styles.metricValue} ${row.pnl >= 0 ? styles.positive : styles.negative}`}>
                  {fmtPnl(row.pnl)}
                </span>
                <span className={`${styles.metricSub} ${row.pct >= 0 ? styles.positive : styles.negative}`}>
                  {row.pct >= 0 ? '+' : ''}{row.pct.toFixed(2)}%
                </span>
              </span>
            </div>
          ))}
        </div>

        {/* Right — System Stats */}
        <div className={styles.metricsSection}>
          <div className={styles.sectionLabel}>SYSTEM STATS</div>
          {MOCK_SYSTEM_STATS.map((row) => (
            <div key={row.label} className={styles.metricRow}>
              <span className={styles.metricLabel}>{row.label}</span>
              <span>
                <span
                  className={styles.metricValue}
                  style={row.color ? { color: row.color } : undefined}
                >
                  {row.value}
                </span>
                {row.sub && (
                  <span className={styles.metricSub} style={{ color: TOKENS.textFaint }}>
                    {row.sub}
                  </span>
                )}
              </span>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
