/**
 * Signals screen — filterable table of trading signals with detail panel.
 *
 * Fetches live signals from GET /api/signals/recent?limit=100 every 15s.
 * Ticker search + status pill tab filter. Row click opens SignalDetailPanel.
 *
 * Task 5 of the web redesign (closes #205).
 */
import { useState, useEffect, useMemo, useRef, useCallback } from 'react';
import DataTable from '../components/table/DataTable';
import FilterBar from '../components/table/FilterBar';
import SignalDetailPanel from '../components/signals/SignalDetailPanel';
import type { Signal } from '../components/signals/SignalDetailPanel';
import type { Column } from '../components/table/DataTable';
import styles from './Signals.module.css';

/* ── Helpers ───────────────────────────────────────────────────────────────── */

function fmtPrice(n: number): string {
  return '$' + n.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

function fmtDate(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  return d.toLocaleString('en-US', {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false,
  });
}

function statusBadgeClass(status: string): string {
  const s = status.toLowerCase();
  if (s === 'pending') return styles.badgePending;
  if (s === 'executed' || s === 'approved') return styles.badgeExecuted;
  return styles.badgeBlocked;
}

/* ── Status pill tabs ─────────────────────────────────────────────────────── */

const STATUS_TABS = ['', 'pending', 'approved', 'executed', 'blocked', 'expired'];
const STATUS_LABELS: Record<string, string> = {
  '': 'ALL', 'pending': 'PENDING', 'approved': 'APPROVED',
  'executed': 'EXECUTED', 'blocked': 'BLOCKED', 'expired': 'EXPIRED',
};

/* ── Columns ───────────────────────────────────────────────────────────────── */

const COLUMNS: Column<Signal>[] = [
  {
    key: 'Ticker',
    label: 'TICKER',
    sortable: true,
    render: (s) => <span style={{ fontWeight: 700 }}>{s.Ticker}</span>,
  },
  {
    key: 'Action',
    label: 'DIR',
    render: (s) => {
      const a = s.Action.toUpperCase();
      if (a === 'BUY') return <span className={styles.dirLong}>▲ LONG</span>;
      if (a === 'SELL') return <span className={styles.dirShort}>▼ SHORT</span>;
      return <span className={styles.dirNeut}>◆ NEUT</span>;
    },
  },
  {
    key: 'Strategy',
    label: 'STRATEGY',
    sortable: true,
    render: (s) => <span style={{ color: 'var(--text-muted)' }}>{s.Strategy}</span>,
  },
  {
    key: 'Status',
    label: 'STATUS',
    render: (s) => (
      <span className={`${styles.badge} ${statusBadgeClass(s.Status)} chamfer-sm`}>
        {s.Status.toUpperCase()}
      </span>
    ),
  },
  {
    key: 'Confidence',
    label: 'CONFIDENCE',
    sortable: true,
    render: (s) => `${(s.Confidence * 100).toFixed(0)}%`,
  },
  {
    key: 'SignalPrice',
    label: 'ENTRY PRICE',
    sortable: true,
    render: (s) => fmtPrice(s.SignalPrice),
  },
  {
    key: 'CreatedAt',
    label: 'CREATED',
    sortable: true,
    render: (s) => fmtDate(s.CreatedAt),
  },
];

/* ── Component ─────────────────────────────────────────────────────────────── */

export default function SignalsScreen() {
  const [signals, setSignals] = useState<Signal[]>([]);
  const [fetchError, setFetchError] = useState<string | null>(null);
  const [tickerSearch, setTickerSearch] = useState('');
  const [statusFilter, setStatusFilter] = useState('');
  const [selectedSignal, setSelectedSignal] = useState<Signal | null>(null);
  const fetchControllerRef = useRef<AbortController | null>(null);

  useEffect(() => {
    async function load() {
      fetchControllerRef.current?.abort();
      const controller = new AbortController();
      fetchControllerRef.current = controller;

      try {
        const res = await fetch('/api/signals/recent?limit=100', { signal: controller.signal });
        if (!res.ok) {
          setFetchError(`${res.status} ${res.statusText}`);
          return;
        }
        const data = await res.json();
        if (data && Array.isArray(data.signals)) {
          setSignals(data.signals);
          setFetchError(null);
        } else {
          setFetchError('Unexpected response shape');
        }
      } catch (err) {
        if (err instanceof Error && err.name !== 'AbortError') {
          console.warn('[SignalsScreen] fetch error:', err);
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

  const filtered = useMemo(() => {
    let rows = signals;
    if (tickerSearch) {
      const q = tickerSearch.toLowerCase();
      rows = rows.filter(s => s.Ticker.toLowerCase().includes(q));
    }
    if (statusFilter) {
      rows = rows.filter(s => s.Status.toLowerCase() === statusFilter);
    }
    return rows;
  }, [signals, tickerSearch, statusFilter]);

  const handleClosePanel = useCallback(() => setSelectedSignal(null), []);

  return (
    <div className={styles.screen}>
      <div className={styles.sectionLabel}>SIGNALS</div>

      {fetchError && (
        <div className={styles.fetchErrorBanner} role="alert">
          {signals.length > 0 ? '⚠ Data may be stale' : '⚠ Failed to load signals'}
        </div>
      )}

      <FilterBar
        searchValue={tickerSearch}
        onSearchChange={setTickerSearch}
        searchPlaceholder="Ticker…"
      />

      <div className={styles.filterPills} role="tablist" aria-label="Signal status filter">
        {STATUS_TABS.map(tab => (
          <button
            key={tab}
            type="button"
            role="tab"
            aria-selected={statusFilter === tab}
            className={`${styles.filterPill} ${statusFilter === tab ? styles.filterPillActive : ''} chamfer-sm`}
            onClick={() => setStatusFilter(tab)}
          >
            {STATUS_LABELS[tab]}
          </button>
        ))}
      </div>

      <DataTable<Signal>
        columns={COLUMNS}
        data={filtered}
        rowKey={(s) => s.ID}
        onRowClick={setSelectedSignal}
        defaultSortKey="CreatedAt"
        defaultSortDir="desc"
        ariaLabel="Trading signals"
      />

      {selectedSignal && (
        <SignalDetailPanel signal={selectedSignal} onClose={handleClosePanel} />
      )}
    </div>
  );
}
