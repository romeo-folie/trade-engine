import styles from './SegmentSelect.module.css';

interface SegmentOption {
  label: string;
  value: string;
  /** Optional prefix element (e.g. warning dot) */
  prefix?: React.ReactNode;
  /** Per-option active colour override */
  activeColor?: 'accent' | 'emerald' | 'amber';
}

interface SegmentSelectProps {
  options: SegmentOption[];
  value: string;
  onChange: (value: string) => void;
  color?: 'accent' | 'emerald' | 'amber';
  fullWidth?: boolean;
}

const COLOR_CLASS: Record<string, string | undefined> = {
  emerald: styles.emerald,
  amber: styles.amber,
};

export default function SegmentSelect({
  options,
  value,
  onChange,
  color = 'accent',
  fullWidth = false,
}: SegmentSelectProps) {
  return (
    <div className={`${styles.group} ${fullWidth ? styles.fullWidth : ''}`}>
      {options.map((opt) => {
        const isActive = opt.value === value;
        const activeVariant = opt.activeColor ?? color;
        const colorCls = isActive ? (COLOR_CLASS[activeVariant] ?? '') : '';
        return (
          <button
            key={opt.value}
            type="button"
            className={`${styles.segment} ${isActive ? styles.active : ''} ${colorCls}`}
            onClick={() => onChange(opt.value)}
          >
            {opt.prefix}
            {opt.label}
          </button>
        );
      })}
    </div>
  );
}
