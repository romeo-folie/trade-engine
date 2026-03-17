import { useMemo } from 'react';
import {
  AreaChart,
  Area,
  XAxis,
  YAxis,
  CartesianGrid,
  ResponsiveContainer,
  ReferenceLine,
  ReferenceDot,
} from 'recharts';
import { TOKENS } from '../../styles/tokens';
import styles from './EquityCurve.module.css';

interface DataPoint {
  date: string;
  value: number;
}

interface EquityCurveProps {
  data: DataPoint[];
}

export default function EquityCurve({ data }: EquityCurveProps) {
  const { peakValue, lastPoint } = useMemo(() => {
    if (data.length === 0) return { peakValue: 0, lastPoint: null };
    let peak = data[0];
    for (const d of data) {
      if (d.value > peak.value) peak = d;
    }
    return {
      peakValue: peak.value,
      lastPoint: data[data.length - 1],
    };
  }, [data]);

  const formatDate = (d: string) => {
    try {
      return new Date(d).toLocaleDateString('en-US', { month: 'short', day: '2-digit' }).toUpperCase();
    } catch {
      return d;
    }
  };

  const formatValue = (v: number) => `$${(v / 1000).toFixed(0)}K`;

  /* SVG glow filter for the area line */
  const glowFilterId = 'equity-glow';

  return (
    <div className={styles.wrapper}>
      {peakValue > 0 && (
        <span className={styles.peakLabel}>PEAK</span>
      )}
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={data} margin={{ top: 16, right: 16, bottom: 0, left: 0 }}>
          <defs>
            <filter id={glowFilterId}>
              <feGaussianBlur stdDeviation="3" result="blur" />
              <feMerge>
                <feMergeNode in="blur" />
                <feMergeNode in="SourceGraphic" />
              </feMerge>
            </filter>
          </defs>
          <CartesianGrid
            stroke={TOKENS.borderMuted}
            strokeDasharray="none"
            vertical={false}
          />
          <XAxis
            dataKey="date"
            tickFormatter={formatDate}
            tick={{
              fontFamily: 'var(--font-mono)',
              fontSize: 10,
              fill: TOKENS.textFaint,
            }}
            axisLine={false}
            tickLine={false}
          />
          <YAxis
            tickFormatter={formatValue}
            tick={{
              fontFamily: 'var(--font-mono)',
              fontSize: 10,
              fill: TOKENS.textFaint,
            }}
            axisLine={false}
            tickLine={false}
            width={60}
            orientation="right"
          />
          <Area
            type="monotone"
            dataKey="value"
            stroke={TOKENS.acidGreen}
            strokeWidth={2}
            fill="rgba(57, 255, 20, 0.08)"
            filter={`url(#${glowFilterId})`}
            isAnimationActive={false}
          />
          {peakValue > 0 && (
            <ReferenceLine
              y={peakValue}
              stroke={TOKENS.textFaint}
              strokeDasharray="4 4"
              label={{
                value: `$${peakValue.toLocaleString()}`,
                position: 'right',
                fill: TOKENS.textFaint,
                fontSize: 10,
                fontFamily: 'var(--font-mono)',
              }}
            />
          )}
          {lastPoint && (
            <ReferenceDot
              x={lastPoint.date}
              y={lastPoint.value}
              r={4}
              fill={TOKENS.acidGreen}
              stroke={TOKENS.acidGreen}
              strokeWidth={2}
            />
          )}
        </AreaChart>
      </ResponsiveContainer>
    </div>
  );
}
