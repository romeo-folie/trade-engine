/**
 * DailyCountersCard — surfaces today's signal-pipeline observability counters
 * (Issue #325): how many signals were generated, blocked, and executed today,
 * which gates blocked the most signals, and how many of the executed trades
 * have closed in profit vs loss so far.
 *
 * Reads /api/portfolio/daily-counters?days=1 every 5 seconds. The endpoint
 * synthesizes today's row live from the signals + trades tables when the
 * daily-summary writer hasn't fired yet, so this card never sits at zero on
 * engine restart.
 */
import { useEffect, useState } from 'react';
import { TOKENS } from '../../styles/tokens';

interface DailyCounter {
  date: string;
  signals_total: number;
  signals_blocked: number;
  signals_executed: number;
  executed_winning: number;
  executed_losing: number;
  blocked_by_reason: Record<string, number> | null;
}

interface CountersResponse {
  counters: DailyCounter[];
}

// Block reasons can be long strings like "confidence 0.55 < threshold 0.65 (R:R 1.40)" —
// truncate to keep the row width predictable. Tooltip carries the full string.
function shortReason(r: string, max = 38): string {
  if (r.length <= max) return r;
  return r.slice(0, max - 1) + '…';
}

function topBlockReasons(map: Record<string, number> | null, n: number): { reason: string; count: number }[] {
  if (!map) return [];
  return Object.entries(map)
    .map(([reason, count]) => ({ reason, count }))
    .sort((a, b) => b.count - a.count)
    .slice(0, n);
}

export default function DailyCountersCard() {
  const [today, setToday] = useState<DailyCounter | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    async function load() {
      try {
        const res = await fetch('/api/portfolio/daily-counters?days=1');
        if (!res.ok) {
          if (!cancelled) setError(`HTTP ${res.status}`);
          return;
        }
        const data: CountersResponse = await res.json();
        if (cancelled) return;
        // Newest day is last in the array (asc by date in the handler);
        // synthesized today is appended last when no row exists yet.
        const last = data.counters?.[data.counters.length - 1] ?? null;
        setToday(last);
        setError(null);
      } catch (err) {
        if (!cancelled && err instanceof Error) {
          setError(err.message);
        }
      }
    }
    load();
    const interval = setInterval(load, 5_000);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, []);

  const total = today?.signals_total ?? 0;
  const blocked = today?.signals_blocked ?? 0;
  const executed = today?.signals_executed ?? 0;
  const winning = today?.executed_winning ?? 0;
  const losing = today?.executed_losing ?? 0;
  const closed = winning + losing;
  const winRate = closed > 0 ? (winning / closed) * 100 : null;

  const topReasons = topBlockReasons(today?.blocked_by_reason ?? null, 3);

  return (
    <div
      style={{
        border: `1px solid ${TOKENS.border}`,
        borderRadius: 4,
        padding: 'var(--space-3)',
      }}
    >
      <div
        style={{
          fontFamily: 'var(--font-mono)',
          fontSize: 'var(--font-size-xs)',
          textTransform: 'uppercase',
          letterSpacing: '0.08em',
          color: TOKENS.textDim,
          marginBottom: 'var(--space-3)',
        }}
      >
        TODAY&apos;S SIGNAL PIPELINE
      </div>

      {error && !today && (
        <div style={{ color: TOKENS.textFaint, fontFamily: 'var(--font-mono)', fontSize: 12 }}>
          {`Failed to load (${error})`}
        </div>
      )}

      {/* Signal counts row: total / blocked / executed */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 8, marginBottom: 12 }}>
        <Metric label="GENERATED" value={total} color={TOKENS.textPrimary} />
        <Metric label="BLOCKED" value={blocked} color={TOKENS.amber} />
        <Metric label="EXECUTED" value={executed} color={TOKENS.acidGreen} />
      </div>

      {/* Win / loss row only when there's at least one closed trade today */}
      {closed > 0 && (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 8, marginBottom: 12 }}>
          <Metric label="WINNING" value={winning} color={TOKENS.emerald} />
          <Metric label="LOSING" value={losing} color={TOKENS.red} />
          <Metric
            label="WIN RATE"
            value={winRate != null ? `${winRate.toFixed(0)}%` : '—'}
            color={winRate != null && winRate >= 50 ? TOKENS.emerald : TOKENS.amber}
          />
        </div>
      )}

      {/* Top-3 block reasons. Hidden when nothing is blocked. */}
      {topReasons.length > 0 && (
        <div>
          <div
            style={{
              fontFamily: 'var(--font-mono)',
              fontSize: 10,
              letterSpacing: '0.08em',
              color: TOKENS.textFaint,
              marginBottom: 4,
            }}
          >
            TOP BLOCK REASONS
          </div>
          {topReasons.map(({ reason, count }) => (
            <div
              key={reason}
              title={reason}
              style={{
                display: 'flex',
                justifyContent: 'space-between',
                fontFamily: 'var(--font-mono)',
                fontSize: 12,
                color: TOKENS.textMuted,
                padding: '2px 0',
              }}
            >
              <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                {shortReason(reason)}
              </span>
              <span style={{ color: TOKENS.amber, marginLeft: 8, fontVariantNumeric: 'tabular-nums' }}>
                {count}
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function Metric({ label, value, color }: { label: string; value: number | string; color: string }) {
  return (
    <div>
      <div
        style={{
          fontFamily: 'var(--font-mono)',
          fontSize: 10,
          letterSpacing: '0.08em',
          color: TOKENS.textFaint,
          marginBottom: 2,
        }}
      >
        {label}
      </div>
      <div
        style={{
          fontFamily: 'var(--font-mono)',
          fontSize: 18,
          fontWeight: 700,
          color,
          fontVariantNumeric: 'tabular-nums',
        }}
      >
        {value}
      </div>
    </div>
  );
}
