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

function fmt$(n: number): string {
  return n.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

function fmtPnl(n: number): string {
  const sign = n >= 0 ? '+' : '';
  return `${sign}$${fmt$(Math.abs(n))}`;
}

function fmtPct(n: number): string {
  const sign = n >= 0 ? '+' : '';
  return `${sign}${n.toFixed(2)}%`;
}

export default function PositionsTable({ positions }: PositionsTableProps) {
  return (
    <div className={styles.section}>
      <div className={styles.sectionLabel}>OPEN POSITIONS</div>
      <table className={styles.table}>
        <thead>
          <tr>
            <th>TICKER</th>
            <th>DIR</th>
            <th>STRATEGY</th>
            <th>ENTRY</th>
            <th>CURRENT</th>
            <th>UNREAL P&amp;L</th>
            <th>P&amp;L %</th>
            <th>STATUS</th>
          </tr>
        </thead>
        <tbody>
          {positions.map((p) => {
            const pnlCls = p.unrealizedPnl >= 0 ? styles.positive : styles.negative;
            return (
              <tr key={p.ticker + p.strategy}>
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
