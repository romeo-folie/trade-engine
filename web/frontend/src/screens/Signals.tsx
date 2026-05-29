/**
 * Signals screen — filterable table of trading signals with detail panel.
 *
 * Fetches signals from GET /api/signals/recent with server-side ticker/status
 * filtering and cursor-based (keyset) pagination. Polls every 15s for new
 * signals on page 1; "Load more" appends older pages without resetting scroll.
 */
import { useState, useEffect, useRef, useCallback } from 'react';
import { useVisiblePoll } from '../hooks/useVisiblePoll';
import DataTable from '../components/table/DataTable';
import FilterBar from '../components/table/FilterBar';
import CornerCard from '../components/CornerCard';
import { ChamferedBox } from '../components/common/ChamferedBox';
import { colors } from '../styles/tokens';
import SignalDetailPanel from '../components/signals/SignalDetailPanel';
import type { Signal } from '../components/signals/SignalDetailPanel';
import type { Column } from '../components/table/DataTable';
import styles from './Signals.module.css';




const ANALYST_BAR_COLORS = [colors.analystTechnical, colors.analystMomentum, colors.analystSentiment, colors.analystMacro];

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
      if (a === 'BUY' || a === 'LONG') return <span className={styles.dirLong}>LONG</span>;
      if (a === 'SELL' || a === 'SHORT') return <span className={styles.dirShort}>SHORT</span>;
      return <span className={styles.dirNeut}>NEUTRAL</span>;
    },
  },
  {
    key: 'Strategy',
    label: 'STRATEGY',
    sortable: true,
    render: (s) => <span style={{ color: 'var(--text-muted)' }}>{s.Strategy}</span>,
  },
  {
    key: 'Confidence',
    label: 'CONFIDENCE',
    sortable: true,
    render: (s) => (
      <div className={styles.confidenceCell}>
        <span className={styles.confidenceValue}>{(s.Confidence * 100).toFixed(0)}%</span>
        <div className={styles.confidenceBar}>
          <div
            className={styles.confidenceFill}
            style={{ width: `${Math.round(s.Confidence * 100)}%` }}
          />
        </div>
      </div>
    ),
  },
  {
    key: 'Consensus',
    label: 'CONSENSUS',
    render: (s) => {
      if (!s.Analysts || s.Analysts.length === 0) {
        return <span style={{ color: colors.textFaint, fontSize: 9 }}>—</span>;
      }
      const active = s.Analysts.filter(a => a.active);
      return (
        <div className={styles.consensusGrid}>
          {active.slice(0, 3).map((a) => {
            const canonicalIdx = s.Analysts!.indexOf(a);
            const barColor = ANALYST_BAR_COLORS[canonicalIdx] ?? colors.borderMuted;
            return (
              <div key={a.name} className={styles.consensusRow}>
                <div className={styles.consensusBar} style={{ width: a.confidence * 40, background: barColor }} />
                <span className={styles.consensusLetter}>{a.name.charAt(0).toUpperCase()}</span>
              </div>
            );
          })}
        </div>
      );
    }
  },
  {
    key: 'Status',
    label: 'STATUS',
    render: (s) => {
      const status = s.Status.toLowerCase();
      let color = colors.borderMuted;
      let bg = 'transparent';
      let text = 'var(--text-primary)';

      if (status === 'pending') { color = colors.amber; bg = 'rgba(245, 158, 11, 0.1)'; text = colors.amber; }
      else if (status === 'approved') { color = colors.acidGreen; bg = 'transparent'; text = colors.acidGreen; }
      else if (status === 'executed') { color = colors.acidGreen; bg = colors.acidGreen; text = '#0D0D11'; }
      else if (status === 'blocked') { color = colors.red; bg = 'rgba(239, 68, 68, 0.15)'; text = colors.red; }
      else if (status === 'expired') { color = colors.textDim; bg = 'transparent'; text = 'var(--text-dim)'; }

      return (
        <ChamferedBox
          chamfer={4}
          centerContent
          backgroundColor={bg}
          borderColor={color}
          style={{ width: 80, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: '2px 0' }}
        >
          <span style={{ fontSize: 9, fontWeight: 700, color: text }}>
            {status.toUpperCase()}
          </span>
        </ChamferedBox>
      );
    },
  },
  {
    key: 'CreatedAt',
    label: 'TIME',
    sortable: true,
    render: (s) => {
      const d = new Date(s.CreatedAt);
      return <span style={{ color: 'var(--text-dim)' }}>{d.toLocaleTimeString('en-GB', { hour12: false })}</span>;
    }
  }
];

/* ── CSV export ────────────────────────────────────────────────────────────── */

function signalsToCsv(sigs: Signal[]): string {
  const escape = (v: string) => `"${v.replace(/"/g, '""')}"`;
  const header = 'ticker,action,strategy,confidence,size_pct,signal_price,stop_loss,take_profit,status,created_at';
  const rows = sigs.map(s =>
    [
      escape(s.Ticker),
      escape(s.Action),
      escape(s.Strategy),
      s.Confidence.toFixed(4),
      s.SizePct.toFixed(2),
      s.SignalPrice.toFixed(2),
      s.StopLoss.toFixed(2),
      s.TakeProfit.toFixed(2),
      escape(s.Status),
      escape(s.CreatedAt),
    ].join(',')
  );
  return [header, ...rows].join('\n');
}

/* ── URL builder ──────────────────────────────────────────────────────────── */

function buildSignalsUrl(ticker: string, status: string, before?: string | null): string {
  const p = new URLSearchParams({ limit: '50' });
  if (ticker) p.set('ticker', ticker);
  if (status) p.set('status', status);
  if (before) p.set('before', before);
  return `/api/signals/recent?${p}`;
}

/* ── Component ─────────────────────────────────────────────────────────────── */

export default function SignalsScreen() {
  const [signals, setSignals] = useState<Signal[]>([]);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [isLoadingMore, setIsLoadingMore] = useState(false);
  const [systemStatus, setSystemStatus] = useState<{ version: string; status: string } | null>(null);
  const [isLoading, setIsLoading] = useState(true);
  const [fetchError, setFetchError] = useState<string | null>(null);
  const [tickerSearch, setTickerSearch] = useState('');
  const [debouncedTicker, setDebouncedTicker] = useState('');
  const [statusFilter, setStatusFilter] = useState('');
  const [selectedSignal, setSelectedSignal] = useState<Signal | null>(null);
  const [toastMsg, setToastMsg] = useState<string | null>(null);
  const fetchControllerRef = useRef<AbortController | null>(null);
  const toastTimer = useRef<ReturnType<typeof setTimeout>>();
  const cursorInitialized = useRef(false);

  // Debounce ticker search — 300ms after last keystroke
  useEffect(() => {
    const t = setTimeout(() => setDebouncedTicker(tickerSearch), 300);
    return () => clearTimeout(t);
  }, [tickerSearch]);

  // Fetch system status once on mount
  useEffect(() => {
    fetch('/api/system/status')
      .then(r => (r.ok ? r.json() : null))
      .then(data => {
        if (data) setSystemStatus({ version: data.version || '0.0.0', status: 'ALL SYSTEMS NOMINAL' });
      })
      .catch(() => {});
  }, []);

  // fetchPage1 closes over the current filters; identity changes when filters
  // change so the reset-and-load effect below re-fires. useVisiblePoll captures
  // the latest fetchPage1 in a ref, so polling always uses current filters
  // without tearing the interval down on every filter change.
  const fetchPage1 = useCallback(async () => {
    fetchControllerRef.current?.abort();
    const controller = new AbortController();
    fetchControllerRef.current = controller;

    try {
      const res = await fetch(buildSignalsUrl(debouncedTicker, statusFilter), { signal: controller.signal });
      if (!res.ok) {
        setFetchError(`${res.status} ${res.statusText}`);
        return;
      }
      const data = await res.json();
      const fresh: Signal[] = data.signals ?? [];

      if (!cursorInitialized.current) {
        // First load for this filter combination — set cursor and replace state
        setNextCursor(data.next_cursor ?? null);
        cursorInitialized.current = true;
        setSignals(fresh);
      } else {
        // Poll merge: prepend new signals, update existing ones in-place
        setSignals(prev => {
          const freshById = new Map(fresh.map((s: Signal) => [s.ID, s]));
          const updated = prev.map(s => freshById.get(s.ID) ?? s);
          const brandNew = fresh.filter(s => !prev.some(e => e.ID === s.ID));
          return [...brandNew, ...updated];
        });
      }
      setFetchError(null);
    } catch (err) {
      if (err instanceof Error && err.name !== 'AbortError') {
        console.warn('[SignalsScreen] fetch error:', err);
        setFetchError(err.message);
      }
    } finally {
      setIsLoading(false);
    }
  }, [debouncedTicker, statusFilter]);

  // Reset + load when filters change (fetchPage1 identity tracks them).
  useEffect(() => {
    setSignals([]);
    setNextCursor(null);
    setIsLoading(true);
    cursorInitialized.current = false;
    fetchPage1();
    return () => {
      fetchControllerRef.current?.abort();
    };
  }, [fetchPage1]);

  useVisiblePoll(fetchPage1, 15_000);

  const handleLoadMore = useCallback(async () => {
    if (!nextCursor || isLoadingMore) return;
    setIsLoadingMore(true);
    try {
      const res = await fetch(buildSignalsUrl(debouncedTicker, statusFilter, nextCursor));
      if (!res.ok) return;
      const data = await res.json();
      const more: Signal[] = data.signals ?? [];
      setSignals(prev => {
        const existingIds = new Set(prev.map(s => s.ID));
        return [...prev, ...more.filter(s => !existingIds.has(s.ID))];
      });
      setNextCursor(data.next_cursor ?? null);
    } catch (err) {
      console.warn('[SignalsScreen] load more error:', err);
    } finally {
      setIsLoadingMore(false);
    }
  }, [nextCursor, isLoadingMore, debouncedTicker, statusFilter]);

  const handleClosePanel = useCallback(() => setSelectedSignal(null), []);

  const handleCopyCsv = useCallback(() => {
    const csv = signalsToCsv(signals);
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
  }, [signals]);

  const stats = {
    total: signals.length,
    pending: signals.filter(s => s.Status === 'pending').length,
    executed: signals.filter(s => s.Status === 'executed').length,
    blocked: signals.filter(s => s.Status === 'blocked').length,
    approved: signals.filter(s => s.Status === 'approved').length,
    expired: signals.filter(s => s.Status === 'expired').length,
  };

  const tableFooter = (
    <div className={styles.tableFooter}>
      <span className={styles.footerCount}>SHOWING {signals.length}</span>
      <button
        className={styles.loadMoreBtn}
        onClick={handleLoadMore}
        disabled={!nextCursor || isLoadingMore}
      >
        {isLoadingMore ? 'LOADING…' : nextCursor ? 'LOAD MORE' : 'ALL LOADED'}
      </button>
    </div>
  );

  return (
    <div className={styles.screen}>
      <div className={styles.topBar}>
        <div className={styles.breadcrumb}>
          <span className={styles.breadcrumbBrand}>~/</span>
          <span className={styles.breadcrumbPage}>SIGNALS</span>
        </div>
        <div className={styles.systemStatus}>
          v{systemStatus?.version ?? '—'} <span className={styles.statusDot}>●</span> {systemStatus?.status ?? 'LOADING...'}
        </div>
      </div>

      <div className={styles.header}>
        <h1 className={styles.pageTitle}>SIGNALS</h1>
        <button className={styles.copyCsvBtn} onClick={handleCopyCsv} title="Copies loaded rows as CSV to clipboard">COPY CSV</button>
      </div>

      {fetchError && (
        <div className={styles.fetchErrorBanner} role="alert">
          {signals.length > 0 ? '⚠ Data may be stale' : '⚠ Failed to load signals'}
        </div>
      )}

      {isLoading && signals.length === 0 && (
        <div className={styles.loadingBanner} role="status" aria-label="Loading signals">
          Loading…
        </div>
      )}

      <div className={styles.statsRow}>
        <CornerCard className={styles.statCard} bracketColor="var(--text-primary)">
          <div className={styles.statLabel}>LOADED</div>
          <div className={`${styles.statValue} ${styles.statLarge}`}>{stats.total}</div>
        </CornerCard>
        <CornerCard className={styles.statCard} bracketColor="var(--amber)">
          <div className={styles.statLabel}>PENDING</div>
          <div className={`${styles.statValue} ${styles.statAmber}`}>{stats.pending}</div>
        </CornerCard>
        <CornerCard className={styles.statCard} bracketColor="var(--accent)">
          <div className={styles.statLabel}>EXECUTED</div>
          <div className={`${styles.statValue} ${styles.statGreen}`}>{stats.executed}</div>
        </CornerCard>
        <CornerCard className={styles.statCard} bracketColor="var(--red)">
          <div className={styles.statLabel}>BLOCKED</div>
          <div className={`${styles.statValue} ${styles.statRed}`}>{stats.blocked}</div>
        </CornerCard>
        <CornerCard className={styles.statCard} bracketColor="var(--accent)">
          <div className={styles.statLabel}>APPROVED</div>
          <div className={styles.statValue}>{stats.approved}</div>
        </CornerCard>
        <CornerCard className={styles.statCard} bracketColor="var(--text-faint)">
          <div className={styles.statLabel}>EXPIRED</div>
          <div className={`${styles.statValue} ${styles.statDim}`}>{stats.expired}</div>
        </CornerCard>
      </div>

      <div className={styles.filterBar}>
        <div className={styles.filterPills}>
          {STATUS_TABS.map(tab => {
            const isActive = statusFilter === tab;
            return (
              <ChamferedBox
                key={tab || 'all'}
                chamfer={4}
                centerContent
                backgroundColor={isActive ? colors.acidGreen : 'transparent'}
                borderColor={isActive ? colors.acidGreen : 'var(--border)'}
                onClick={() => setStatusFilter(tab)}
                style={{
                  padding: '2px 14px',
                  cursor: 'pointer'
                }}
              >
                <span className={`${styles.filterPill} ${isActive ? styles.filterPillActive : ''}`}
                      style={{ color: isActive ? '#0D0D11' : 'var(--text-primary)' }}>
                  {STATUS_LABELS[tab]}
                </span>
              </ChamferedBox>
            );
          })}
        </div>

        <FilterBar
          searchValue={tickerSearch}
          onSearchChange={setTickerSearch}
          searchPlaceholder="Ticker…"
        />
      </div>

      {!(isLoading && signals.length === 0) && (
        <DataTable<Signal>
          columns={COLUMNS}
          data={signals}
          rowKey={(s) => s.ID}
          onRowClick={setSelectedSignal}
          defaultSortKey="CreatedAt"
          defaultSortDir="desc"
          ariaLabel="Trading signals"
          footer={tableFooter}
        />
      )}

      {selectedSignal && (
        <SignalDetailPanel signal={selectedSignal} onClose={handleClosePanel} />
      )}

      {toastMsg && (
        <div className={`${styles.toast} chamfer-sm`} role="status">{toastMsg}</div>
      )}
    </div>
  );
}
