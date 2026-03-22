import { useEffect, useRef } from 'react';
import { fmtPrice, fmtDate } from '../../utils/format';
import CornerCard from '../CornerCard';
import styles from './SignalDetailPanel.module.css';

/* ── Types ─────────────────────────────────────────────────────────────────── */

export interface AnalystData {
  name: string;
  verdict: string;       // 'LONG' | 'SHORT' | 'NEUTRAL'
  confidence: number;    // 0–1
  description: string;
  weight: number;
  active: boolean;
  model: string;
}

export interface EnsembleData {
  verdict: string;
  confidence: number;
}

export interface Signal {
  ID: string;
  Ticker: string;
  Action: string;
  Strategy: string;
  SignalPrice: number;
  StopLoss: number;
  TakeProfit: number;
  Confidence: number;
  SizePct: number;
  Status: string;
  BlockReason: string;
  ExpiresAt: string;
  CreatedAt: string;
  Analysts?: AnalystData[];
  Ensemble?: EnsembleData;
}

interface SignalDetailPanelProps {
  signal: Signal;
  onClose: () => void;
}

/* ── Helpers ───────────────────────────────────────────────────────────────── */

function statusClass(status: string): string {
  const s = status.toLowerCase();
  if (s === 'pending') return styles.statusPending;
  if (s === 'approved') return styles.statusApproved;
  if (s === 'executed') return styles.statusExecuted;
  if (s === 'blocked') return styles.statusBlocked;
  if (s === 'expired' || s === 'stale') return styles.statusExpired;
  return styles.statusBlocked;
}

/* ── Stub analyst data — fallback when API data is absent ────────────────── */

const STUB_ANALYSTS: AnalystData[] = [
  { name: 'Technical', verdict: 'LONG', confidence: 0.87, description: 'RSI 68, MACD crossover confirmed', weight: 0.25, active: true, model: '' },
  { name: 'Momentum', verdict: 'LONG', confidence: 0.92, description: 'Volume spike 2.1x, price breakout', weight: 0.25, active: true, model: '' },
  { name: 'Sentiment', verdict: 'NEUTRAL', confidence: 0.55, description: 'Mixed news sentiment, no clear bias', weight: 0.25, active: true, model: '' },
  { name: 'Macro', verdict: 'NEUTRAL', confidence: 0, description: 'Not routed this cycle', weight: 0.25, active: false, model: '' },
];

/* ── Colour maps ────────────────────────────────────────────────────────── */

const ANALYST_COLORS: Record<string, string> = {
  Technical: 'var(--indigo)',
  Momentum:  'var(--amber)',
  Sentiment: 'var(--violet)',
  Macro:     'var(--blue)',
};

function analystBracketColor(name: string, active: boolean): string {
  if (!active) return 'var(--border)';
  return ANALYST_COLORS[name] ?? 'var(--border)';
}

function analystBarColor(name: string): string {
  return ANALYST_COLORS[name] ?? 'var(--border)';
}

function verdictColor(verdict: string): string {
  if (verdict === 'LONG') return 'var(--accent)';
  if (verdict === 'SHORT') return 'var(--red)';
  return 'var(--amber)';
}

function executionLabel(status: string): { label: string; dotClass: string } {
  const s = status.toLowerCase();
  if (s === 'executed') return { label: 'EXECUTED', dotClass: styles.armedDot };
  if (s === 'blocked') return { label: 'BLOCKED', dotClass: styles.blockedDot };
  return { label: 'ARMED', dotClass: styles.armedDot };
}

/* ── Component ─────────────────────────────────────────────────────────────── */

export default function SignalDetailPanel({ signal, onClose }: SignalDetailPanelProps) {
  const panelRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    function handleKey(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        onClose();
        return;
      }
      // Focus trap: keep Tab/Shift+Tab within the panel
      if (e.key === 'Tab' && panelRef.current) {
        const focusable = panelRef.current.querySelectorAll<HTMLElement>(
          'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
        );
        if (focusable.length === 0) return;
        const first = focusable[0];
        const last = focusable[focusable.length - 1];
        if (e.shiftKey) {
          if (document.activeElement === first) { e.preventDefault(); last?.focus(); }
        } else {
          if (document.activeElement === last) { e.preventDefault(); first?.focus(); }
        }
      }
    }
    document.addEventListener('keydown', handleKey);
    panelRef.current?.focus();
    return () => document.removeEventListener('keydown', handleKey);
  }, [onClose]);

  const dirIsLong = signal.Action.toUpperCase() === 'BUY';

  // Use real analyst data when available, fall back to stubs
  const analysts = signal.Analysts && signal.Analysts.length > 0 ? signal.Analysts : STUB_ANALYSTS;
  const ensemble = signal.Ensemble ?? null;
  const activeAnalysts = analysts.filter(a => a.active);
  const exec = executionLabel(signal.Status);

  return (
    <>
      <div className={styles.backdrop} onClick={onClose} />
      <div
        className={styles.panel}
        ref={panelRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-label={`Signal detail for ${signal.Ticker}`}
      >
        {/* Header */}
        <div className={styles.header}>
          <div className={styles.headerLeft}>
            <span className={styles.title}>{signal.Ticker}</span>
            <span className={`${styles.dirBadge} ${dirIsLong ? styles.dirLong : styles.dirShort} chamfer-sm`}>
              {dirIsLong ? '▲ LONG' : '▼ SHORT'}
            </span>
          </div>
          <span className={styles.confidenceHeader}>CONFIDENCE {(signal.Confidence * 100).toFixed(0)}%</span>
          <button
            type="button"
            className={styles.closeBtn}
            onClick={onClose}
            aria-label="Close signal detail panel"
          >
            ✕
          </button>
        </div>

        {/* Sub-header: timestamp + strategy */}
        <div className={styles.subHeader}>
          <span className={styles.subHeaderText}>{fmtDate(signal.CreatedAt)}</span>
          <span className={styles.subHeaderText}>STRATEGY: {signal.Strategy}</span>
        </div>

        <div className={styles.body}>
          {/* Analyst routing */}
          <div className={styles.routingLabel}>
            ROUTED: {activeAnalysts.length} of {analysts.length} ANALYSTS
          </div>
          <div className={styles.analystCards}>
            {analysts.map(analyst => (
              <CornerCard
                key={analyst.name}
                bracketColor={analystBracketColor(analyst.name, analyst.active)}
                bracketSize={10}
                bracketWidth={1}
                className={`${styles.analystCard} ${!analyst.active ? styles.bypassed : ''}`}
              >
                <div className={styles.analystHeader}>
                  <span className={styles.analystName}>{analyst.name}</span>
                  <span className={styles.analystVerdict} style={{ color: analyst.active ? verdictColor(analyst.verdict) : 'var(--text-faint)' }}>
                    {analyst.active ? analyst.verdict : 'BYPASSED'}
                  </span>
                </div>
                {analyst.active && (
                  <div className={styles.analystBarTrack}>
                    <div className={styles.analystBar} style={{ background: analystBarColor(analyst.name), width: `${analyst.confidence * 100}%` }} />
                  </div>
                )}
                <div className={styles.analystDesc}>{analyst.description}</div>
              </CornerCard>
            ))}
          </div>

          {/* Ensemble synthesis */}
          <div className={styles.ensembleSection}>
            <div className={styles.sectionLabel}>ENSEMBLE SYNTHESIS</div>
            {activeAnalysts.map(a => (
              <div key={a.name} className={styles.ensembleRow}>
                <span className={styles.ensembleLabel}>{a.name}</span>
                <div className={styles.ensembleTrack}>
                  <div className={styles.ensembleFill} style={{ width: `${a.weight * 100}%`, background: analystBarColor(a.name) }} />
                </div>
                <span className={styles.ensembleWeight}>{a.weight.toFixed(2)}</span>
              </div>
            ))}
            <div className={styles.ensembleVerdict}>
              <span className={styles.verdictText} style={{ color: verdictColor(ensemble?.verdict ?? (dirIsLong ? 'LONG' : 'SHORT')) }}>
                {ensemble?.verdict ?? (dirIsLong ? 'LONG' : 'SHORT')}
              </span>
              <span className={styles.verdictScore}>WEIGHTED <strong>{(ensemble?.confidence ?? signal.Confidence).toFixed(2)}</strong></span>
            </div>
          </div>

          {/* Execution */}
          <div className={styles.executionSection}>
            <div className={styles.sectionLabel}>EXECUTION</div>
            <div className={styles.executionStatus}>
              <span className={exec.dotClass} />
              <span className={styles.armedLabel}>{exec.label}</span>
            </div>
            <div className={styles.executionPrice}>
              <span className={styles.priceLabel}>Entry</span>
              <span className={styles.priceValue}>{fmtPrice(signal.SignalPrice)}</span>
            </div>
            <div className={styles.slTp}>
              <span className={styles.sl}>SL {fmtPrice(signal.StopLoss)}</span>
              <span className={styles.tp}>TP {fmtPrice(signal.TakeProfit)}</span>
            </div>
          </div>

          {/* Key-value details */}
          <div className={styles.kvGrid}>
            <span className={styles.kvLabel}>Size %</span>
            <span className={styles.kvValue}>{signal.SizePct.toFixed(1)}%</span>

            {signal.BlockReason && (
              <>
                <span className={styles.kvLabel}>Block Reason</span>
                <span className={styles.kvValue}>{signal.BlockReason}</span>
              </>
            )}

            <span className={styles.kvLabel}>Status</span>
            <span className={styles.kvValue}>
              <span className={`${styles.statusBadge} ${statusClass(signal.Status)} chamfer-sm`}>
                {signal.Status.toUpperCase()}
              </span>
            </span>

            <span className={styles.kvLabel}>Expires</span>
            <span className={styles.kvValue}>{fmtDate(signal.ExpiresAt)}</span>
          </div>

          {/* Action buttons */}
          <div className={styles.actionRow}>
            <button
              type="button"
              className={`${styles.btnOverride} chamfer-sm`}
              disabled
              title="Manual override — not yet implemented"
            >OVERRIDE</button>
            <button type="button" className={`${styles.btnDismiss} chamfer-sm`} onClick={onClose}>CLOSE</button>
          </div>
        </div>
      </div>
    </>
  );
}
