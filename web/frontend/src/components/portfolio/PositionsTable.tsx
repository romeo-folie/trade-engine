import { useState, useMemo } from 'react';
import type React from 'react';
import styles from './PositionsTable.module.css';

export interface Position {
  ticker: string;
  direction: 'LONG' | 'SHORT';
  strategy: string;
  entry: number;
  current: number;
  unrealizedPnl: number;
  pnlPct: number;
}

interface PositionsTableProps {
  positions: Position[];
}

type SortKey = keyof Pick<Position, 'ticker' | 'entry' | 'current' | 'unrealizedPnl' | 'pnlPct'>;
type SortDir = 'asc' | 'desc';

function fmtPrice(n: number): string {
  return n.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

function fmtPnl(n: number): string {
  const sign = n > 0 ? '+' : n < 0 ? '-' : '';
  return `${sign}$${fmtPrice(Math.abs(n))}`;
}

function fmtPct(n: number): string {
  const sign = n > 0 ? '+' : '';
  return `${sign}${n.toFixed(2)}%`;
}

function SortIndicator({ col, sortKey, sortDir }: { col: SortKey; sortKey: SortKey; sortDir: SortDir }) {
  if (col !== sortKey) return <span aria-hidden="true" className={styles.sortIndicator}>⇅</span>;
  return <span aria-hidden="true" className={styles.sortIndicatorActive}>{sortDir === 'asc' ? '↑' : '↓'}</span>;
}

// Human-readable labels for sort keys — used in aria-labels for screen readers
const SORT_LABELS: Record<SortKey, string> = {
  ticker:       'Ticker',
  entry:        'Entry Price',
  current:      'Current Price',
  unrealizedPnl:'Unrealized P&L',
  pnlPct:       'P&L Percent',
};

function SortTh({ col, sortKey, sortDir, onSort, children }: {
  col: SortKey;
  sortKey: SortKey;
  sortDir: SortDir;
  onSort: (col: SortKey) => void;
  children: React.ReactNode;
}) {
  const isActive = col === sortKey;
  // For an active column the next direction toggles; for an inactive column clicking always starts with desc.
  const nextDir = isActive ? (sortDir === 'asc' ? 'descending' : 'ascending') : 'descending';
  const ariaLabel = `${SORT_LABELS[col]}, sort ${nextDir}`;
  return (
    <th scope="col" aria-sort={isActive ? (sortDir === 'asc' ? 'ascending' : 'descending') : 'none'}>
      <button
        type="button"
        className={styles.sortBtn}
        onClick={() => onSort(col)}
        aria-label={ariaLabel}
      >
        {children}
      </button>
    </th>
  );
}

export default function PositionsTable({ positions }: PositionsTableProps) {
  const [sortKey, setSortKey] = useState<SortKey>('unrealizedPnl');
  const [sortDir, setSortDir] = useState<SortDir>('desc');

  function handleSort(col: SortKey) {
    if (col === sortKey) {
      setSortDir(d => d === 'asc' ? 'desc' : 'asc');
    } else {
      setSortKey(col);
      setSortDir('desc');
    }
  }

  const sorted = useMemo(() => {
    return [...positions].sort((a, b) => {
      const av = a[sortKey];
      const bv = b[sortKey];
      if (typeof av === 'string' && typeof bv === 'string') {
        return sortDir === 'asc' ? av.localeCompare(bv) : bv.localeCompare(av);
      }
      return sortDir === 'asc' ? (av as number) - (bv as number) : (bv as number) - (av as number);
    });
  }, [positions, sortKey, sortDir]);

  return (
    <div className={styles.section}>
      <div className={styles.sectionLabel}>OPEN POSITIONS</div>
      <table className={styles.table} aria-label="Open positions">
        <thead>
          <tr>
            <SortTh col="ticker" sortKey={sortKey} sortDir={sortDir} onSort={handleSort}>
              TICKER <SortIndicator col="ticker" sortKey={sortKey} sortDir={sortDir} />
            </SortTh>
            <th scope="col">DIR</th>
            <th scope="col">STRATEGY</th>
            <SortTh col="entry" sortKey={sortKey} sortDir={sortDir} onSort={handleSort}>
              ENTRY <SortIndicator col="entry" sortKey={sortKey} sortDir={sortDir} />
            </SortTh>
            <SortTh col="current" sortKey={sortKey} sortDir={sortDir} onSort={handleSort}>
              CURRENT <SortIndicator col="current" sortKey={sortKey} sortDir={sortDir} />
            </SortTh>
            <SortTh col="unrealizedPnl" sortKey={sortKey} sortDir={sortDir} onSort={handleSort}>
              UNREAL P&amp;L <SortIndicator col="unrealizedPnl" sortKey={sortKey} sortDir={sortDir} />
            </SortTh>
            <SortTh col="pnlPct" sortKey={sortKey} sortDir={sortDir} onSort={handleSort}>
              P&amp;L % <SortIndicator col="pnlPct" sortKey={sortKey} sortDir={sortDir} />
            </SortTh>
            <th scope="col">STATUS</th>
          </tr>
        </thead>
        <tbody>
          {sorted.map((p, i) => {
            const pnlCls = p.unrealizedPnl >= 0 ? styles.positive : styles.negative;
            // Use index as tiebreaker — the backend provides no stable row id.
            // Positions for the same ticker+strategy+entry are indistinguishable without one.
            const rowKey = `${p.ticker}-${p.strategy}-${p.entry}-${i}`;
            return (
              <tr key={rowKey}>
                <td style={{ fontWeight: 700 }}>{p.ticker}</td>
                <td>
                  <span
                    className={`${styles.badge} ${p.direction === 'LONG' ? styles.badgeLong : styles.badgeShort} chamfer-sm`}
                  >
                    {p.direction === 'LONG' ? 'BUY' : 'SELL'}
                  </span>
                </td>
                <td style={{ color: 'var(--text-muted)' }}>{p.strategy}</td>
                <td>{fmtPrice(p.entry)}</td>
                <td>{fmtPrice(p.current)}</td>
                <td className={pnlCls}>{fmtPnl(p.unrealizedPnl)}</td>
                <td className={pnlCls}>{fmtPct(p.pnlPct)}</td>
                <td>
                  <span className={styles.status}>
                    <span className={styles.statusDot} />
                    OPEN
                  </span>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
