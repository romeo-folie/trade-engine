import { fmtDate } from '../../utils/format';
import type { AnalysisRecord } from '../../screens/Analysis';
import styles from './AnalysisCard.module.css';

interface AnalysisCardProps {
  analysis: AnalysisRecord;
  onClick: () => void;
}

function actionStyle(action: string) {
  const a = action.toUpperCase();
  if (a === 'BUY') return { chip: styles.chipBuy, conf: styles.confBuy };
  if (a === 'SELL') return { chip: styles.chipSell, conf: styles.confSell };
  return { chip: styles.chipHold, conf: styles.confHold };
}

export default function AnalysisCard({ analysis, onClick }: AnalysisCardProps) {
  const { chip, conf } = actionStyle(analysis.Action);
  const confPct = (analysis.Confidence * 100).toFixed(0);

  return (
    <div
      className={`${styles.card} chamfer`}
      onClick={onClick}
      onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onClick(); } }}
      tabIndex={0}
      role="button"
      aria-label={`${analysis.Ticker} ${analysis.Action} analysis`}
    >
      <span className={styles.bracketTL} aria-hidden="true">⌐</span>
      <span className={styles.bracketBR} aria-hidden="true">¬</span>

      <div className={styles.headerRow}>
        <span className={styles.ticker}>{analysis.Ticker}</span>
        <span className={styles.timestamp}>{fmtDate(analysis.CreatedAt)}</span>
      </div>

      <div className={styles.model}>
        {analysis.ModelProvider}/{analysis.ModelName}
      </div>

      <div className={styles.recRow}>
        <span className={`${styles.chip} ${chip} chamfer-sm`}>
          {analysis.Action.toUpperCase()}
        </span>
        <span className={`${styles.confidence} ${conf}`}>{confPct}%</span>
      </div>

      <div className={styles.reasoning}>{analysis.Reasoning}</div>
    </div>
  );
}
