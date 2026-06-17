import { useEffect, useState } from 'react';
import { colors, typography } from '../../styles/tokens';
import type { ConnectionStatus } from '../../hooks/useEngineSocket';

interface BottomBarProps {
  status: ConnectionStatus;
  lastSignalAt: number | null;
  avgLatencyMs: number | null;
  baseUptimeSeconds: number | null;
  nextCycleAt: string | null;
}

// Tolerance for browser-vs-server clock skew when accepting a next-cycle timestamp.
const CLOCK_SKEW_MS = 10_000;

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
  const [targetMs, setTargetMs] = useState<number | null>(null);
  const [remainingMs, setRemainingMs] = useState<number | null>(null);

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

  // Poll /api/system/status for the next-cycle timestamp whenever we lack a usable
  // one. useSystemStatus only fetches once on mount, so the bar polls on its own in
  // two cases: (1) at startup before the engine has written next_cycle_at — the web
  // server can start before the engine's first schedule, and a startup tick can take
  // minutes; (2) after the current cycle elapses — the engine writes the next
  // schedule only once its (possibly minutes-long) tick finishes, so an immediate
  // fetch may still return the elapsed timestamp. Keyed on a stable boolean so it
  // does not restart every second; the chained setTimeout caps requests at ~1/5s.
  const needTarget = targetMs == null || (remainingMs != null && remainingMs <= 0);
  useEffect(() => {
    if (!needTarget) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    const poll = () => {
      fetch('/api/system/status')
        .then(r => (r.ok ? r.json() : null))
        .then((d: { next_cycle_at?: string } | null) => {
          if (cancelled) return;
          const next = d?.next_cycle_at ? Date.parse(d.next_cycle_at) : NaN;
          // Accept a different timestamp that isn't clearly stale. The skew window
          // tolerates a browser clock lagging the server's (a genuinely-future
          // next_cycle_at can otherwise read as just-past here); `next !== targetMs`
          // is what actually rejects the current elapsed target being re-returned.
          if (!Number.isNaN(next) && next > Date.now() - CLOCK_SKEW_MS && next !== targetMs) {
            setTargetMs(next); // fresh schedule — effect stops polling
          } else if (!cancelled) {
            timer = setTimeout(poll, 5000); // still unknown/elapsed — keep trying
          }
        })
        .catch(() => { if (!cancelled) timer = setTimeout(poll, 5000); });
    };
    // Small initial delay; give a known-elapsed target a moment to reschedule.
    timer = setTimeout(poll, targetMs == null ? 1000 : 3000);
    return () => { cancelled = true; clearTimeout(timer); };
  }, [needTarget, targetMs]);

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
