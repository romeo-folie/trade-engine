import { useEffect, useRef } from 'react';
import styles from './SignalDetailPanel.module.css';

/* ── Types ─────────────────────────────────────────────────────────────────── */

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
}

interface SignalDetailPanelProps {
  signal: Signal;
  onClose: () => void;
}

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

function statusClass(status: string): string {
  const s = status.toLowerCase();
  if (s === 'pending') return styles.statusPending;
  if (s === 'approved') return styles.statusApproved;
  if (s === 'executed') return styles.statusExecuted;
  if (s === 'blocked') return styles.statusBlocked;
  return styles.statusBlocked;
}

/* ── Stub analyst data — replace with real API data once #217 ships ──────── */

const STUB_ANALYSTS = [
  { name: 'Technical', verdict: 'LONG', verdictColor: 'var(--accent)', bracketColor: 'var(--blue)', barColor: 'var(--blue)', confidence: 0.87, description: 'RSI 68, MACD crossover confirmed', bypassed: false },
  { name: 'Momentum', verdict: 'LONG', verdictColor: 'var(--accent)', bracketColor: 'var(--amber)', barColor: 'var(--amber)', confidence: 0.92, description: 'Volume spike 2.1x, price breakout', bypassed: false },
  { name: 'Sentiment', verdict: 'NEUTRAL', verdictColor: 'var(--amber)', bracketColor: 'var(--violet)', barColor: 'var(--violet)', confidence: 0.55, description: 'Mixed news sentiment, no clear bias', bypassed: false },
  { name: 'Macro', verdict: 'BYPASSED', verdictColor: 'var(--text-faint)', bracketColor: 'var(--border)', barColor: 'var(--border)', confidence: 0, description: 'Not routed this cycle', bypassed: true },
];

/* ── Component ─────────────────────────────────────────────────────────────── */

export default function SignalDetailPanel({ signal, onClose }: SignalDetailPanelProps) {
  const panelRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    function handleKey(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose();
    }
    document.addEventListener('keydown', handleKey);
    panelRef.current?.focus();
    return () => document.removeEventListener('keydown', handleKey);
  }, [onClose]);

  const dirIsLong = signal.Action.toUpperCase() === 'BUY';

  return (
    <>
      <div className={styles.backdrop} onClick={onClose} />
      <div
        className={styles.panel}
        ref={panelRef}
        tabIndex={-1}
        role="dialog"
        aria-label={`Signal detail for ${signal.Ticker}`}
      >
        {/* Header */}
        <div className={styles.header}>
          <div className={styles.headerLeft}>
            <span className={styles.title}>{signal.Ticker}</span>
            <span className={`${styles.dirBadge} ${dirIsLong ? styles.dirLong : styles.dirShort} chamfer-sm`}>
              {dirIsLong ? '▲ LONG' : '▼ SHORT'}
            </span>
            <span className={`${styles.statusBadge} ${statusClass(signal.Status)} chamfer-sm`}>
              {signal.Status.toUpperCase()}
            </span>
          </div>
          <button type="button" className={`${styles.closeBtn} chamfer-sm`} onClick={onClose} aria-label="Close panel">
            ESC
          </button>
        </div>

        {/* Confidence */}
        <div className={styles.confidenceRow}>
          <span className={styles.confidenceLabel}>CONFIDENCE</span>
          <span className={styles.confidenceValue}>{(signal.Confidence * 100).toFixed(0)}%</span>
          <div className={styles.confidenceBar}>
            <div
              className={styles.confidenceFill}
              style={{ width: `${Math.round(signal.Confidence * 100)}%` }}
            />
          </div>
        </div>

        <div className={styles.body}>
          {/* Analyst routing */}
          <div className={styles.routingLabel}>
            ROUTED: {STUB_ANALYSTS.filter(a => !a.bypassed).length} of {STUB_ANALYSTS.length} ANALYSTS
          </div>
          <div className={styles.analystCards}>
            {STUB_ANALYSTS.map(analyst => (
              <div key={analyst.name} className={`${styles.analystCard} ${analyst.bypassed ? styles.bypassed : ''}`}>
                <span className={styles.bracketTL} style={{ color: analyst.bracketColor }}>⌐</span>
                <span className={styles.bracketBR} style={{ color: analyst.bracketColor }}>¬</span>
                <div className={styles.analystHeader}>
                  <span className={styles.analystName}>{analyst.name}</span>
                  <span className={styles.analystVerdict} style={{ color: analyst.verdictColor }}>
                    {analyst.verdict}
                  </span>
                </div>
                {!analyst.bypassed && (
                  <div className={styles.analystBarTrack}>
                    <div className={styles.analystBar} style={{ background: analyst.barColor, width: `${analyst.confidence * 100}%` }} />
                  </div>
                )}
                <div className={styles.analystDesc}>{analyst.description}</div>
              </div>
            ))}
          </div>

          {/* Ensemble synthesis */}
          <div className={styles.ensembleSection}>
            <div className={styles.sectionLabel}>ENSEMBLE SYNTHESIS</div>
            {STUB_ANALYSTS.filter(a => !a.bypassed).map(a => (
              <div key={a.name} className={styles.ensembleRow}>
                <span className={styles.ensembleLabel}>{a.name}</span>
                <div className={styles.ensembleTrack}>
                  <div className={styles.ensembleFill} style={{ width: `${a.confidence * 100}%`, background: a.barColor }} />
                </div>
                <span className={styles.ensembleWeight}>{a.confidence.toFixed(2)}</span>
              </div>
            ))}
            <div className={styles.ensembleVerdict}>
              <span className={styles.verdictText}>{dirIsLong ? 'LONG' : 'SHORT'}</span>
              <span className={styles.verdictScore}>WEIGHTED <strong>{signal.Confidence.toFixed(2)}</strong></span>
            </div>
          </div>

          {/* Execution */}
          <div className={styles.executionSection}>
            <div className={styles.sectionLabel}>EXECUTION</div>
            <div className={styles.executionStatus}>
              <span className={styles.armedDot} />
              <span className={styles.armedLabel}>ARMED</span>
            </div>
            <div className={styles.executionPrice}>
              <span className={styles.priceLabel}>ENTRY</span>
              <span className={styles.priceValue}>{fmtPrice(signal.SignalPrice)}</span>
            </div>
            <div className={styles.slTp}>
              <span className={styles.sl}>SL {fmtPrice(signal.StopLoss)}</span>
              <span className={styles.tp}>TP {fmtPrice(signal.TakeProfit)}</span>
            </div>
          </div>

          {/* Key-value details */}
          <div className={styles.kvGrid}>
            <span className={styles.kvLabel}>Strategy</span>
            <span className={styles.kvValue}>{signal.Strategy}</span>

            <span className={styles.kvLabel}>Size %</span>
            <span className={styles.kvValue}>{signal.SizePct.toFixed(1)}%</span>

            {signal.BlockReason && (
              <>
                <span className={styles.kvLabel}>Block Reason</span>
                <span className={styles.kvValue}>{signal.BlockReason}</span>
              </>
            )}

            <span className={styles.kvLabel}>Expires</span>
            <span className={styles.kvValue}>{fmtDate(signal.ExpiresAt)}</span>

            <span className={styles.kvLabel}>Created</span>
            <span className={styles.kvValue}>{fmtDate(signal.CreatedAt)}</span>
          </div>

          {/* Action buttons */}
          <div className={styles.actionRow}>
            <button type="button" className={`${styles.btnOverride} chamfer-sm`}>OVERRIDE</button>
            <button type="button" className={`${styles.btnDismiss} chamfer-sm`} onClick={onClose}>DISMISS</button>
          </div>
        </div>
      </div>
    </>
  );
}
