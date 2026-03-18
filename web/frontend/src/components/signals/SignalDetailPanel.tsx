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
  return n.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
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
  if (s === 'executed' || s === 'approved') return styles.statusExecuted;
  return styles.statusBlocked;
}

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

  // Price range: compute fill position between SL and TP
  const sl = signal.StopLoss;
  const tp = signal.TakeProfit;
  const entry = signal.SignalPrice;
  const rangeMin = Math.min(sl, tp, entry);
  const rangeMax = Math.max(sl, tp, entry);
  const span = rangeMax - rangeMin || 1;
  const slPct = ((sl - rangeMin) / span) * 100;
  const tpPct = ((tp - rangeMin) / span) * 100;
  const leftPct = Math.min(slPct, tpPct);
  const widthPct = Math.abs(tpPct - slPct);

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
        <div className={styles.header}>
          <span className={styles.title}>{signal.Ticker}</span>
          <button type="button" className={`${styles.closeBtn} chamfer-sm`} onClick={onClose} aria-label="Close panel">
            ESC
          </button>
        </div>

        <div className={styles.body}>
          {/* Key-value grid */}
          <div className={styles.kvGrid}>
            <span className={styles.kvLabel}>ID</span>
            <span className={styles.kvValue}>{signal.ID}</span>

            <span className={styles.kvLabel}>Action</span>
            <span className={`${styles.kvValue} ${signal.Action.toUpperCase() === 'BUY' ? styles.actionBuy : styles.actionSell}`}>
              {signal.Action.toUpperCase()}
            </span>

            <span className={styles.kvLabel}>Strategy</span>
            <span className={styles.kvValue}>{signal.Strategy}</span>

            <span className={styles.kvLabel}>Status</span>
            <span className={`${styles.statusBadge} ${statusClass(signal.Status)} chamfer-sm`}>
              {signal.Status.toUpperCase()}
            </span>

            <span className={styles.kvLabel}>Entry Price</span>
            <span className={styles.kvValue}>${fmtPrice(signal.SignalPrice)}</span>

            <span className={styles.kvLabel}>Stop Loss</span>
            <span className={`${styles.kvValue} ${styles.priceRangeSl}`}>${fmtPrice(signal.StopLoss)}</span>

            <span className={styles.kvLabel}>Take Profit</span>
            <span className={`${styles.kvValue} ${styles.priceRangeTp}`}>${fmtPrice(signal.TakeProfit)}</span>

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

          {/* Confidence bar */}
          <div className={styles.confidenceSection}>
            <div className={styles.confidenceLabel}>CONFIDENCE</div>
            <div className={styles.confidenceBar}>
              <div
                className={styles.confidenceFill}
                style={{ width: `${Math.round(signal.Confidence * 100)}%` }}
              />
            </div>
            <div className={styles.confidenceValue}>{(signal.Confidence * 100).toFixed(1)}%</div>
          </div>

          {/* Price range mini bar */}
          <div className={styles.priceRange}>
            <div className={styles.priceRangeLabel}>PRICE RANGE</div>
            <div className={styles.priceRangeBar}>
              <span className={styles.priceRangeSl}>SL ${fmtPrice(sl)}</span>
              <div className={styles.priceRangeTrack}>
                <div
                  className={styles.priceRangeFill}
                  style={{
                    left: `${leftPct}%`,
                    width: `${widthPct}%`,
                    background: `linear-gradient(to right, var(--red), var(--emerald))`,
                  }}
                />
              </div>
              <span className={styles.priceRangeTp}>TP ${fmtPrice(tp)}</span>
            </div>
          </div>
        </div>
      </div>
    </>
  );
}
