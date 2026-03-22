import { useState } from 'react';
import Toggle from './Toggle';
import styles from './StrategyRow.module.css';

interface StrategyRowProps {
  name: string;
  enabled: boolean;
  onToggle: (enabled: boolean) => void;
  params?: string;
  defaultExpanded?: boolean;
}

export default function StrategyRow({
  name,
  enabled,
  onToggle,
  params,
  defaultExpanded = false,
}: StrategyRowProps) {
  const [expanded, setExpanded] = useState(defaultExpanded);

  return (
    <div className={styles.wrapper}>
      <div className={styles.row}>
        <span className={styles.name}>{name}</span>
        <button
          type="button"
          className={styles.chevron}
          onClick={() => setExpanded(!expanded)}
          aria-label={expanded ? 'Collapse' : 'Expand'}
        >
          {expanded ? '▼' : '▶'}
        </button>
        <Toggle checked={enabled} onChange={onToggle} aria-label={`${name} strategy`} />
      </div>
      {expanded && params && (
        <div className={styles.params}>{params}</div>
      )}
    </div>
  );
}
