import { colors, typography } from '../../styles/tokens';

interface ConsensusItem {
  label: string;
  value: number; // 0-1
  color: string;
}

interface EnsembleConsensusProps {
  items: ConsensusItem[];
}

export default function EnsembleConsensus({ items }: EnsembleConsensusProps) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 4, width: '100%' }}>
      {items.map((item, i) => (
        <div key={i} style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <div style={{ 
            flex: 1, 
            height: 3, 
            background: colors.borderMuted, 
            borderRadius: 1,
            position: 'relative',
            overflow: 'hidden'
          }}>
            <div style={{
              position: 'absolute',
              left: 0,
              top: 0,
              bottom: 0,
              width: `${item.value * 100}%`,
              background: item.color,
              boxShadow: `0 0 4px ${item.color}66`
            }} />
          </div>
          <span style={{ 
            fontFamily: typography.mono, 
            fontSize: 8, 
            color: colors.textDim,
            width: 24,
            textAlign: 'right'
          }}>
            {Math.round(item.value * 100)}%
          </span>
        </div>
      ))}
    </div>
  );
}
