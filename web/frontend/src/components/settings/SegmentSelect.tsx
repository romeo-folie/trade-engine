import styles from './SegmentSelect.module.css';

interface SegmentOption {
  label: string;
  value: string;
  /** Optional prefix element (e.g. warning dot) */
  prefix?: React.ReactNode;
}

interface SegmentSelectProps {
  options: SegmentOption[];
  value: string;
  onChange: (value: string) => void;
  color?: 'accent' | 'emerald';
}

export default function SegmentSelect({
  options,
  value,
  onChange,
  color = 'accent',
}: SegmentSelectProps) {
  return (
    <div className={styles.group}>
      {options.map((opt) => (
        <button
          key={opt.value}
          type="button"
          className={`${styles.segment} ${opt.value === value ? styles.active : ''} ${
            color === 'emerald' ? styles.emerald : ''
          }`}
          onClick={() => onChange(opt.value)}
        >
          {opt.prefix}
          {opt.label}
        </button>
      ))}
    </div>
  );
}
