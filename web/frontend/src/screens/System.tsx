/**
 * System screen — KPI strip, Model Performance table, Prompt Cache stats,
 * and live Engine Activity log from WebSocket events.
 *
 * Task 8 of the web redesign (closes #206).
 */
import { useState, useEffect, useRef } from 'react';
import { useEngineSocketContext } from '../contexts/EngineSocketContext';
import { EVENT_LABELS } from '../types/protocol';
import type { IndexedWebEvent } from '../hooks/useEngineSocket';
import CornerCard from '../components/CornerCard';
import styles from './System.module.css';

/* ── Types ─────────────────────────────────────────────────────────────────── */

interface SystemStatus {
  mode: string;
  tick_interval_sec: number;
  tick_interval_min: number; // float: e.g. 0.5 for 30s, 1.0 for 60s
  uptime_seconds: number;
  drawdown?: number;
}

interface CacheStats {
  hits: number;
  misses: number;
  evictions: number;
  errors: number;
}

interface ModelPerfRow {
  Model: string;
  Provider: string;
  TradeCount: number;
  WinRate: number | null;
  AvgPnLPct: number | null;
  AvgLatencyMs: number | null;
  TokensPerCall: number | null;
}

/* ── Helpers ───────────────────────────────────────────────────────────────── */

function fmtUptime(sec: number): string {
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  return `${String(h).padStart(2, '0')}h ${String(m).padStart(2, '0')}m ${String(s).padStart(2, '0')}s`;
}

function fmtPct(v: number | null): string {
  if (v == null) return '—';
  return v.toFixed(1) + '%';
}

function winRateClass(wr: number | null): string {
  if (wr == null) return '';
  const pct = wr * 100;
  if (pct >= 65) return styles.green;
  if (pct >= 55) return styles.amber;
  return styles.red;
}

function fmtTime(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  return d.toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
}

/** Map event type to a badge label for the activity log */
function badgeLabel(ev: IndexedWebEvent): string {
  if (EVENT_LABELS[ev.type]) {
    const label = EVENT_LABELS[ev.type];
    if (label === 'Signal' || label === 'Queued') return 'SIGNAL';
    if (label === 'Analysis' || label === 'Indicators') return 'MODEL';
    if (label === 'Trade' || label === 'Simulated' || label === 'Closed' || label === 'Position') return 'TRADE';
    if (label === 'Risk OK') return 'RISK';
    if (label === 'Blocked' || label === 'Expired') return 'BLOCKED';
    if (label === 'Error') return 'ERROR';
    if (label === 'Market Data') return 'DATA';
    if (label === 'Portfolio') return 'TRADE';
    return label.toUpperCase();
  }
  if (ev.type.toLowerCase().includes('cache')) return 'CACHE';
  return ev.type.toUpperCase().slice(0, 10);
}

/** Map badge label to CSS class */
function badgeClass(label: string): string {
  switch (label) {
    case 'SIGNAL': return styles.badgeSignal;
    case 'MODEL': return styles.badgeModel;
    case 'CACHE': return styles.badgeCache;
    case 'TRADE': return styles.badgeTrade;
    case 'RISK': return styles.badgeRisk;
    case 'BLOCKED':
    case 'ERROR': return styles.badgeBlocked;
    default: return styles.badgeDefault;
  }
}

/** Build a human-readable description for an engine event */
function eventDescription(ev: IndexedWebEvent): string {
  const p = ev.payload as Record<string, unknown> | undefined;
  const parts: string[] = [];

  const label = EVENT_LABELS[ev.type] ?? ev.type;
  parts.push(label);

  if (ev.source) parts.push('—', ev.source);
  if (ev.target && ev.target !== ev.source) parts.push('·', ev.target);

  if (p) {
    if (p.ticker) parts.push('·', String(p.ticker));
    if (p.direction) parts.push(String(p.direction));
    if (p.strategy) parts.push('·', String(p.strategy));
    if (p.confidence != null) parts.push('·', `confidence ${Math.round(Number(p.confidence) * 100)}%`);
    if (p.latency_ms != null) parts.push('·', `${Number(p.latency_ms).toLocaleString()}ms`);
    if (p.tokens != null) parts.push('·', `${Number(p.tokens).toLocaleString()} tokens`);
    if (p.reason) parts.push('·', String(p.reason));
    if (p.entry_price != null) parts.push('·', `entry ${Number(p.entry_price).toLocaleString()}`);
    if (p.drawdown != null) parts.push('·', `drawdown ${(Number(p.drawdown) * 100).toFixed(1)}%`);
  }

  return parts.join(' ');
}



/* ── Component ─────────────────────────────────────────────────────────────── */

export default function SystemScreen() {
  const { status: wsStatus, events } = useEngineSocketContext();

  const [systemStatus, setSystemStatus] = useState<SystemStatus | null>(null);
  const [cacheStats, setCacheStats] = useState<CacheStats | null>(null);
  const [modelPerf, setModelPerf] = useState<ModelPerfRow[]>([]);
  const [signalCount, setSignalCount] = useState<number | null>(null);
  const [riskData, setRiskData] = useState<{ open_trade_count: number } | null>(null);
  const [uptime, setUptime] = useState(0);
  const baseUptime = useRef(0);
  const loadedAt = useRef(Date.now());

  useEffect(() => {
    async function fetchAll() {
      const [statusRes, cacheRes, modelsRes, signalsRes, riskRes] = await Promise.allSettled([
        fetch('/api/system/status'),
        fetch('/api/system/cache'),
        fetch('/api/node/models'),
        fetch('/api/signals/recent?limit=200'),
        fetch('/api/node/risk'),
      ]);

      if (statusRes.status === 'fulfilled' && statusRes.value.ok) {
        const data = await statusRes.value.json();
        setSystemStatus(data);
        baseUptime.current = data.uptime_seconds;
        loadedAt.current = Date.now();
      }
      if (cacheRes.status === 'fulfilled' && cacheRes.value.ok) {
        setCacheStats(await cacheRes.value.json());
      }
      if (modelsRes.status === 'fulfilled' && modelsRes.value.ok) {
        const data = await modelsRes.value.json();
        if (data?.model_performance) setModelPerf(data.model_performance);
      }
      if (signalsRes.status === 'fulfilled' && signalsRes.value.ok) {
        const data = await signalsRes.value.json();
        // Best-effort count from last 200 signals; may undercount if >200 signals today.
        if (data?.signals && Array.isArray(data.signals)) {
          const todayPrefix = new Date().toISOString().slice(0, 10);
          const todayCount = data.signals.filter((s: { CreatedAt: string }) =>
            s.CreatedAt.startsWith(todayPrefix)
          ).length;
          setSignalCount(todayCount);
        }
      }
      if (riskRes.status === 'fulfilled' && riskRes.value.ok) {
        setRiskData(await riskRes.value.json());
      }
    }

    fetchAll();
    const interval = setInterval(fetchAll, 30_000);
    return () => clearInterval(interval);
  }, []);

  // Tick uptime every second
  useEffect(() => {
    const id = setInterval(() => {
      const elapsed = Math.floor((Date.now() - loadedAt.current) / 1000);
      setUptime(baseUptime.current + elapsed);
    }, 1000);
    return () => clearInterval(id);
  }, []);

  const mode = systemStatus?.mode ?? '—';
  const tickSec = systemStatus?.tick_interval_sec ?? null;
  const tickDisplay = tickSec == null ? '—'
    : tickSec < 60 ? `${tickSec}s`
    : `${Math.round(tickSec / 60)}m`;
  const cacheHitRate = cacheStats && (cacheStats.hits + cacheStats.misses) > 0
    ? ((cacheStats.hits / (cacheStats.hits + cacheStats.misses)) * 100).toFixed(1) + '%'
    : '—';

  const openPositions = riskData?.open_trade_count ?? '—';
  const activityEvents = events.slice(0, 100);
  const displayPerf = modelPerf;

  const displayCache = cacheStats;
  const displayHitRate = cacheHitRate;

  const KPI_DATA = [
    { label: 'ENGINE MODE', value: mode.toUpperCase(), color: mode.toUpperCase() === 'PAPER' ? styles.kpiAmber : styles.kpiGreen },
    { label: 'TICK INTERVAL', value: tickDisplay },
    { label: 'SIGNALS TODAY', value: signalCount ?? '—' },
    { label: 'CACHE HIT RATE', value: displayHitRate },
    { label: 'DRAWDOWN', value: fmtPct(systemStatus?.drawdown != null ? systemStatus.drawdown * 100 : null) },
    { label: 'OPEN POSITIONS', value: openPositions },
  ];

  const CACHE_STATS_DATA = [
    { label: 'HITS', value: displayCache?.hits.toLocaleString() ?? '—', color: styles.kpiGreen },
    { label: 'MISSES', value: displayCache?.misses.toLocaleString() ?? '—' },
    { label: 'EVICTIONS', value: displayCache?.evictions.toLocaleString() ?? '—' },
    { label: 'ERRORS', value: displayCache?.errors.toLocaleString() ?? '—', color: (displayCache?.errors ?? 0) > 0 ? styles.kpiRed : undefined },
  ];

  return (
    <div className={styles.screen}>
      {/* Breadcrumb */}
      <div className={styles.headerRow}>
        <div className={styles.breadcrumb}>
          <span className={styles.breadcrumbBrand}>~/</span>
          <span className={styles.breadcrumbPage}>SYSTEM</span>
        </div>
        <div className={styles.versionStatus}>
          ALL SYSTEMS NOMINAL
        </div>
      </div>

      {/* Title row */}
      <div className={styles.titleRow}>
        <div>
          <h1 className={styles.pageTitle}>SYSTEM</h1>
          <div className={styles.uptimeLabel}>UPTIME {fmtUptime(uptime)}</div>
        </div>
        <div className={styles.titleRight}>
          <button type="button" className={
            mode === 'AUTO' ? `${styles.modeBtn} ${styles.modeBtnAuto}`
              : mode === 'MANUAL' ? `${styles.modeBtn} ${styles.modeBtnManual}`
              : `${styles.modeBtn} ${styles.modeBtnPaper}`
          }>{mode} MODE</button>
          <span className={styles.wsStatus}>
            <span className={wsStatus === 'connected' ? styles.greenDot : styles.redDot} />
            {wsStatus === 'connected' ? 'WS CONNECTED' : 'WS ' + wsStatus.toUpperCase()}
          </span>
        </div>
      </div>

      <div className={styles.kpiStrip}>
        {KPI_DATA.map((kpi) => (
          <div key={kpi.label} className={styles.kpiCard}>
            <div className={styles.kpiLabel}>{kpi.label}</div>
            <div className={`${styles.kpiValue} ${kpi.color || ''}`}>{kpi.value}</div>
          </div>
        ))}
      </div>

      {/* Two-column main area */}
      <div className={styles.mainColumns}>
        {/* Left column */}
        <div className={styles.leftCol}>
          <div className={styles.sectionHeader}>
            <span className={styles.sectionTitle}>MODEL PERFORMANCE</span>
            <span className={styles.sectionAnnotation}>min 5 trades</span>
          </div>

          <div className={styles.tableWrap}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th className={styles.th}>MODEL</th>
                  <th className={styles.th}>TRADES</th>
                  <th className={styles.th}>WIN RATE</th>
                  <th className={styles.th}>AVG P&amp;L</th>
                  <th className={styles.th}>AVG LATENCY</th>
                  <th className={styles.th}>TOKENS/CALL</th>
                </tr>
              </thead>
              <tbody>
                {displayPerf.map(row => (
                  <tr key={`${row.Provider}-${row.Model}`}>
                    <td className={styles.td}>{row.Model}</td>
                    <td className={styles.td}>{row.TradeCount}</td>
                    <td className={`${styles.td} ${winRateClass(row.WinRate)}`}>{fmtPct(row.WinRate != null ? row.WinRate * 100 : null)}</td>
                    <td className={`${styles.td} ${(row.AvgPnLPct ?? 0) >= 0 ? styles.green : styles.red}`}>
                      {row.AvgPnLPct != null ? (row.AvgPnLPct >= 0 ? '+' : '') + fmtPct(row.AvgPnLPct * 100) : '—'}
                    </td>
                    <td className={styles.td}>{row.AvgLatencyMs != null ? `${row.AvgLatencyMs.toLocaleString()}ms` : '—'}</td>
                    <td className={styles.td}>{row.TokensPerCall != null ? row.TokensPerCall.toLocaleString() : '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          {/* Prompt Cache */}
          <div className={styles.sectionHeader} style={{ marginTop: 24 }}>
            <span className={styles.sectionTitle}>PROMPT CACHE</span>
            <span className={styles.sectionAnnotation}>process lifetime</span>
          </div>

          <div className={styles.cacheStrip}>
            {CACHE_STATS_DATA.map((item) => (
              <CornerCard key={item.label} bracketColor={item.color || 'var(--border)'} bracketSize={12}>
                <div className={styles.cacheCard}>
                  <div className={styles.cacheLabel}>{item.label}</div>
                  <div className={`${styles.cacheValue} ${item.color || ''}`}>{item.value}</div>
                </div>
              </CornerCard>
            ))}
          </div>
        </div>

        {/* Right column — Engine Activity */}
        <div className={styles.rightCol}>
          <div className={styles.sectionHeader}>
            <span className={styles.sectionTitle}>ENGINE ACTIVITY</span>
            <span className={wsStatus === 'connected' ? styles.liveIndicator : styles.livePolling}>
              <span className={`${styles.pulseDot}${wsStatus !== 'connected' ? ` ${styles.pulseDotAmber}` : ''}`} />
              {wsStatus === 'connected' ? 'LIVE' : 'POLLING'}
            </span>
          </div>

          <div className={styles.activityLog}>
            {activityEvents.length === 0 ? (
              <div className={styles.emptyText}>No events yet</div>
            ) : (
              activityEvents.map(ev => {
                const label = badgeLabel(ev);
                return (
                  <div key={ev._seqId} className={styles.logRow}>
                    <span className={styles.logTime}>{fmtTime(ev.timestamp)}</span>
                    <span className={`${styles.logBadge} ${badgeClass(label)}`}>{label}</span>
                    <span className={styles.logDesc}>{eventDescription(ev)}</span>
                  </div>
                );
              })
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
