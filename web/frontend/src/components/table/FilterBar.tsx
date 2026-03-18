import type React from 'react';
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
  return (
    <div className={styles.bar}>
      <div className={styles.searchWrap}>
        <label className={styles.searchLabel} htmlFor="filter-search">SEARCH</label>
        <input
          id="filter-search"
          type="text"
          className={styles.searchInput}
          value={searchValue}
          onChange={(e: React.ChangeEvent<HTMLInputElement>) => onSearchChange(e.target.value)}
          placeholder={searchPlaceholder}
        />
      </div>

      {filters?.map(f => (
        <div key={f.label} className={styles.filterWrap}>
          <label className={styles.filterLabel} htmlFor={`filter-${f.label}`}>{f.label}</label>
          <select
            id={`filter-${f.label}`}
            className={styles.filterSelect}
            value={f.value}
            onChange={(e: React.ChangeEvent<HTMLSelectElement>) => f.onChange(e.target.value)}
          >
            {f.options.map(o => (
              <option key={o.value} value={o.value}>{o.label}</option>
            ))}
          </select>
        </div>
      ))}

      {actions && actions.length > 0 && (
        <div className={styles.actions}>
          {actions.map(a => (
            <button
              key={a.label}
              type="button"
              className={`${styles.actionBtn} chamfer-sm`}
              onClick={a.onClick}
            >
              {a.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
