/**
 * OverlayChart — dual-series equity curve.
 *
 * Renders Alpaca equity (solid, neutral) and TradeEngine equity (dashed, accent)
 * on a shared time axis. Supports three display modes:
 *   - "overlay": both series rendered together (default)
 *   - "alpaca":  only the Alpaca broker series
 *   - "te":      only the TradeEngine DB series
 *
 * Used by the Portfolio screen to surface broker-truth vs our own snapshotted
 * equity side-by-side.
 */
import { useMemo, useId } from 'react';
import {
  LineChart,
  Line,
  XAxis,
  YAxis,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
} from 'recharts';
import { TOKENS } from '../../styles/tokens';
import styles from './OverlayChart.module.css';

export type ChartMode = 'overlay' | 'alpaca' | 'te';

interface EquityPoint {
  t: string;   // ISO timestamp from broker  OR "YYYY-MM-DD" from TE DB
  v: number;
}

interface OverlayChartProps {
  alpaca: EquityPoint[];
  tradeEngine: EquityPoint[];
  mode: ChartMode;
}

// Merge two series on a shared time axis, preferring the alpaca timestamp
// as the canonical key. Both series may have different cadences.
function mergePoints(
  alpaca: EquityPoint[],
  te: EquityPoint[],
): Array<{ date: string; alpaca?: number; te?: number }> {
  const byDate = new Map<string, { alpaca?: number; te?: number }>();

  const dateKey = (t: string) => {
    // Normalise both ISO timestamps and YYYY-MM-DD to YYYY-MM-DD.
    const d = new Date(t);
    if (isNaN(d.getTime())) return t.slice(0, 10);
    return d.toISOString().slice(0, 10);
  };

  for (const p of alpaca) {
    const k = dateKey(p.t);
    byDate.set(k, { ...byDate.get(k), alpaca: p.v });
  }
  for (const p of te) {
    const k = dateKey(p.t);
    byDate.set(k, { ...byDate.get(k), te: p.v });
  }

  return [...byDate.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([date, v]) => ({ date, ...v }));
}

function formatDate(d: string) {
  try {
    return new Date(`${d}T00:00:00`).toLocaleDateString('en-US', { month: 'short', day: '2-digit' }).toUpperCase();
  } catch {
    return d;
  }
}

function fmt$(n: number) {
  return '$' + (n / 1000).toFixed(1) + 'K';
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function CustomTooltip({ active, payload, label }: any) {
  if (!active || !payload?.length) return null;

  const alpacaEntry = payload.find((p: any) => p.dataKey === 'alpaca');
  const teEntry     = payload.find((p: any) => p.dataKey === 'te');
  const alpacaVal   = alpacaEntry?.value as number | undefined;
  const teVal       = teEntry?.value as number | undefined;
  const delta       = alpacaVal != null && teVal != null ? alpacaVal - teVal : null;

  return (
    <div className={styles.tooltip}>
      <div className={styles.tooltipDate}>{label}</div>
      {alpacaVal != null && (
        <div className={styles.tooltipRow}>
          <span style={{ color: TOKENS.textMuted }}>■</span>
          <span className={styles.tooltipLabel}>Alpaca</span>
          <span className={styles.tooltipVal}>${alpacaVal.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}</span>
        </div>
      )}
      {teVal != null && (
        <div className={styles.tooltipRow}>
          <span style={{ color: TOKENS.acidGreen }}>╌</span>
          <span className={styles.tooltipLabel}>TradeEngine</span>
          <span className={styles.tooltipVal}>${teVal.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}</span>
        </div>
      )}
      {delta != null && (
        <div className={styles.tooltipDelta} style={{ color: delta >= 0 ? TOKENS.emerald : TOKENS.red }}>
          Δ {delta >= 0 ? '+' : ''}{delta.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}
        </div>
      )}
    </div>
  );
}

export default function OverlayChart({ alpaca, tradeEngine, mode }: OverlayChartProps) {
  const uid = useId();
  const glowId = `overlay-glow-${uid.replace(/:/g, '')}`;

  const data = useMemo(() => mergePoints(alpaca, tradeEngine), [alpaca, tradeEngine]);

  const showAlpaca = mode === 'overlay' || mode === 'alpaca';
  const showTE     = mode === 'overlay' || mode === 'te';

  return (
    <div className={styles.wrapper} aria-label="Portfolio equity chart">
      <ResponsiveContainer width="100%" height="100%">
        <LineChart data={data} margin={{ top: 16, right: 16, bottom: 0, left: 0 }}>
          <defs>
            <filter id={glowId}>
              <feGaussianBlur stdDeviation="3" result="blur" />
              <feMerge>
                <feMergeNode in="blur" />
                <feMergeNode in="SourceGraphic" />
              </feMerge>
            </filter>
          </defs>
          <CartesianGrid stroke={TOKENS.borderMuted} strokeDasharray="none" vertical={false} />
          <XAxis
            dataKey="date"
            tickFormatter={formatDate}
            tick={{ fontFamily: 'var(--font-mono)', fontSize: 10, fill: TOKENS.textFaint }}
            axisLine={false}
            tickLine={false}
          />
          <YAxis
            tickFormatter={fmt$}
            tick={{ fontFamily: 'var(--font-mono)', fontSize: 10, fill: TOKENS.textFaint }}
            axisLine={false}
            tickLine={false}
            width={60}
            orientation="right"
          />
          <Tooltip content={<CustomTooltip />} />
          {showAlpaca && (
            <Line
              type="monotone"
              dataKey="alpaca"
              stroke={TOKENS.textMuted}
              strokeWidth={2}
              dot={false}
              isAnimationActive={false}
              connectNulls
            />
          )}
          {showTE && (
            <Line
              type="monotone"
              dataKey="te"
              stroke={TOKENS.acidGreen}
              strokeWidth={1.5}
              strokeDasharray="5 3"
              dot={false}
              isAnimationActive={false}
              filter={`url(#${glowId})`}
              connectNulls
            />
          )}
        </LineChart>
      </ResponsiveContainer>
    </div>
  );
}
