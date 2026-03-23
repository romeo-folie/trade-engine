import { ChamferedBox } from '../common/ChamferedBox';
import { colors } from '../../styles/tokens';
import type { AnalysisRecord } from '../../screens/Analysis';
import styles from './AnalysisCard.module.css';

interface AnalysisCardProps {
  analysis: AnalysisRecord;
  onClick: () => void;
}

function actionColors(action: string) {
  const a = action.toUpperCase();
  if (a === 'BUY') return colors.acidGreen;
  if (a === 'SELL') return colors.red;
  return colors.amber;
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
  const accent = actionColors(analysis.Action);
  const confPct = (analysis.Confidence * 100).toFixed(0);

  return (
    <ChamferedBox
      chamfer={12}
      className={styles.card}
      onClick={onClick}
      backgroundColor={colors.cardBg}
      borderColor={colors.borderMuted}
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
        <ChamferedBox
          chamfer={4}
          backgroundColor={accent}
          borderColor={accent}
          style={{ 
            padding: '0px 6px',
            display: 'inline-flex',
            alignItems: 'center',
            justifyContent: 'center'
          }}
        >
          <span style={{ fontSize: 9, fontWeight: 700, color: '#0D0D11' }}>
            {analysis.Action.toUpperCase()}
          </span>
        </ChamferedBox>
        <span className={styles.confidence} style={{ color: colors.textPrimary }}>{confPct}%</span>
        <div className={styles.confBarTrack}>
          <div
            className={styles.confBarFill}
            style={{ 
              width: `${Math.min(Number(confPct), 100)}%`,
              backgroundColor: accent
            }}
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
    </ChamferedBox>
  );
}
