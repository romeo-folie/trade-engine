/**
 * Trades screen — summary KPIs, filterable table, CSV clipboard export.
 *
 * Fetches live trades from GET /api/trades/recent?limit=200 every 30s.
 * Date filter (today/week/month/all). P&L coloring (emerald/red).
 *
 * Task 5 of the web redesign (closes #205).
 */
import { useState, useEffect, useMemo, useRef, useCallback } from 'react';
import DataTable from '../components/table/DataTable';
import FilterBar from '../components/table/FilterBar';
import type { Column } from '../components/table/DataTable';
import { fmtPrice, fmtDate } from '../utils/format';
import styles from './Trades.module.css';

/* ── Types ─────────────────────────────────────────────────────────────────── */

interface Trade {
  ID: string;
  SignalID: string;
  Ticker: string;
  Direction: string;
  Strategy: string;
  EntryPrice: number;
  ExitPrice: number;
  Quantity: number;
  PnL: number;
  PnLPct: number;
  Status: string;
  ModelProvider: string;
  ModelName: string;
  OpenedAt: string;
  ClosedAt: string;
}

/* ── Helpers ───────────────────────────────────────────────────────────────── */

function fmtPnl(n: number): string {
  const sign = n > 0 ? '+' : n < 0 ? '-' : '';
  return `${sign}$${Math.abs(n).toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}

function fmtPct(n: number): string {
  const sign = n > 0 ? '+' : '';
  return `${sign}${n.toFixed(2)}%`;
}

function pnlClass(n: number): string {
  if (n > 0) return styles.positive;
  if (n < 0) return styles.negative;
  return '';
}

function statusBadgeClass(status: string): string {
  const s = status.toLowerCase();
  if (s === 'open') return styles.badgeOpen;
  if (s === 'closed') return styles.badgeClosed;
  return styles.badgeCancelled;
}

/* ── Date filter helpers ───────────────────────────────────────────────────── */

const DATE_OPTIONS = [
  { label: 'All', value: '' },
  { label: 'Today', value: 'today' },
  { label: '7D', value: 'week' },
  { label: 'Month', value: 'month' },
];

function dateThreshold(period: string): Date | null {
  if (!period) return null;
  const now = new Date();
  if (period === 'today') {
    return new Date(now.getFullYear(), now.getMonth(), now.getDate());
  }
  if (period === 'week') {
    const d = new Date(now);
    d.setDate(d.getDate() - 7);
    return d;
  }
  if (period === 'month') {
    const d = new Date(now);
    d.setMonth(d.getMonth() - 1);
    return d;
  }
  return null;
}

/* ── Columns ───────────────────────────────────────────────────────────────── */

const COLUMNS: Column<Trade>[] = [
  {
    key: 'Ticker',
    label: 'TICKER',
    sortable: true,
    render: (t) => <span style={{ fontWeight: 700 }}>{t.Ticker}</span>,
  },
  {
    key: 'Direction',
    label: 'DIR',
    render: (t) => (
      <span className={t.Direction.toLowerCase() === 'buy' ? styles.dirBuy : styles.dirSell}>
        {t.Direction.toUpperCase()}
      </span>
    ),
  },
  {
    key: 'Strategy',
    label: 'STRATEGY',
    sortable: true,
    render: (t) => <span style={{ color: 'var(--text-muted)' }}>{t.Strategy}</span>,
  },
  {
    key: 'EntryPrice',
    label: 'ENTRY',
    sortable: true,
    render: (t) => <span className={styles.numericCell}>{fmtPrice(t.EntryPrice)}</span>,
  },
  {
    key: 'ExitPrice',
    label: 'EXIT',
    sortable: true,
    render: (t) => <span className={styles.numericCell}>{t.ExitPrice ? fmtPrice(t.ExitPrice) : '—'}</span>,
  },
  {
    key: 'PnL',
    label: 'P&L',
    sortable: true,
    render: (t) => <span className={`${styles.numericCell} ${pnlClass(t.PnL)}`}>{fmtPnl(t.PnL)}</span>,
  },
  {
    key: 'PnLPct',
    label: 'P&L %',
    sortable: true,
    render: (t) => <span className={`${styles.numericCell} ${pnlClass(t.PnLPct)}`}>{fmtPct(t.PnLPct)}</span>,
  },
  {
    key: 'ModelName',
    label: 'MODEL',
    render: (t) => <span style={{ color: 'var(--text-muted)' }}>{t.ModelName || '—'}</span>,
  },
  {
    key: 'Status',
    label: 'STATUS',
    render: (t) => (
      <span className={`${styles.badge} ${statusBadgeClass(t.Status)} chamfer-sm`}>
        {t.Status.toUpperCase()}
      </span>
    ),
  },
  {
    key: 'OpenedAt',
    label: 'OPENED',
    sortable: true,
    render: (t) => fmtDate(t.OpenedAt),
  },
];

/* ── CSV export ────────────────────────────────────────────────────────────── */

function tradesToCsv(trades: Trade[]): string {
  const escape = (v: string) => `"${v.replace(/"/g, '""')}"`;
  const header = 'ticker,direction,strategy,model,entry,exit,pnl,pnl_pct,status,opened';
  const rows = trades.map(t =>
    [
      escape(t.Ticker),
      escape(t.Direction),
      escape(t.Strategy),
      escape(t.ModelName || ''),
      t.EntryPrice.toFixed(2),
      t.ExitPrice ? t.ExitPrice.toFixed(2) : '',
      t.PnL.toFixed(2),
      t.PnLPct.toFixed(2),
      escape(t.Status),
      escape(t.OpenedAt),
    ].join(',')
  );
  return [header, ...rows].join('\n');
}

/* ── Component ─────────────────────────────────────────────────────────────── */

export default function TradesScreen() {
  const [trades, setTrades] = useState<Trade[]>([]);
  const [isLoading, setIsLoading] = useState(true);
  const [fetchError, setFetchError] = useState<string | null>(null);
  const [tickerSearch, setTickerSearch] = useState('');
  const [dateFilter, setDateFilter] = useState('');
  const [toastMsg, setToastMsg] = useState<string | null>(null);
  const fetchControllerRef = useRef<AbortController | null>(null);
  const toastTimer = useRef<ReturnType<typeof setTimeout>>();

  useEffect(() => {
    async function load() {
      fetchControllerRef.current?.abort();
      const controller = new AbortController();
      fetchControllerRef.current = controller;

      try {
        const res = await fetch('/api/trades/recent?limit=200', { signal: controller.signal });
        if (!res.ok) {
          setFetchError(`${res.status} ${res.statusText}`);
          return;
        }
        const data = await res.json();
        if (data && Array.isArray(data.trades)) {
          setTrades(data.trades);
          setFetchError(null);
        } else {
          setFetchError('Unexpected response shape');
        }
      } catch (err) {
        if (err instanceof Error && err.name !== 'AbortError') {
          console.warn('[TradesScreen] fetch error:', err);
          setFetchError(err.message);
        }
      } finally {
        setIsLoading(false);
      }
    }

    load();
    const interval = setInterval(load, 30_000);
    return () => {
      fetchControllerRef.current?.abort();
      clearInterval(interval);
    };
  }, []);

  useEffect(() => () => clearTimeout(toastTimer.current), []);

  const filtered = useMemo(() => {
    let rows = trades;
    if (tickerSearch) {
      const q = tickerSearch.toLowerCase();
      rows = rows.filter(t => t.Ticker.toLowerCase().includes(q));
    }
    const threshold = dateThreshold(dateFilter);
    if (threshold) {
      rows = rows.filter(t => new Date(t.OpenedAt) >= threshold);
    }
    return rows;
  }, [trades, tickerSearch, dateFilter]);

  // Summary stats computed from filtered set
  const summary = useMemo(() => {
    const total = filtered.length;
    const openCount = filtered.filter(t => t.Status.toLowerCase() === 'open').length;
    const closed = filtered.filter(t => t.Status.toLowerCase() === 'closed');
    const wins = closed.filter(t => t.PnL > 0).length;
    const winRate = closed.length > 0 ? (wins / closed.length) * 100 : 0;
    const avgPnl = closed.length > 0 ? closed.reduce((s, t) => s + t.PnL, 0) / closed.length : 0;
    return { total, openCount, winRate, avgPnl };
  }, [filtered]);

  const periodLabel = useMemo(() => {
    if (!dateFilter) return '';
    const match = DATE_OPTIONS.find(o => o.value === dateFilter);
    return match ? ` (${match.label.toUpperCase()})` : '';
  }, [dateFilter]);

  const handleExport = useCallback(() => {
    const csv = tradesToCsv(filtered);
    if (!navigator.clipboard) {
      setToastMsg('Clipboard not available (requires HTTPS)');
      toastTimer.current = setTimeout(() => setToastMsg(null), 2000);
      return;
    }
    navigator.clipboard.writeText(csv).then(() => {
      setToastMsg('CSV copied to clipboard');
      toastTimer.current = setTimeout(() => setToastMsg(null), 2000);
    }).catch(() => {
      setToastMsg('Failed to copy');
      toastTimer.current = setTimeout(() => setToastMsg(null), 2000);
    });
  }, [filtered]);

  return (
    <div className={styles.screen}>
      <div className={styles.sectionLabel}>TRADES</div>

      {fetchError && (
        <div className={styles.fetchErrorBanner} role="alert">
          {trades.length > 0 ? '⚠ Data may be stale' : '⚠ Failed to load trades'}
        </div>
      )}

      {isLoading && trades.length === 0 && (
        <div className={styles.loadingBanner} role="status" aria-label="Loading trades">
          Loading…
        </div>
      )}

      {/* Summary row */}
      <div className={styles.summaryRow}>
        <div className={styles.summaryCard}>
          <span className={styles.summaryLabel}>TOTAL{periodLabel}</span>
          <span className={styles.summaryValue}>{summary.total}</span>
        </div>
        <div className={styles.summaryCard}>
          <span className={styles.summaryLabel}>OPEN{periodLabel}</span>
          <span className={styles.summaryValue}>{summary.openCount}</span>
        </div>
        <div className={styles.summaryCard}>
          <span className={styles.summaryLabel}>WIN RATE{periodLabel}</span>
          <span className={`${styles.summaryValue} ${summary.winRate >= 50 ? styles.positive : styles.negative}`}>
            {summary.winRate.toFixed(1)}%
          </span>
        </div>
        <div className={styles.summaryCard}>
          <span className={styles.summaryLabel}>AVG P&amp;L{periodLabel}</span>
          <span className={`${styles.summaryValue} ${pnlClass(summary.avgPnl)}`}>
            {fmtPnl(summary.avgPnl)}
          </span>
        </div>
      </div>

      <FilterBar
        searchValue={tickerSearch}
        onSearchChange={setTickerSearch}
        searchPlaceholder="Ticker…"
        filters={[
          {
            label: 'PERIOD',
            value: dateFilter,
            onChange: setDateFilter,
            options: DATE_OPTIONS,
          },
        ]}
        actions={[
          { label: 'EXPORT CSV', onClick: handleExport },
        ]}
      />

      {!(isLoading && trades.length === 0) && (
        <DataTable<Trade>
          columns={COLUMNS}
          data={filtered}
          rowKey={(t) => t.ID}
          defaultSortKey="OpenedAt"
          defaultSortDir="desc"
          ariaLabel="Trade history"
        />
      )}

      {toastMsg && (
        <div className={`${styles.toast} chamfer-sm`} role="status">{toastMsg}</div>
      )}
    </div>
  );
}
