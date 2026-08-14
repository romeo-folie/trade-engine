import Toggle from './Toggle';
import styles from './StrategyRow.module.css';

interface StrategyRowProps {
  name: string;
  enabled: boolean;
}

export default function StrategyRow({ name, enabled }: StrategyRowProps) {
  return (
    <div className={styles.wrapper}>
      <div className={styles.row}>
        <span className={styles.name}>{name}</span>
        <span className={styles.status}>{enabled ? 'ENABLED' : 'DISABLED'}</span>
        <Toggle
          checked={enabled}
          onChange={() => undefined}
          disabled
          aria-label={`${name} strategy is ${enabled ? 'enabled' : 'disabled'} in config`}
        />
      </div>
    </div>
  );
}
