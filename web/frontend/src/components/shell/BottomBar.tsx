import { colors, typography } from '../../styles/tokens';

export default function BottomBar() {
  const stats = [
    { label: 'PIPELINE ACTIVE', color: colors.acidGreen },
    { label: 'LAST SIGNAL: 14:32:01', color: colors.textDim },
    { label: 'UPTIME: 47h 12m', color: colors.textDim },
    { label: 'LATENCY: 12ms', color: colors.emerald },
  ];

  return (
    <footer style={{
      height: 32,
      background: colors.canvas,
      borderTop: '1px solid rgba(255, 255, 255, 0.15)',
      display: 'flex',
      alignItems: 'center',
      padding: '0 16px',
      gap: 24,
      fontFamily: typography.mono,
      fontSize: 11,
      flexShrink: 0,
      zIndex: 100,
    }}>
      {stats.map((s, i) => (
        <div key={i} style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
          {s.label === 'PIPELINE ACTIVE' && (
            <div style={{ width: 6, height: 6, borderRadius: '50%', background: s.color, boxShadow: `0 0 4px ${s.color}` }} />
          )}
          <span style={{ color: s.color, letterSpacing: '0.02em' }}>{s.label}</span>
          {i < stats.length - 1 && <span style={{ color: colors.textFaint, marginLeft: 12 }}>|</span>}
        </div>
      ))}
    </footer>
  );
}
