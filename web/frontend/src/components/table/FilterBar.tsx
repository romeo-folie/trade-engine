import { useId } from 'react';
import type React from 'react';
import { ChamferedBox } from '../common/ChamferedBox';
import { colors } from '../../styles/tokens';
import styles from './FilterBar.module.css';

/* ── Types ─────────────────────────────────────────────────────────────────── */

export interface FilterDef {
  label: string;
  value: string;
  onChange: (value: string) => void;
  options: { label: string; value: string }[];
}

export interface ActionDef {
  label: string;
  onClick: () => void;
}

interface FilterBarProps {
  searchValue: string;
  onSearchChange: (value: string) => void;
  searchPlaceholder?: string;
  filters?: FilterDef[];
  actions?: ActionDef[];
}

/* ── Component ─────────────────────────────────────────────────────────────── */

export default function FilterBar({
  searchValue,
  onSearchChange,
  searchPlaceholder = 'Search…',
  filters,
  actions,
}: FilterBarProps) {
  const uid = useId();
  const searchId = `${uid}-search`;

  return (
    <div className={styles.bar}>
      <div className={styles.searchWrap}>
        <label className={styles.searchLabel} htmlFor={searchId}>SEARCH</label>
        <input
          id={searchId}
          type="text"
          className={styles.searchInput}
          value={searchValue}
          onChange={(e: React.ChangeEvent<HTMLInputElement>) => onSearchChange(e.target.value)}
          placeholder={searchPlaceholder}
        />
      </div>

      {filters?.map(f => {
        const filterId = `${uid}-filter-${f.label}`;
        return (
        <div key={f.label} className={styles.filterWrap}>
          <label className={styles.filterLabel} htmlFor={filterId}>{f.label}</label>
          <select
            id={filterId}
            className={styles.filterSelect}
            value={f.value}
            onChange={(e: React.ChangeEvent<HTMLSelectElement>) => f.onChange(e.target.value)}
          >
            {f.options.map(o => (
              <option key={o.value} value={o.value}>{o.label}</option>
            ))}
          </select>
        </div>
        );
      })}

      {actions && actions.length > 0 && (
        <div className={styles.actions}>
          {actions.map(a => (
            <ChamferedBox
              key={a.label}
              chamfer={6}
              borderColor={colors.borderMuted}
              onClick={a.onClick}
              className={styles.actionBtn}
              style={{ padding: '4px 12px' }}
            >
              {a.label}
            </ChamferedBox>
          ))}
        </div>
      )}
    </div>
  );
}
