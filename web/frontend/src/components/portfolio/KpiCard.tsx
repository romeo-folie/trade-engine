import styles from './KpiCard.module.css';

interface KpiCardProps {
  label: string;
  value: string;
  /** CSS color for the value text (CSS variable or TOKENS.* hex value) */
  valueColor?: string;
  /** Prominent card gets acid-green border + larger font */
  prominent?: boolean;
}

/**
 * KPI card — matches portfolio-overview.png design.
 * Uses corner bracket accents (⌐ top-left / ¬ bottom-right) per the design system,
 * consistent with PipelineNodeCard analyst nodes. NOT chamfer clip-path.
 */
export default function KpiCard({ label, value, valueColor, prominent }: KpiCardProps) {
  const cls = [styles.card];
  if (prominent) cls.push(styles.prominent);

  return (
    <div className={cls.join(' ')}>
      <span className={styles.bracketTL} aria-hidden="true">⌐</span>
      <span className={styles.bracketBR} aria-hidden="true">¬</span>
      <span className={styles.label}>{label}</span>
      <span className={styles.value} style={valueColor ? { color: valueColor } : undefined}>
        {value}
      </span>
    </div>
  );
}
