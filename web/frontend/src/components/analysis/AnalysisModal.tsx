import { useEffect, useRef } from 'react';
import { fmtDate, fmtPrice } from '../../utils/format';
import type { AnalysisRecord } from '../../screens/Analysis';
import styles from './AnalysisModal.module.css';

interface AnalysisModalProps {
  analysis: AnalysisRecord;
  onClose: () => void;
}

function dirClass(action: string): string {
  const a = action.toUpperCase();
  if (a === 'BUY') return styles.dirBuy;
  if (a === 'SELL') return styles.dirSell;
  return styles.dirHold;
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

  return (
    <>
      <div className={styles.backdrop} onClick={onClose} />
      <div
        className={`${styles.panel} chamfer-panel-right`}
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
              <span className={`${styles.dirBadge} ${dirClass(analysis.Action)} chamfer-sm`}>
                {analysis.Action.toUpperCase()}
              </span>
            </div>
            <span className={styles.headerMeta}>
              {fmtDate(analysis.CreatedAt)} · {analysis.Strategy}
            </span>
          </div>
          <button type="button" className={`${styles.closeBtn} chamfer-sm`} onClick={onClose} aria-label="Close panel">
            ESC
          </button>
        </div>

        {/* Body */}
        <div className={styles.body}>
          {/* Full reasoning */}
          <div>
            <div className={styles.sectionLabel}>REASONING</div>
            <div className={styles.reasoning}>{analysis.Reasoning}</div>
          </div>

          {/* Metrics grid 2×2 */}
          <div>
            <div className={styles.sectionLabel}>METRICS</div>
            <div className={styles.metricsGrid}>
              <div className={`${styles.metricCard} chamfer-sm`}>
                <span className={styles.metricLabel}>Confidence</span>
                <span className={styles.metricValue}>{confPct}%</span>
              </div>
              <div className={`${styles.metricCard} chamfer-sm`}>
                <span className={styles.metricLabel}>Size %</span>
                <span className={styles.metricValue}>{analysis.SizePct.toFixed(1)}%</span>
              </div>
              <div className={`${styles.metricCard} chamfer-sm`}>
                <span className={styles.metricLabel}>Stop Loss</span>
                <span className={styles.metricValue}>{analysis.StopLoss ? fmtPrice(analysis.StopLoss) : '—'}</span>
              </div>
              <div className={`${styles.metricCard} chamfer-sm`}>
                <span className={styles.metricLabel}>Take Profit</span>
                <span className={styles.metricValue}>{analysis.TakeProfit ? fmtPrice(analysis.TakeProfit) : '—'}</span>
              </div>
            </div>
          </div>

          {/* Model info */}
          <div>
            <div className={styles.sectionLabel}>MODEL</div>
            <div className={styles.modelInfo}>
              <span className={styles.kvLabel}>Provider</span>
              <span className={styles.kvValue}>{analysis.ModelProvider}</span>
              <span className={styles.kvLabel}>Name</span>
              <span className={styles.kvValue}>{analysis.ModelName}</span>
              <span className={styles.kvLabel}>Input Tokens</span>
              <span className={styles.kvValue}>{analysis.InputTokens.toLocaleString()}</span>
              <span className={styles.kvLabel}>Output Tokens</span>
              <span className={styles.kvValue}>{analysis.OutputTokens.toLocaleString()}</span>
              <span className={styles.kvLabel}>Latency</span>
              <span className={styles.kvValue}>{analysis.LatencyMs.toLocaleString()}ms</span>
            </div>
          </div>
        </div>

        {/* Footer */}
        <div className={styles.footer}>
          <button type="button" className={`${styles.btnClose} chamfer-sm`} onClick={onClose}>
            CLOSE
          </button>
        </div>
      </div>
    </>
  );
}
