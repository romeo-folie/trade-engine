import { useState, useMemo } from 'react';
import type React from 'react';
import { TOKENS } from '../../styles/tokens';
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

function fmt$(n: number): string {
  return n.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

function fmtPnl(n: number): string {
  const sign = n >= 0 ? '+' : '-';
  return `${sign}$${fmt$(Math.abs(n))}`;
}

function fmtPct(n: number): string {
  const sign = n >= 0 ? '+' : '';
  return `${sign}${n.toFixed(2)}%`;
}

function SortIndicator({ col, sortKey, sortDir }: { col: SortKey; sortKey: SortKey; sortDir: SortDir }) {
  if (col !== sortKey) return <span style={{ color: 'var(--text-faint)', marginLeft: 4 }}>⇅</span>;
  return <span style={{ color: TOKENS.acidGreen, marginLeft: 4 }}>{sortDir === 'asc' ? '↑' : '↓'}</span>;
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

  function SortTh({ col, children }: { col: SortKey; children: React.ReactNode }) {
    return (
      <th aria-sort={col === sortKey ? (sortDir === 'asc' ? 'ascending' : 'descending') : 'none'}>
        <button
          type="button"
          onClick={() => handleSort(col)}
          style={{ background: 'none', border: 'none', cursor: 'pointer', color: 'inherit',
            fontFamily: 'inherit', fontSize: 'inherit', fontWeight: 'inherit',
            letterSpacing: 'inherit', textTransform: 'inherit', padding: 0, display: 'flex',
            alignItems: 'center', gap: 2 }}
        >
          {children}
        </button>
      </th>
    );
  }

  return (
    <div className={styles.section}>
      <div className={styles.sectionLabel}>OPEN POSITIONS</div>
      <table className={styles.table}>
        <thead>
          <tr>
            <SortTh col="ticker">
              TICKER <SortIndicator col="ticker" sortKey={sortKey} sortDir={sortDir} />
            </SortTh>
            <th>DIR</th>
            <th>STRATEGY</th>
            <SortTh col="entry">
              ENTRY <SortIndicator col="entry" sortKey={sortKey} sortDir={sortDir} />
            </SortTh>
            <SortTh col="current">
              CURRENT <SortIndicator col="current" sortKey={sortKey} sortDir={sortDir} />
            </SortTh>
            <SortTh col="unrealizedPnl">
              UNREAL P&amp;L <SortIndicator col="unrealizedPnl" sortKey={sortKey} sortDir={sortDir} />
            </SortTh>
            <SortTh col="pnlPct">
              P&amp;L % <SortIndicator col="pnlPct" sortKey={sortKey} sortDir={sortDir} />
            </SortTh>
            <th>STATUS</th>
          </tr>
        </thead>
        <tbody>
          {sorted.map((p) => {
            const pnlCls = p.unrealizedPnl >= 0 ? styles.positive : styles.negative;
            return (
              <tr key={`${p.ticker}-${p.strategy}-${p.entry}`}>
                <td style={{ fontWeight: 700 }}>{p.ticker}</td>
                <td>
                  <span
                    className={`${styles.badge} ${p.direction === 'LONG' ? styles.badgeLong : styles.badgeShort} chamfer-sm`}
                  >
                    {p.direction === 'LONG' ? 'BUY' : 'SELL'}
                  </span>
                </td>
                <td style={{ color: 'var(--text-muted)' }}>{p.strategy}</td>
                <td>{fmt$(p.entry)}</td>
                <td>{fmt$(p.current)}</td>
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
