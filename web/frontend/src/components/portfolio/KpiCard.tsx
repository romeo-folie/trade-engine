import CornerCard from '../CornerCard';
import styles from './KpiCard.module.css';

interface KpiCardProps {
  label: string;
  value: string;
  /** CSS color for the value text (CSS variable or TOKENS.* hex value) */
  valueColor?: string;
  /** Prominent card gets acid-green full border + larger font */
  prominent?: boolean;
}

/**
 * KPI card — matches portfolio-overview.png design.
 *
 * Normal state:  4 corner L-bracket marks (no outer border).
 * Prominent state: full acid-green perimeter border (active variant).
 */
export default function KpiCard({ label, value, valueColor, prominent }: KpiCardProps) {
  return (
    <CornerCard
      active={prominent}
      bracketColor="rgba(57,255,20,0.4)"
      bracketSize={10}
      bracketWidth={1}
      className={`${styles.card} ${prominent ? styles.prominent : ''}`}
    >
      <span className={styles.label}>{label}</span>
      <span
        className={styles.value}
        style={valueColor ? { color: valueColor } : undefined}
      >
        {value}
      </span>
    </CornerCard>
  );
}
