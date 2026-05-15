/**
 * Analysis screen — filterable card grid of trading analyses with detail modal.
 *
 * Fetches live analyses from GET /api/analyses/recent?limit=100 every 30s.
 * Action pill filter (ALL | BUY | SELL | HOLD). Card click opens AnalysisModal.
 *
 * Task 8 of the web redesign (closes #206).
 */
import { useState, useEffect, useMemo, useRef, useCallback } from 'react';
import AnalysisCard from '../components/analysis/AnalysisCard';
import AnalysisModal from '../components/analysis/AnalysisModal';
import { ChamferedBox } from '../components/common/ChamferedBox';
import { colors } from '../styles/tokens';
import styles from './Analysis.module.css';

/* ── Types ─────────────────────────────────────────────────────────────────── */

export interface AnalystResult {
  name: string;
  verdict: string;
  confidence: number;
  description: string;
  weight: number;
  active: boolean;
  model: string;
}

export interface AnalysisRecord {
  ID: string;
  Ticker: string;
  Strategy: string;
  Action: string;
  Confidence: number;
  SizePct: number;
  StopLoss: number;
  TakeProfit: number;
  Reasoning: string;
  ModelProvider: string;
  ModelName: string;
  InputTokens: number;
  OutputTokens: number;
  LatencyMs: number;
  CreatedAt: string;
  AnalystResults: AnalystResult[] | null;
  ArbiterReasoning: string;
  DissentSummary: string;
  FastPathed: boolean;
}

/* ── Filter tabs ───────────────────────────────────────────────────────────── */

const ACTION_TABS = ['', 'BUY', 'SELL', 'HOLD'] as const;
const ACTION_LABELS: Record<string, string> = {
  '': 'ALL', 'BUY': 'BUY', 'SELL': 'SELL', 'HOLD': 'HOLD',
};


/* ── Component ─────────────────────────────────────────────────────────────── */

export default function AnalysisScreen() {
  const [analyses, setAnalyses] = useState<AnalysisRecord[]>([]);
  const [isLoading, setIsLoading] = useState(true);
  const [fetchError, setFetchError] = useState<string | null>(null);
  const [tickerSearch, setTickerSearch] = useState('');
  const [modelSearch, setModelSearch] = useState('');
  const [actionFilter, setActionFilter] = useState('');
  const [selected, setSelected] = useState<AnalysisRecord | null>(null);
  const fetchControllerRef = useRef<AbortController | null>(null);

  useEffect(() => {
    async function load() {
      fetchControllerRef.current?.abort();
      const controller = new AbortController();
      fetchControllerRef.current = controller;

      try {
        const res = await fetch('/api/analyses/recent?limit=100', { signal: controller.signal });
        if (!res.ok) {
          setFetchError(`${res.status} ${res.statusText}`);
          return;
        }
        const data = await res.json();
        if (data && Array.isArray(data.analyses)) {
          setAnalyses(data.analyses);
          setSelected(sel =>
            sel ? data.analyses.find((a: AnalysisRecord) => a.ID === sel.ID) ?? sel : null
          );
          setFetchError(null);
        } else {
          setFetchError('Unexpected response shape');
        }
      } catch (err) {
        if (err instanceof Error && err.name !== 'AbortError') {
          console.warn('[AnalysisScreen] fetch error:', err);
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

  const filtered = useMemo(() => {
    let rows = analyses;
    if (tickerSearch) {
      const q = tickerSearch.toLowerCase();
      rows = rows.filter(a => a.Ticker.toLowerCase().includes(q));
    }
    if (modelSearch) {
      const mq = modelSearch.toLowerCase();
      rows = rows.filter(a =>
        a.ModelName.toLowerCase().includes(mq) || a.ModelProvider.toLowerCase().includes(mq)
      );
    }
    if (actionFilter) {
      rows = rows.filter(a => a.Action.toUpperCase() === actionFilter);
    }
    return rows;
  }, [analyses, tickerSearch, modelSearch, actionFilter]);

  const handleClose = useCallback(() => setSelected(null), []);

  return (
    <div className={styles.screen}>
      {/* Breadcrumb + auto-refresh */}
      <div className={styles.headerRow}>
        <div className={styles.breadcrumb}>
          <span className={styles.breadcrumbBrand}>~/</span>
          <span className={styles.breadcrumbPage}>ANALYSIS</span>
        </div>
        <div className={styles.autoRefresh}>
          <span className={styles.greenDot} />
          AUTO-REFRESH 30s
        </div>
      </div>

      <h1 className={styles.pageTitle}>ANALYSIS</h1>

      {fetchError && (
        <div className={styles.fetchErrorBanner} role="alert">
          {analyses.length > 0 ? '⚠ Data may be stale' : '⚠ Failed to load analyses'}
        </div>
      )}

      {isLoading && analyses.length === 0 && (
        <div className={styles.loadingBanner} role="status" aria-label="Loading analyses">
          Loading…
        </div>
      )}

      {/* Filter row 1: search inputs */}
      <div className={styles.filterRow}>
        <div className={styles.searchInput}>
          <span className={styles.searchLabel}>TICKER</span>
          <input
            type="text"
            className={styles.searchField}
            value={tickerSearch}
            onChange={e => setTickerSearch(e.target.value)}
            placeholder="BTC/USDT"
          />
        </div>
        <div className={styles.searchInput}>
          <span className={styles.searchLabel}>MODEL</span>
          <input
            type="text"
            className={styles.searchField}
            value={modelSearch}
            onChange={e => setModelSearch(e.target.value)}
            placeholder="claude-sonnet"
          />
        </div>
      </div>

      {/* Filter row 2: action tabs */}
      <div className={styles.filterPills} role="group" aria-label="Action filter">
        {ACTION_TABS.map(tab => {
          const label = ACTION_LABELS[tab];
          const isActive = actionFilter === tab;
          
          let color = colors.borderMuted;
          if (tab === 'BUY' || tab === '') color = colors.acidGreen;
          else if (tab === 'SELL') color = colors.red;
          else if (tab === 'HOLD') color = colors.amber;

          return (
            <ChamferedBox
              key={tab}
              chamfer={4}
              isActive={isActive}
              backgroundColor={isActive ? color : 'transparent'}
              borderColor={isActive ? color : colors.borderMuted}
              activeColor={color}
              onClick={() => setActionFilter(tab)}
              className={styles.filterPill}
              style={{ 
                padding: '1px 10px',
                display: 'inline-flex',
                alignItems: 'center',
                justifyContent: 'center'
              }}
            >
              <span style={{ color: isActive ? '#0D0D11' : undefined }}>
                {label}
              </span>
            </ChamferedBox>
          );
        })}
      </div>

      {/* Card grid — 3 columns */}
      {!(isLoading && analyses.length === 0) && (
        <div className={styles.grid}>
          {filtered.length === 0 ? (
            <div className={styles.emptyState}>
              <div className={styles.emptyTitle}>NO ANALYSES FOUND</div>
              <div className={styles.emptySub}>
                {analyses.length > 0
                  ? 'Try adjusting your filters'
                  : 'Analyses will appear here once the engine runs'}
              </div>
            </div>
          ) : (
            filtered.map(a => (
              <AnalysisCard
                key={a.ID}
                analysis={a}
                onClick={() => setSelected(a)}
              />
            ))
          )}
        </div>
      )}

      {selected && (
        <AnalysisModal analysis={selected} onClose={handleClose} />
      )}
    </div>
  );
}
