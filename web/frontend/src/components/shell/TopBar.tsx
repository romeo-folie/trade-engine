import { useEffect, useState } from 'react';
import { colors, typography } from '../../styles/tokens';
import type { ConnectionStatus } from '../../hooks/useEngineSocket';

interface TopBarProps {
  status: ConnectionStatus;
  eventsPerSecond: number;
}

export default function TopBar({ status }: TopBarProps) {
  const [clock, setClock] = useState(() => formatClock());
  const [version, setVersion] = useState<string>('...');

  useEffect(() => {
    const id = setInterval(() => setClock(formatClock()), 1000);
    return () => clearInterval(id);
  }, []);

  useEffect(() => {
    fetch('/api/system/status')
      .then(r => r.json())
      .then((d: { version?: string }) => { if (d.version) setVersion(d.version); })
      .catch(() => setVersion('—'));
  }, []);

  return (
    <header style={{
      height: 40,
      background: colors.canvas,
      borderBottom: '1px solid rgba(255, 255, 255, 0.15)',
      display: 'flex',
      alignItems: 'center',
      padding: '0 20px',
      justifyContent: 'space-between',
      position: 'fixed',
      top: 0,
      left: 0,
      right: 0,
      zIndex: 'var(--z-topbar)',
    }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
        <span style={{ 
          color: colors.acidGreen, 
          fontFamily: typography.mono, 
          fontWeight: 700, 
          fontSize: 14,
          letterSpacing: '0.05em'
        }}>
          TRADEENGINE
        </span>
        <span style={{ 
          color: colors.textDim, 
          fontFamily: typography.mono, 
          fontSize: 11,
          marginTop: 1
        }}>
          v{version}
        </span>
      </div>

      <div style={{ display: 'flex', alignItems: 'center', gap: 20 }}>
        <div style={{ 
          display: 'flex', 
          alignItems: 'center', 
          gap: 8,
          fontSize: 11,
          fontFamily: typography.mono,
        }}>
           <span style={{ color: colors.textDim }}>{clock}</span>
           <div style={{ 
             background: status === 'connected' ? colors.emerald : colors.red,
             color: colors.canvas,
             padding: '1px 6px',
             borderRadius: 2,
             fontSize: 9,
             fontWeight: 700
           }}>
             {status === 'connected' ? 'LIVE' : 'CLOSED'}
           </div>
           <div style={{ 
             width: 6, height: 6, borderRadius: '50%', 
             background: status === 'connected' ? colors.acidGreen : colors.textDim,
             boxShadow: status === 'connected' ? `0 0 6px ${colors.acidGreen}` : 'none'
           }} />
        </div>
      </div>
    </header>
  );
}

function formatClock(): string {
  return new Date().toLocaleTimeString('en-GB', {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
    timeZoneName: 'short',
  });
}
