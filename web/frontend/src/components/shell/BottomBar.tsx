import { useEffect, useState } from 'react';
import { colors, typography } from '../../styles/tokens';
import type { ConnectionStatus } from '../../hooks/useEngineSocket';

interface BottomBarProps {
  status: ConnectionStatus;
  lastSignalAt: number | null;
  avgLatencyMs: number | null;
}

function fmtUptime(sec: number): string {
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  return h > 0 ? `${h}h ${m}m` : `${m}m`;
}

function fmtSignalTime(ts: number | null): string {
  if (!ts) return '—';
  return new Date(ts).toLocaleTimeString('en-GB', {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  });
}

function StatItem({ label, value, color }: { label: string; value: string; color?: string }) {
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
      <span style={{ color: colors.textFaint, letterSpacing: '0.02em' }}>{label}</span>
      <span style={{ color: color ?? colors.textDim, letterSpacing: '0.02em' }}>{value}</span>
    </div>
  );
}

function Divider() {
  return <span style={{ color: colors.textFaint }}>|</span>;
}

export default function BottomBar({ status, lastSignalAt, avgLatencyMs }: BottomBarProps) {
  const [uptime, setUptime] = useState<number | null>(null);

  // Fetch uptime from API on mount, refresh every 60s
  useEffect(() => {
    const fetchUptime = () => {
      fetch('/api/system/status')
        .then(r => r.json())
        .then((d: { uptime_seconds?: number }) => {
          if (d.uptime_seconds != null) setUptime(d.uptime_seconds);
        })
        .catch(() => {});
    };
    fetchUptime();
    const id = setInterval(fetchUptime, 60_000);
    return () => clearInterval(id);
  }, []);

  // Increment locally every second so the display ticks without waiting 60s
  useEffect(() => {
    if (uptime === null) return;
    const id = setInterval(() => setUptime(u => (u ?? 0) + 1), 1000);
    return () => clearInterval(id);
  }, [uptime !== null]); // eslint-disable-line react-hooks/exhaustive-deps

  const isPipelineActive = status === 'connected';

  return (
    <footer style={{
      height: 32,
      background: colors.canvas,
      borderTop: '1px solid rgba(255, 255, 255, 0.15)',
      display: 'flex',
      alignItems: 'center',
      padding: '0 16px',
      gap: 16,
      fontFamily: typography.mono,
      fontSize: 11,
      flexShrink: 0,
      zIndex: 100,
    }}>
      {/* PIPELINE ACTIVE / OFFLINE */}
      <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
        <div style={{
          width: 6,
          height: 6,
          borderRadius: '50%',
          background: isPipelineActive ? colors.acidGreen : colors.textDim,
          boxShadow: isPipelineActive ? `0 0 4px ${colors.acidGreen}` : 'none',
        }} />
        <span style={{
          color: isPipelineActive ? colors.acidGreen : colors.textDim,
          letterSpacing: '0.02em',
        }}>
          {isPipelineActive ? 'PIPELINE ACTIVE' : 'OFFLINE'}
        </span>
      </div>

      <Divider />

      <StatItem label="LAST SIGNAL" value={fmtSignalTime(lastSignalAt)} />

      <Divider />

      <StatItem label="UPTIME" value={uptime !== null ? fmtUptime(uptime) : '—'} />

      <Divider />

      <StatItem
        label="LATENCY"
        value={avgLatencyMs !== null ? `${avgLatencyMs}ms` : '—'}
        color={avgLatencyMs !== null ? colors.emerald : colors.textDim}
      />
    </footer>
  );
}
