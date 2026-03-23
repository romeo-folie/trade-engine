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

const ACTION_TABS = ['', 'BUY', 'SELL', 'HOLD'] as const;
const ACTION_LABELS: Record<string, string> = {
  '': 'ALL', 'BUY': 'BUY', 'SELL': 'SELL', 'HOLD': 'HOLD',
};

const MOCK_ANALYSES: AnalysisRecord[] = [
  {
    ID: '1', Ticker: 'BTC/USDT', Strategy: 'TREND', Action: 'BUY', Confidence: 0.87,
    SizePct: 2.5, StopLoss: 81200, TakeProfit: 86500,
    Reasoning: 'Strong momentum on 4H chart. RSI remains below overbought levels. LLM detected breakout pattern from yesterday consolidation.',
    ModelProvider: 'Anthropic', ModelName: 'claude-3-5-sonnet',
    InputTokens: 2120, OutputTokens: 450, LatencyMs: 1240,
    CreatedAt: '2026-03-23T16:04:22Z'
  },
  {
    ID: '2', Ticker: 'ETH/USDT', Strategy: 'MEAN_REV', Action: 'BUY', Confidence: 0.72,
    SizePct: 1.8, StopLoss: 3150, TakeProfit: 3450,
    Reasoning: 'Oversold condition on 1H timeframe. Sentiment analysis shows extreme fear which usually precedes localized bottoming.',
    ModelProvider: 'OpenAI', ModelName: 'gpt-4o',
    InputTokens: 1780, OutputTokens: 380, LatencyMs: 980,
    CreatedAt: '2026-03-23T15:52:10Z'
  },
  {
    ID: '3', Ticker: 'SOL/USDT', Strategy: 'MOMENTUM', Action: 'SELL', Confidence: 0.61,
    SizePct: 1.0, StopLoss: 185.00, TakeProfit: 162.00,
    Reasoning: 'Large sell volume detected on major exchanges. Funding rate is highly positive suggesting a potential long squeeze in short term.',
    ModelProvider: 'Google', ModelName: 'gemini-1.5-pro',
    InputTokens: 2400, OutputTokens: 520, LatencyMs: 1450,
    CreatedAt: '2026-03-23T15:38:45Z'
  },
  {
    ID: '4', Ticker: 'NVDA', Strategy: 'TECH', Action: 'HOLD', Confidence: 0.55,
    SizePct: 0.0, StopLoss: 0, TakeProfit: 0,
    Reasoning: 'Earnings gap already closed. Neutral price action expected while waiting for broader indices to trend. Low signal-to-noise ratio.',
    ModelProvider: 'Anthropic', ModelName: 'claude-3-5-sonnet',
    InputTokens: 1950, OutputTokens: 410, LatencyMs: 1100,
    CreatedAt: '2026-03-23T15:12:08Z'
  },
  {
    ID: '5', Ticker: 'BTC/USDT', Strategy: 'VOLATILITY', Action: 'SELL', Confidence: 0.68,
    SizePct: 1.2, StopLoss: 84000, TakeProfit: 80500,
    Reasoning: 'Exhaustion wick on daily candle. Negative divergence in MACD suggests temporary local top before continuation.',
    ModelProvider: 'Mistral', ModelName: 'mistral-large-2',
    InputTokens: 2100, OutputTokens: 440, LatencyMs: 1620,
    CreatedAt: '2026-03-23T14:45:30Z'
  },
  {
    ID: '6', Ticker: 'AAPL', Strategy: 'NEWS', Action: 'BUY', Confidence: 0.91,
    SizePct: 3.0, StopLoss: 195, TakeProfit: 212,
    Reasoning: 'Positive sentiment from analyst upgrade and supply chain improvements. High confidence due to clear fundamentals alignment.',
    ModelProvider: 'OpenAI', ModelName: 'gpt-4o-mini',
    InputTokens: 1200, OutputTokens: 280, LatencyMs: 650,
    CreatedAt: '2026-03-23T14:22:15Z'
  }
];

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
    let rows = analyses.length > 0 ? analyses : MOCK_ANALYSES;
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
          <span className={styles.breadcrumbBrand}>TRADEENGINE</span>
          <span className={styles.breadcrumbSep}> / </span>
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
