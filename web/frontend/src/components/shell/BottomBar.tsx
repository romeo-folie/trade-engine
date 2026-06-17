import { useEffect, useRef, useState } from 'react';
import { colors, typography } from '../../styles/tokens';
import type { ConnectionStatus } from '../../hooks/useEngineSocket';

interface BottomBarProps {
  status: ConnectionStatus;
  lastSignalAt: number | null;
  avgLatencyMs: number | null;
  baseUptimeSeconds: number | null;
  nextCycleAt: string | null;
}

function fmtUptime(sec: number): string {
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  return h > 0 ? `${h}h ${m}m` : `${m}m`;
}

// Renders the countdown to the next engine run cycle.
// null target → "—"; already elapsed → "DUE"; otherwise Xh Ym / Ym Zs / Zs.
function fmtCountdown(remainingMs: number | null): string {
  if (remainingMs == null) return '—';
  if (remainingMs <= 0) return 'DUE';
  const sec = Math.floor(remainingMs / 1000);
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
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

export default function BottomBar({ status, lastSignalAt, avgLatencyMs, baseUptimeSeconds, nextCycleAt }: BottomBarProps) {
  const [uptime, setUptime] = useState<number | null>(null);

  // Seed uptime from parent prop and start a 1s local tick.
  // Cleanup clears the interval on unmount or when baseUptimeSeconds changes (reconnect).
  useEffect(() => {
    if (baseUptimeSeconds == null) return;
    setUptime(baseUptimeSeconds);
    const id = setInterval(() => setUptime(u => (u ?? 0) + 1), 1000);
    return () => clearInterval(id);
  }, [baseUptimeSeconds]);

  // Next-cycle countdown. Tracks the target time locally and ticks every second.
  // The parent fetches /api/system/status once on mount, so when the countdown
  // elapses we refetch ourselves to pick up the engine's next scheduled cycle.
  const [targetMs, setTargetMs] = useState<number | null>(null);
  const [remainingMs, setRemainingMs] = useState<number | null>(null);
  const refetchingFor = useRef<number | null>(null);

  useEffect(() => {
    setTargetMs(nextCycleAt ? Date.parse(nextCycleAt) : null);
  }, [nextCycleAt]);

  useEffect(() => {
    if (targetMs == null) {
      setRemainingMs(null);
      return;
    }
    const update = () => setRemainingMs(targetMs - Date.now());
    update();
    const id = setInterval(update, 1000);
    return () => clearInterval(id);
  }, [targetMs]);

  // When the cycle elapses, poll for the engine's next scheduled cycle. The engine
  // only writes the new next_cycle_at *after* its (possibly minutes-long) tick
  // completes, so a single fetch may still return the elapsed timestamp — keep
  // retrying (every ~3s) until a fresh, later timestamp arrives. The guard ensures
  // only one request is in flight per target at a time, never a per-second storm.
  useEffect(() => {
    if (targetMs == null || remainingMs == null || remainingMs > 0) return;
    if (refetchingFor.current === targetMs) return;
    refetchingFor.current = targetMs;
    let cancelled = false;
    const timer = setTimeout(() => {
      fetch('/api/system/status')
        .then(r => (r.ok ? r.json() : null))
        .then((d: { next_cycle_at?: string } | null) => {
          if (cancelled) return;
          const next = d?.next_cycle_at ? Date.parse(d.next_cycle_at) : NaN;
          if (!Number.isNaN(next) && next !== targetMs) {
            setTargetMs(next); // fresh schedule — guard resets via targetMs change
          } else {
            refetchingFor.current = null; // not rescheduled yet — allow a retry
          }
        })
        .catch(() => { if (!cancelled) refetchingFor.current = null; });
    }, 3000);
    return () => { cancelled = true; clearTimeout(timer); };
  }, [targetMs, remainingMs]);

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

      <StatItem label="NEXT CYCLE" value={fmtCountdown(remainingMs)} />

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
