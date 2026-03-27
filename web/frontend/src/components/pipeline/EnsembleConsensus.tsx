/**
 * EnsembleConsensus — sequential bar reveal for the consensus pipeline card.
 *
 * Bars appear one-at-a-time as each analyst WS result arrives (Option A).
 * Each bar animates from 0 → value when it first mounts, conveying that
 * analysis is still running while incomplete.
 *
 * When items is empty (no analysts reported yet), shows a single idle bar
 * so the card doesn't collapse.
 */
import { useEffect, useRef, useState } from 'react';
import { colors, typography } from '../../styles/tokens';

interface ConsensusItem {
  label: string;
  value?: number; // 0–1, optional if loading
  color: string;
  isLoading?: boolean;
}

interface EnsembleConsensusProps {
  items: ConsensusItem[];
}

/** Single bar that animates from 0 → target width on mount / value change. */
function AnimatedBar({ item, index }: { item: ConsensusItem; index: number }) {
  const [width, setWidth] = useState(0);
  const prevValue = useRef(-1);

  useEffect(() => {
    if (item.value === undefined || item.value === prevValue.current) return;
    prevValue.current = item.value;
    const id = setTimeout(() => setWidth(item.value! * 100), 30 + index * 60);
    return () => clearTimeout(id);
  }, [item.value, index]);

  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
      <span style={{
        fontFamily: typography.mono,
        fontSize: 8,
        color: item.color,
        width: 28,
        flexShrink: 0,
        letterSpacing: '0.04em',
      }}>
        {item.label}
      </span>
      <div style={{
        flex: 1,
        height: 3,
        background: colors.borderMuted,
        borderRadius: 1,
        position: 'relative',
        overflow: 'hidden',
      }}>
        {item.isLoading ? (
          <div 
            className="consensus-pulse"
            style={{
              position: 'absolute',
              height: '100%',
              width: '40%',
              background: `linear-gradient(90deg, transparent, ${item.color}44, transparent)`,
              left: '-40%',
            }}
          />
        ) : (
          <div style={{
            position: 'absolute',
            left: 0,
            top: 0,
            bottom: 0,
            width: `${width}%`,
            background: item.color,
            boxShadow: `0 0 4px ${item.color}66`,
            transition: 'width 0.6s cubic-bezier(0.16, 1, 0.3, 1)',
          }} />
        )}
      </div>
      <span style={{
        fontFamily: typography.mono,
        fontSize: 8,
        color: item.isLoading ? colors.textFaint : colors.textDim,
        width: 24,
        textAlign: 'right',
        flexShrink: 0,
        opacity: item.isLoading ? 0.3 : 1,
      }}>
        {item.isLoading ? '...' : `${Math.round((item.value || 0) * 100)}%`}
      </span>
    </div>
  );
}

export default function EnsembleConsensus({ items }: EnsembleConsensusProps) {
  if (items.length === 0) {
    // Nothing reported yet — show a single idle placeholder
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <span style={{ fontFamily: typography.mono, fontSize: 8, color: colors.textFaint, width: 28 }}>—</span>
        <div style={{
          flex: 1, height: 3,
          background: colors.borderMuted,
          borderRadius: 1,
          overflow: 'hidden',
        }}>
          <div style={{
            height: '100%', width: 0,
            background: colors.borderMuted,
          }} />
        </div>
        <span style={{ fontFamily: typography.mono, fontSize: 8, color: colors.textFaint, width: 24, textAlign: 'right' }}>
          —
        </span>
      </div>
    );
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 5, width: '100%' }}>
      {items.map((item, i) => (
        <AnimatedBar key={item.label} item={item} index={i} />
      ))}
    </div>
  );
}
