import React, { useState, useMemo } from 'react';
import styles from './DataTable.module.css';

/* ── Types ─────────────────────────────────────────────────────────────────── */

export interface Column<T> {
  key: string;
  label: string;
  sortable?: boolean;
  sortKey?: keyof T;
  render: (row: T) => React.ReactNode;
}

interface DataTableProps<T> {
  columns: Column<T>[];
  data: T[];
  rowKey: (row: T, idx: number) => string;
  onRowClick?: (row: T) => void;
  expandedRowKey?: string | null;
  renderExpandedRow?: (row: T) => React.ReactNode;
  defaultSortKey?: keyof T;
  defaultSortDir?: 'asc' | 'desc';
  ariaLabel?: string;
}

type SortDir = 'asc' | 'desc';

/* ── Sort indicator ────────────────────────────────────────────────────────── */

function SortGlyph({ active, dir }: { active: boolean; dir: SortDir }) {
  if (!active) return <span aria-hidden="true" className={styles.sortIndicator}>⇅</span>;
  return <span aria-hidden="true" className={styles.sortIndicatorActive}>{dir === 'asc' ? '↑' : '↓'}</span>;
}

/* ── Component ─────────────────────────────────────────────────────────────── */

export default function DataTable<T>({
  columns,
  data,
  rowKey,
  onRowClick,
  expandedRowKey,
  renderExpandedRow,
  defaultSortKey,
  defaultSortDir = 'desc',
  ariaLabel = 'Data table',
}: DataTableProps<T>) {
  const [sortKey, setSortKey] = useState<keyof T | null>(defaultSortKey ?? null);
  const [sortDir, setSortDir] = useState<SortDir>(defaultSortDir);

  function handleSort(col: Column<T>) {
    const sk = col.sortKey ?? (col.key as keyof T);
    if (sk === sortKey) {
      setSortDir(d => (d === 'asc' ? 'desc' : 'asc'));
    } else {
      setSortKey(sk);
      setSortDir('desc');
    }
  }

  const sorted = useMemo(() => {
    if (sortKey == null) return data;
    return [...data].sort((a, b) => {
      const av = a[sortKey];
      const bv = b[sortKey];
      if (typeof av === 'string' && typeof bv === 'string') {
        return sortDir === 'asc' ? av.localeCompare(bv) : bv.localeCompare(av);
      }
      const an = Number(av); const bn = Number(bv);
      if (isNaN(an) || isNaN(bn)) return 0;
      return sortDir === 'asc' ? an - bn : bn - an;
    });
  }, [data, sortKey, sortDir]);

  return (
    <table className={styles.table} aria-label={ariaLabel}>
      <thead>
        <tr>
          {columns.map(col => {
            const sk = col.sortKey ?? (col.key as keyof T);
            const isActive = col.sortable && sk === sortKey;
            const nextDir = isActive ? (sortDir === 'asc' ? 'descending' : 'ascending') : 'descending';

            return col.sortable ? (
              <th
                key={col.key}
                scope="col"
                aria-sort={isActive ? (sortDir === 'asc' ? 'ascending' : 'descending') : 'none'}
              >
                <button
                  type="button"
                  className={styles.sortBtn}
                  onClick={() => handleSort(col)}
                  aria-label={`${col.label}, sort ${nextDir}`}
                >
                  {col.label}
                  <SortGlyph active={!!isActive} dir={sortDir} />
                </button>
              </th>
            ) : (
              <th key={col.key} scope="col">{col.label}</th>
            );
          })}
        </tr>
      </thead>
      <tbody>
        {sorted.length === 0 ? (
          <tr className={styles.emptyRow}>
            <td colSpan={columns.length}>No data</td>
          </tr>
        ) : (
          sorted.map((row, i) => {
            const key = rowKey(row, i);
            const isExpanded = expandedRowKey != null && key === expandedRowKey;
            return (
              <React.Fragment key={key}>
                <tr
                  className={onRowClick ? styles.clickable : undefined}
                  onClick={onRowClick ? () => onRowClick(row) : undefined}
                  tabIndex={onRowClick ? 0 : undefined}
                  onKeyDown={onRowClick ? (e) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault();
                      onRowClick(row);
                    }
                  } : undefined}
                >
                  {columns.map(col => (
                    <td key={col.key}>{col.render(row)}</td>
                  ))}
                </tr>
                {isExpanded && renderExpandedRow && (
                  <tr className={styles.expandedRow}>
                    <td colSpan={columns.length}>
                      {renderExpandedRow(row)}
                    </td>
                  </tr>
                )}
              </React.Fragment>
            );
          })
        )}
      </tbody>
    </table>
  );
}
