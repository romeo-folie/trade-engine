/**
 * Portfolio screen — live broker-direct equity, KPI cards, overlay chart,
 * positions table, performance metrics.
 *
 * Data sources (Phase 18):
 *   GET /api/node/portfolio      — account snapshot (broker-first, 5s refresh)
 *   GET /api/portfolio/history   — dual equity series (Alpaca + TradeEngine)
 *   GET /api/portfolio/positions — positions (broker-first)
 *   GET /api/portfolio/summaries — 7-day period performance
 *   GET /api/portfolio/stats     — aggregate win-rate, PF, etc.
 */
import { useState, useMemo } from 'react';
import useSWR from 'swr';
import { jsonFetcher } from '../hooks/swr';
import { useEngineSocketContext } from '../contexts/EngineSocketContext';
import { TOKENS } from '../styles/tokens';
import KpiCard from '../components/portfolio/KpiCard';
import OverlayChart, { ChartMode } from '../components/portfolio/OverlayChart';
import PositionsTable from '../components/portfolio/PositionsTable';
import DailyCountersCard from '../components/portfolio/DailyCountersCard';
import ErrorBoundary from '../components/ErrorBoundary';
import styles from './Portfolio.module.css';

/* ── API response types ─────────────────────────────────────────────────────── */

interface PortfolioSnapshot {
  TotalValue: number;
  Cash: number;
  Invested: number;
  UnrealizedPnL: number;
  DayPnL: number;
  BuyingPower?: number;
  OpenPositions: number;
  source: 'broker' | 'tracker' | 'db';
  stale_at: string | null;
}

interface EquityPoint {
  t: string;
  v: number;
}

interface HistoryResponse {
  alpaca: EquityPoint[] | null;
  trade_engine: EquityPoint[] | null;
  source: string;
}

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

function formatStaleTime(iso: string): string {
  try {
    return new Date(iso).toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit', hour12: false });
  } catch {
    return iso;
  }
}

/* ── Source badge ───────────────────────────────────────────────────────────── */

function SourceBadge({ source, staleAt }: { source?: string; staleAt?: string | null }) {
  if (!source) return null;
  const isLive = source === 'broker' || source === 'tracker';
  const label  = isLive ? 'LIVE' : `STALE ${staleAt ? formatStaleTime(staleAt) : ''}`.trim();
  const color  = isLive ? TOKENS.acidGreen : TOKENS.amber;
  return (
    <span style={{
      fontFamily: 'var(--font-mono)',
      fontSize: 10,
      color,
      border: `1px solid ${color}`,
      borderRadius: 3,
      padding: '1px 6px',
      letterSpacing: '0.08em',
    }}>
      {label}
    </span>
  );
}

/* ── Component ──────────────────────────────────────────────────────────────── */

type ModeFilter = 'all' | 'live' | 'paper';
type PeriodKey = '1M' | '3M' | '1A' | 'all';
const CHART_MODES: { key: ChartMode; label: string }[] = [
  { key: 'overlay', label: 'OVERLAY' },
  { key: 'alpaca',  label: 'ALPACA'  },
  { key: 'te',      label: 'TRADEENGINE' },
];
const MODE_FILTERS: { key: ModeFilter; label: string }[] = [
  { key: 'all',   label: 'ALL'   },
  { key: 'live',  label: 'LIVE'  },
  { key: 'paper', label: 'PAPER' },
];
// Keys follow Alpaca portfolio/history period conventions ("1A" = 1 year,
// "all" = full account history) — passed straight through to the backend.
const PERIODS: { key: PeriodKey; label: string }[] = [
  { key: '1M',  label: '1M'  },
  { key: '3M',  label: '3M'  },
  { key: '1A',  label: '1Y'  },
  { key: 'all', label: 'ALL' },
];

export default function PortfolioScreen() {
  const [chartMode, setChartMode]   = useState<ChartMode>('overlay');
  const [modeFilter, setModeFilter] = useState<ModeFilter>('all');
  const [period, setPeriod]         = useState<PeriodKey>('1M');

  // Snapshot now comes from the WebSocket portfolio stream (PR-C of the perf
  // cleanup). useEngineSocket already does a one-time REST seed at mount, so
  // the card shows fresh values even before the first push event arrives —
  // no need to poll /api/node/portfolio in parallel.
  const { portfolioState } = useEngineSocketContext();
  const snapshot: PortfolioSnapshot | null = portfolioState ? {
    TotalValue:    portfolioState.totalValue,
    Cash:          portfolioState.cash,
    Invested:      portfolioState.invested,
    UnrealizedPnL: portfolioState.unrealizedPnL,
    DayPnL:        portfolioState.realizedPnlToday,
    OpenPositions: portfolioState.openPositions,
    source:        'tracker',
    stale_at:      null,
  } : null;

  // Equity history depends on modeFilter — SWR keys on the URL, so changing
  // the filter automatically fetches a new key + caches both. 5s refresh
  // matches the prior cadence; SWR pauses it while the tab is hidden.
  const histUrl = `/api/portfolio/history?period=${period}&timeframe=1D${modeFilter !== 'all' ? `&mode=${modeFilter}` : ''}`;
  const swrOpts = { refreshInterval: 5_000 } as const;
  const { data: history = null,    error: historyError }   = useSWR<HistoryResponse>(histUrl, jsonFetcher, swrOpts);
  const { data: summariesData,     error: summariesError } = useSWR<{ summaries?: DailySummary[] }>('/api/portfolio/summaries?limit=7', jsonFetcher, swrOpts);
  const summaries = useMemo(() => summariesData?.summaries ?? [], [summariesData]);
  const { data: stats = null,      error: statsError }     = useSWR<PortfolioStats>('/api/portfolio/stats', jsonFetcher, swrOpts);
  const { data: positionsData,     error: positionsError } = useSWR<{ positions?: any[] }>('/api/portfolio/positions', jsonFetcher, swrOpts);
  const positions = useMemo(() => positionsData?.positions ?? [], [positionsData]);
  // Surface ANY failing endpoint so the alert banner + chart placeholder
  // reflect a real error state (not just positions). Mirrors the prior
  // single-fetchAll behavior where any thrown error set fetchError.
  const fetchError = [historyError, summariesError, statsError, positionsError]
    .find((e): e is Error => e instanceof Error)?.message ?? null;

  const hasData    = snapshot !== null;
  const totalValue = snapshot?.TotalValue ?? 0;
  const cash       = snapshot?.Cash ?? 0;
  const invested   = snapshot?.Invested ?? 0;
  const unrealizedPnl = snapshot?.UnrealizedPnL ?? 0;
  const dayPnl     = snapshot?.DayPnL ?? 0;

  const alpacaSeries = useMemo(() => history?.alpaca ?? [], [history]);
  const teSeries     = useMemo(() => {
    if (history?.trade_engine?.length) return history.trade_engine;
    // Fallback: build from DB summaries when history endpoint has no TE series
    if (summaries.length > 1) {
      return [...summaries].reverse().map(s => ({
        t: s.Date.split('T')[0],
        v: s.EndingValue,
      }));
    }
    return [];
  }, [history, summaries]);

  return (
    <div className={styles.screen}>
      <div className={styles.headerRow}>
        <div className={styles.breadcrumb}>
          <span className={styles.breadcrumbBrand}>~/</span>
          <span className={styles.breadcrumbPage}>PORTFOLIO</span>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <SourceBadge source={snapshot?.source} staleAt={snapshot?.stale_at} />
          <div className={styles.autoRefresh}>
            <span className={styles.greenDot} />
            AUTO-REFRESH 5s
          </div>
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

      {/* ── Equity Chart ──────────────────────────────────────────────────── */}
      <div className={styles.curveSection}>
        {/* Chart controls: source tabs + mode filter */}
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 8 }}>
          {/* Source tabs */}
          <div style={{ display: 'flex', gap: 2 }}>
            {CHART_MODES.map(({ key, label }) => (
              <button
                key={key}
                onClick={() => setChartMode(key)}
                style={{
                  fontFamily: 'var(--font-mono)',
                  fontSize: 10,
                  letterSpacing: '0.08em',
                  padding: '3px 8px',
                  border: `1px solid ${chartMode === key ? TOKENS.acidGreen : TOKENS.border}`,
                  borderRadius: 3,
                  background: chartMode === key ? TOKENS.accentGlow : 'transparent',
                  color: chartMode === key ? TOKENS.acidGreen : TOKENS.textFaint,
                  cursor: 'pointer',
                }}
              >
                {label}
              </button>
            ))}
          </div>
          {/* Right-hand controls: period range + mode filter */}
          <div style={{ display: 'flex', gap: 12 }}>
            {/* Period range */}
            <div style={{ display: 'flex', gap: 2 }}>
              {PERIODS.map(({ key, label }) => (
                <button
                  key={key}
                  onClick={() => setPeriod(key)}
                  style={{
                    fontFamily: 'var(--font-mono)',
                    fontSize: 10,
                    letterSpacing: '0.08em',
                    padding: '3px 8px',
                    border: `1px solid ${period === key ? TOKENS.indigo : TOKENS.border}`,
                    borderRadius: 3,
                    background: period === key ? 'rgba(99,102,241,0.1)' : 'transparent',
                    color: period === key ? TOKENS.indigo : TOKENS.textFaint,
                    cursor: 'pointer',
                  }}
                >
                  {label}
                </button>
              ))}
            </div>
            {/* Mode filter */}
            <div style={{ display: 'flex', gap: 2 }}>
              {MODE_FILTERS.map(({ key, label }) => (
                <button
                  key={key}
                  onClick={() => setModeFilter(key)}
                  style={{
                    fontFamily: 'var(--font-mono)',
                    fontSize: 10,
                    letterSpacing: '0.08em',
                    padding: '3px 8px',
                    border: `1px solid ${modeFilter === key ? TOKENS.amber : TOKENS.border}`,
                    borderRadius: 3,
                    background: modeFilter === key ? 'rgba(245,158,11,0.1)' : 'transparent',
                    color: modeFilter === key ? TOKENS.amber : TOKENS.textFaint,
                    cursor: 'pointer',
                  }}
                >
                  {label}
                </button>
              ))}
            </div>
          </div>
        </div>

        {hasData || alpacaSeries.length > 0 || teSeries.length > 0 ? (
          <ErrorBoundary fallback={<div className={styles.curvePlaceholder}>Chart unavailable</div>}>
            <OverlayChart
              alpaca={alpacaSeries}
              tradeEngine={teSeries}
              mode={chartMode}
            />
          </ErrorBoundary>
        ) : (
          <div className={styles.curvePlaceholder}>
            {fetchError ? 'Unable to load equity data' : 'Loading equity data…'}
          </div>
        )}
      </div>

      {/* ── Today's signal pipeline (Issue #325) ───────────────────────── */}
      <div style={{ marginBottom: 'var(--space-6)' }}>
        <DailyCountersCard />
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
