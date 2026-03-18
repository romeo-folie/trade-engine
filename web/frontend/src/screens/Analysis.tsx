/**
 * Analysis screen — filterable card grid of trading analyses with detail modal.
 *
 * Fetches live analyses from GET /api/analyses/recent?limit=100 every 30s.
 * Action pill filter (ALL | BUY | SELL | HOLD). Card click opens AnalysisModal.
 *
 * Task 8 of the web redesign (closes #206).
 */
import { useState, useEffect, useMemo, useRef, useCallback } from 'react';
import FilterBar from '../components/table/FilterBar';
import AnalysisCard from '../components/analysis/AnalysisCard';
import AnalysisModal from '../components/analysis/AnalysisModal';
import styles from './Analysis.module.css';

/* ── Types ─────────────────────────────────────────────────────────────────── */

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
}

/* ── Filter tabs ───────────────────────────────────────────────────────────── */

const ACTION_TABS = ['', 'BUY', 'SELL', 'HOLD'];
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
      <div className={styles.sectionLabel}>ANALYSIS</div>

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

      <div className={styles.filterRow}>
        <FilterBar
          searchValue={tickerSearch}
          onSearchChange={setTickerSearch}
          searchPlaceholder="Ticker…"
        />
        <div className={styles.modelSearchWrap}>
          <label className={styles.modelSearchLabel} htmlFor="model-filter">MODEL</label>
          <input
            id="model-filter"
            type="text"
            className={styles.modelSearchInput}
            value={modelSearch}
            onChange={e => setModelSearch(e.target.value)}
            placeholder="Model…"
          />
        </div>
      </div>

      <div className={styles.filterPills} role="group" aria-label="Action filter">
        {ACTION_TABS.map(tab => (
          <button
            key={tab}
            type="button"
            aria-pressed={actionFilter === tab}
            className={`${styles.filterPill} ${actionFilter === tab ? styles.filterPillActive : ''}`}
            onClick={() => setActionFilter(tab)}
          >
            {ACTION_LABELS[tab]}
          </button>
        ))}
      </div>

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
