import type { ConnectionStatus } from '../hooks/useEngineSocket';

interface StatusBarProps {
  status: ConnectionStatus;
  eventsPerSecond: number;
  paused: boolean;
  onTogglePause: () => void;
}

const STATUS_COLORS: Record<ConnectionStatus, string> = {
  connected:    '#22c55e',
  connecting:   '#f59e0b',
  reconnecting: '#f59e0b',
  disconnected: '#ef4444',
};

const STATUS_LABELS: Record<ConnectionStatus, string> = {
  connected:    '● Connected',
  connecting:   '○ Connecting…',
  reconnecting: '⟳ Reconnecting…',
  disconnected: '✕ Disconnected',
};

export function StatusBar({ status, eventsPerSecond, paused, onTogglePause }: StatusBarProps) {
  return (
    <header style={{
      display: 'flex', alignItems: 'center', gap: 20,
      padding: '8px 20px',
      background: '#0a0820',
      borderBottom: '1px solid #1e1b35',
      fontFamily: 'monospace', fontSize: 12,
      flexShrink: 0,
    }}>
      <span style={{ color: '#00f5ff', fontWeight: 700, letterSpacing: 2 }}>TRADEENGINE</span>
      <span style={{ color: '#6b6b8f' }}>·</span>
      <span style={{ color: '#a855f7' }}>Live Data Flow</span>

      <div style={{ marginLeft: 'auto', display: 'flex', gap: 16, alignItems: 'center' }}>
        <span style={{ color: '#6b6b8f' }}>
          {eventsPerSecond.toFixed(1)} <span style={{ color: '#4b4b6f' }}>ev/s</span>
        </span>

        <button
          onClick={onTogglePause}
          style={{
            background: paused ? '#2d2b45' : 'transparent',
            border: '1px solid #2d2b45',
            borderRadius: 4,
            color: paused ? '#f59e0b' : '#6b6b8f',
            padding: '3px 10px',
            cursor: 'pointer',
            fontFamily: 'monospace',
            fontSize: 11,
          }}
        >
          {paused ? '▶ Resume' : '⏸ Pause'}
        </button>

        <span style={{ color: STATUS_COLORS[status] }}>
          {STATUS_LABELS[status]}
        </span>
      </div>
    </header>
  );
}
