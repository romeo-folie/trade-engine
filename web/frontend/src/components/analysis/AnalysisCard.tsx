import type { AnalysisRecord } from '../../screens/Analysis';
import styles from './AnalysisCard.module.css';

interface AnalysisCardProps {
  analysis: AnalysisRecord;
  onClick: () => void;
}

function actionColor(action: string) {
  const a = action.toUpperCase();
  if (a === 'BUY') return { chip: styles.chipBuy, conf: styles.confBuy, bar: styles.barBuy };
  if (a === 'SELL') return { chip: styles.chipSell, conf: styles.confSell, bar: styles.barSell };
  return { chip: styles.chipHold, conf: styles.confHold, bar: styles.barHold };
}

function fmtTime(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  return d.toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
}

function fmtSLTP(v: number | null | undefined): string {
  if (v == null || v === 0) return '—';
  return v.toLocaleString('en-US', { maximumFractionDigits: 0 });
}

export default function AnalysisCard({ analysis, onClick }: AnalysisCardProps) {
  const { chip, conf, bar } = actionColor(analysis.Action);
  const confPct = (analysis.Confidence * 100).toFixed(0);

  return (
    <div
      className={styles.card}
      onClick={onClick}
      onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onClick(); } }}
      tabIndex={0}
      role="button"
      aria-label={`${analysis.Ticker} ${analysis.Action} analysis`}
    >
      {/* Row 1: Ticker + timestamp */}
      <div className={styles.headerRow}>
        <span className={styles.ticker}>{analysis.Ticker}</span>
        <span className={styles.timestamp}>{fmtTime(analysis.CreatedAt)}</span>
      </div>

      {/* Row 2: provider / model */}
      <div className={styles.model}>
        {analysis.ModelProvider} / {analysis.ModelName}
      </div>

      {/* Row 3: Signal badge + confidence + bar */}
      <div className={styles.signalRow}>
        <span className={`${styles.chip} ${chip}`}>
          {analysis.Action.toUpperCase()}
        </span>
        <span className={`${styles.confidence} ${conf}`}>{confPct}%</span>
        <div className={styles.confBarTrack}>
          <div
            className={`${styles.confBarFill} ${bar}`}
            style={{ width: `${Math.min(Number(confPct), 100)}%` }}
          />
        </div>
      </div>

      {/* Row 4: Reasoning excerpt */}
      <div className={styles.reasoning}>{analysis.Reasoning}</div>

      {/* Row 5: Strategy + SL/TP */}
      <div className={styles.bottomRow}>
        <span className={styles.strategy}>{analysis.Strategy}</span>
        <span className={styles.slValue}>SL {fmtSLTP(analysis.StopLoss)}</span>
        <span className={styles.tpValue}>TP {fmtSLTP(analysis.TakeProfit)}</span>
      </div>
    </div>
  );
}
