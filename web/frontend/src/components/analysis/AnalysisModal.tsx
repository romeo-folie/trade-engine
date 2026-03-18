import { useEffect, useRef } from 'react';
import type { AnalysisRecord } from '../../screens/Analysis';
import styles from './AnalysisModal.module.css';

interface AnalysisModalProps {
  analysis: AnalysisRecord;
  onClose: () => void;
}

function actionClass(action: string): string {
  const a = action.toUpperCase();
  if (a === 'BUY') return styles.badgeBuy;
  if (a === 'SELL') return styles.badgeSell;
  return styles.badgeHold;
}

function confClass(action: string): string {
  const a = action.toUpperCase();
  if (a === 'BUY') return styles.confBuy;
  if (a === 'SELL') return styles.confSell;
  return styles.confHold;
}

function barClass(action: string): string {
  const a = action.toUpperCase();
  if (a === 'BUY') return styles.barBuy;
  if (a === 'SELL') return styles.barSell;
  return styles.barHold;
}

function fmtDateTime(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  const y = d.getFullYear();
  const mo = String(d.getMonth() + 1).padStart(2, '0');
  const da = String(d.getDate()).padStart(2, '0');
  const h = String(d.getHours()).padStart(2, '0');
  const mi = String(d.getMinutes()).padStart(2, '0');
  const s = String(d.getSeconds()).padStart(2, '0');
  return `${y}-${mo}-${da} ${h}:${mi}:${s}`;
}

function fmtPrice(v: number): string {
  if (!v) return '—';
  return v.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

export default function AnalysisModal({ analysis, onClose }: AnalysisModalProps) {
  const panelRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    function handleKey(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        onClose();
        return;
      }
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

  const confPct = (analysis.Confidence * 100).toFixed(0);
  const action = analysis.Action.toUpperCase();

  return (
    <>
      <div className={styles.backdrop} onClick={onClose} />
      <div
        className={styles.panel}
        ref={panelRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-label={`Analysis detail for ${analysis.Ticker}`}
      >
        {/* Header */}
        <div className={styles.header}>
          <div className={styles.headerLeft}>
            <div className={styles.headerTopRow}>
              <span className={styles.title}>{analysis.Ticker}</span>
              <span className={`${styles.badge} ${actionClass(analysis.Action)}`}>
                {action}
              </span>
            </div>
            <span className={styles.headerMeta}>
              {fmtDateTime(analysis.CreatedAt)} · {analysis.Strategy.replace(/_/g, ' ')} · {analysis.ModelProvider} / {analysis.ModelName}
            </span>
          </div>
          <button type="button" className={styles.closeBtn} onClick={onClose} aria-label="Close panel">
            ×
          </button>
        </div>

        {/* Confidence */}
        <div className={styles.confSection}>
          <span className={styles.confLabel}>CONFIDENCE</span>
          <span className={`${styles.confValue} ${confClass(analysis.Action)}`}>{confPct}%</span>
          <div className={styles.confBarTrack}>
            <div
              className={`${styles.confBarFill} ${barClass(analysis.Action)}`}
              style={{ width: `${Math.min(Number(confPct), 100)}%` }}
            />
          </div>
        </div>

        {/* Body */}
        <div className={styles.body}>
          {/* Full Reasoning */}
          <div className={styles.section}>
            <div className={styles.sectionLabel}>FULL REASONING</div>
            <div className={styles.reasoning}>{analysis.Reasoning}</div>
          </div>

          {/* Signal Parameters */}
          <div className={styles.section}>
            <div className={styles.sectionLabel}>SIGNAL PARAMETERS</div>
            <div className={styles.kvList}>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Action</span>
                <span className={`${styles.kvVal} ${actionClass(analysis.Action)}`}>{action}</span>
              </div>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Stop Loss</span>
                <span className={`${styles.kvVal} ${styles.valRed}`}>
                  {analysis.StopLoss ? fmtPrice(analysis.StopLoss) : '—'}
                </span>
              </div>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Take Profit</span>
                <span className={`${styles.kvVal} ${styles.valGreen}`}>
                  {analysis.TakeProfit ? fmtPrice(analysis.TakeProfit) : '—'}
                </span>
              </div>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Size %</span>
                <span className={styles.kvVal}>{analysis.SizePct.toFixed(1)}%</span>
              </div>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Strategy</span>
                <span className={styles.kvVal}>{analysis.Strategy}</span>
              </div>
            </div>
          </div>

          {/* Model Info */}
          <div className={styles.section}>
            <div className={styles.sectionLabel}>MODEL INFO</div>
            <div className={styles.kvList}>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Model</span>
                <span className={styles.kvVal}>{analysis.ModelName}</span>
              </div>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Input tokens</span>
                <span className={styles.kvVal}>{analysis.InputTokens.toLocaleString()}</span>
              </div>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Output tokens</span>
                <span className={styles.kvVal}>{analysis.OutputTokens.toLocaleString()}</span>
              </div>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Latency</span>
                <span className={styles.kvVal}>{analysis.LatencyMs.toLocaleString()}ms</span>
              </div>
            </div>
          </div>
        </div>
      </div>
    </>
  );
}
