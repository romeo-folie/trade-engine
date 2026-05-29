/**
 * Analysis screen — filterable card grid of trading analyses with detail modal.
 *
 * Fetches live analyses from GET /api/analyses/recent?limit=100 every 30s.
 * Action pill filter (ALL | BUY | SELL | HOLD). Card click opens AnalysisModal.
 *
 * Task 8 of the web redesign (closes #206).
 */
import { useState, useEffect, useMemo, useCallback } from 'react';
import useSWR from 'swr';
import AnalysisCard from '../components/analysis/AnalysisCard';
import AnalysisModal from '../components/analysis/AnalysisModal';
import { ChamferedBox } from '../components/common/ChamferedBox';
import { jsonFetcher } from '../hooks/swr';
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
  // AnalystResults and ArbiterReasoning are omitted from /api/analyses/recent
  // for payload efficiency — AnalysisModal lazy-fetches them via
  // /api/analyses/{id}. Optional here so consumers can't accidentally rely on
  // their presence from a list response.
  AnalystResults?: AnalystResult[] | null;
  ArbiterReasoning?: string;
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
  const [tickerSearch, setTickerSearch] = useState('');
  const [modelSearch, setModelSearch] = useState('');
  const [actionFilter, setActionFilter] = useState('');
  const [selected, setSelected] = useState<AnalysisRecord | null>(null);

  // SWR handles dedup, cross-screen cache, visibility-aware revalidation, and
  // abort under the hood — replacing the prior useVisiblePoll + AbortController
  // dance. refreshInterval=30s matches the previous cadence; SWR pauses it
  // automatically while the tab is hidden.
  const { data, error, isLoading } = useSWR<{ analyses: AnalysisRecord[] }>(
    '/api/analyses/recent?limit=100',
    jsonFetcher,
    { refreshInterval: 30_000 },
  );

  const analyses = useMemo(() => data?.analyses ?? [], [data]);
  const fetchError = error instanceof Error ? error.message : null;

  // Keep `selected` synced with the latest list on each poll: if the user has a
  // modal open and that analysis was updated server-side, swap in the fresh row.
  useEffect(() => {
    if (!data?.analyses) return;
    setSelected(sel =>
      sel ? data.analyses.find((a: AnalysisRecord) => a.ID === sel.ID) ?? sel : null,
    );
  }, [data]);

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
