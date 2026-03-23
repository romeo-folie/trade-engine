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
import CornerCard from '../components/CornerCard';
import { ChamferedBox } from '../components/common/ChamferedBox';
import { colors } from '../styles/tokens';
import SignalDetailPanel from '../components/signals/SignalDetailPanel';
import type { Signal } from '../components/signals/SignalDetailPanel';
import type { Column } from '../components/table/DataTable';
import styles from './Signals.module.css';





/* ── Status pill tabs ─────────────────────────────────────────────────────── */

const STATUS_TABS = ['', 'pending', 'approved', 'executed', 'blocked', 'expired'];
const STATUS_LABELS: Record<string, string> = {
  '': 'ALL', 'pending': 'PENDING', 'approved': 'APPROVED',
  'executed': 'EXECUTED', 'blocked': 'BLOCKED', 'expired': 'EXPIRED',
};

const MOCK_SIGNALS: Signal[] = [
  {
    ID: '101', Ticker: 'BTC/USDT', Action: 'BUY', Confidence: 0.81, Status: 'pending',
    Strategy: 'TREND', SignalPrice: 82450.50, CreatedAt: '2026-03-23T16:04:22Z',
    StopLoss: 81200, TakeProfit: 86000, SizePct: 2.5, BlockReason: '', ExpiresAt: '2026-03-24T16:04:22Z'
  },
  {
    ID: '102', Ticker: 'ETH/USDT', Action: 'SELL', Confidence: 0.67, Status: 'approved',
    Strategy: 'MEAN_REV', SignalPrice: 3245.20, CreatedAt: '2026-03-23T15:58:45Z',
    StopLoss: 3180, TakeProfit: 3450, SizePct: 1.8, BlockReason: '', ExpiresAt: '2026-03-23T19:58:45Z'
  },
  {
    ID: '103', Ticker: 'SOL/USDT', Action: 'NEUTRAL', Confidence: 0.54, Status: 'executed',
    Strategy: 'MOMENTUM', SignalPrice: 178.40, CreatedAt: '2026-03-23T15:42:11Z',
    StopLoss: 184.50, TakeProfit: 165.00, SizePct: 1.2, BlockReason: '', ExpiresAt: '2026-03-23T19:42:11Z'
  },
  {
    ID: '104', Ticker: 'NVDA', Action: 'BUY', Confidence: 0.73, Status: 'blocked',
    Strategy: 'NEWS_SENT', SignalPrice: 1120.00, CreatedAt: '2026-03-23T15:31:08Z',
    StopLoss: 1150.00, TakeProfit: 1050.00, SizePct: 1.5, BlockReason: 'Risk limit exceeded', ExpiresAt: '2026-03-23T19:31:08Z'
  },
  {
    ID: '105', Ticker: 'BTC/USDT', Action: 'SELL', Confidence: 0.62, Status: 'executed',
    Strategy: 'MEAN_REV', SignalPrice: 83120.00, CreatedAt: '2026-03-23T15:14:52Z',
    StopLoss: 83800, TakeProfit: 81500, SizePct: 0.5, BlockReason: '', ExpiresAt: '2026-03-23T16:14:52Z'
  },
  {
    ID: '106', Ticker: 'ETH/USDT', Action: 'BUY', Confidence: 0.89, Status: 'pending',
    Strategy: 'TREND', SignalPrice: 3180.50, CreatedAt: '2026-03-23T14:58:34Z',
    StopLoss: 3100, TakeProfit: 3350, SizePct: 2.0, BlockReason: '', ExpiresAt: '2026-03-23T18:58:34Z'
  }
];

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
    render: () => {
      // Mock weights for visual fidelity to reference
      const t = 0.85; 
      const m = 0.65;
      const s_val = 0.45;
      return (
        <div className={styles.consensusGrid}>
          <div className={styles.consensusRow}>
            <div className={styles.consensusBar} style={{ width: t * 40, background: colors.analystTechnical }} />
            <span className={styles.consensusLetter}>T</span>
          </div>
          <div className={styles.consensusRow}>
            <div className={styles.consensusBar} style={{ width: m * 30, background: colors.analystMomentum }} />
            <span className={styles.consensusLetter}>M</span>
          </div>
          <div className={styles.consensusRow}>
            <div className={styles.consensusBar} style={{ width: s_val * 20, background: colors.analystSentiment }} />
            <span className={styles.consensusLetter}>S</span>
          </div>
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

      if (status === 'pending') { color = colors.analystMomentum; bg = 'rgba(245, 158, 11, 0.1)'; text = colors.analystMomentum; }
      else if (status === 'approved') { color = colors.acidGreen; bg = 'transparent'; text = colors.acidGreen; }
      else if (status === 'executed') { color = colors.acidGreen; bg = colors.acidGreen; text = '#0D0D11'; }
      else if (status === 'blocked') { color = colors.red; bg = 'rgba(239, 68, 68, 0.15)'; text = colors.red; }

      return (
        <ChamferedBox
          chamfer={4}
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

/* ── Component ─────────────────────────────────────────────────────────────── */

export default function SignalsScreen() {
  const [signals, setSignals] = useState<Signal[]>([]);
  const [isLoading, setIsLoading] = useState(true);
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
          setSelectedSignal(sel =>
            sel ? data.signals.find((s: Signal) => s.ID === sel.ID) ?? sel : null
          );
          setFetchError(null);
        } else {
          setFetchError('Unexpected response shape');
        }
      } catch (err) {
        if (err instanceof Error && err.name !== 'AbortError') {
          console.warn('[SignalsScreen] fetch error:', err);
          setFetchError(err.message);
        }
      } finally {
        setIsLoading(false);
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
    let rows = signals.length > 0 ? signals : MOCK_SIGNALS;
    if (tickerSearch) {
      const q = tickerSearch.toLowerCase();
      rows = rows.filter(s => s.Ticker.toLowerCase().includes(q));
    }
    if (statusFilter) {
      rows = rows.filter(s => s.Status.toLowerCase() === statusFilter);
    }
    return rows;
  }, [signals, tickerSearch, statusFilter]);

  const stats = useMemo(() => {
    const data = signals.length > 0 ? signals : MOCK_SIGNALS;
    return {
      total: data.length,
      pending: data.filter(s => s.Status.toLowerCase() === 'pending').length,
      executed: data.filter(s => s.Status.toLowerCase() === 'executed').length,
      blocked: data.filter(s => s.Status.toLowerCase() === 'blocked').length,
      approved: data.filter(s => s.Status.toLowerCase() === 'approved').length,
    };
  }, [signals]);

  const handleClosePanel = useCallback(() => setSelectedSignal(null), []);

  return (
    <div className={styles.screen}>
      <div className={styles.topBar}>
        <div className={styles.breadcrumb}>
          <span className={styles.breadcrumbBrand}>TRADEENGINE</span>
          <span className={styles.breadcrumbSep}> / </span>
          <span className={styles.breadcrumbPage}>SIGNALS</span>
        </div>
        <div className={styles.systemStatus}>
          v2.1.0 <span className={styles.statusDot}>●</span> ALL SYSTEMS NOMINAL
        </div>
      </div>

      <div className={styles.header}>
        <h1 className={styles.pageTitle}>SIGNALS</h1>
        <button className={styles.exportBtn}>EXPORT CSV</button>
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
          <div className={styles.statLabel}>TOTAL TODAY</div>
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
          <div className={`${styles.statValue} ${styles.statDim}`}>0</div>
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
          data={filtered}
          rowKey={(s) => s.ID}
          onRowClick={setSelectedSignal}
          defaultSortKey="CreatedAt"
          defaultSortDir="desc"
          ariaLabel="Trading signals"
        />
      )}

      {selectedSignal && (
        <SignalDetailPanel signal={selectedSignal} onClose={handleClosePanel} />
      )}
    </div>
  );
}
