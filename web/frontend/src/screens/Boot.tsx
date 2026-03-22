/**
 * Boot Screen — terminal-style loading animation shown while the engine
 * WebSocket is in the 'connecting' state.
 *
 * Lines animate in via CSS animation-delay stagger (no JS setTimeout).
 * After the static boot lines finish, a "systems nominal" summary appears,
 * followed by timestamped live log entries that append one-by-one via
 * useEffect + useState at 400ms intervals. A blinking cursor trails the
 * last rendered line.
 *
 * Transitions to the Shell once socket reaches 'connected'.
 */
import { useEffect, useState } from 'react';
import type { ConnectionStatus } from '../hooks/useEngineSocket';
import styles from './Boot.module.css';

/* ── Boot log sequence ───────────────────────────────────────────────────── */

interface LogLine {
  tag?: 'OK' | 'WAIT' | 'ERR' | 'INFO' | 'SYSTEM';
  text: string;
  kind?: 'title' | 'separator';
}

const BOOT_LINES: LogLine[] = [
  { tag: 'SYSTEM', text: 'TradeEngine v2.0.0 — initialising', kind: 'title' },
  { text: '───────────────────────────────────────────────', kind: 'separator' },
  { tag: 'OK',   text: 'kernel modules loaded' },
  { tag: 'OK',   text: 'config validated' },
  { tag: 'OK',   text: 'database connection established' },
  { tag: 'WAIT', text: 'connecting to broker API...' },
  { tag: 'OK',   text: 'risk parameters loaded' },
  { tag: 'OK',   text: 'strategy registry: 3 strategies active' },
  { tag: 'WAIT', text: 'establishing live data feed...' },
  { tag: 'OK',   text: 'market data feed — online' },
  { tag: 'OK',   text: 'signal processor — ready' },
  { tag: 'WAIT', text: 'connecting to engine socket...' },
];

const LINE_DELAY_MS = 80;

/** Simulated rolling log entries shown after the "systems nominal" line. */
const LIVE_LOG_ENTRIES: { level: 'INFO' | 'WARN'; message: string }[] = [
  { level: 'INFO', message: 'Arbiter: routing to TECHNICAL, MOMENTUM' },
  { level: 'INFO', message: 'Analyst/Technical: signal=LONG confidence=0.84' },
  { level: 'INFO', message: 'Analyst/Momentum: signal=LONG confidence=0.79' },
  { level: 'INFO', message: 'Analyst/Sentiment: signal=NEUTRAL confidence=0.61' },
  { level: 'WARN', message: 'Analyst/Macro: bypassed (regime=BYPASS)' },
  { level: 'INFO', message: 'Synthesis: LONG consensus=0.81 weighted' },
  { level: 'INFO', message: 'Execution: ARMED — awaiting trigger' },
];

const TAG_CLASS: Record<string, string> = {
  OK:     styles.tagOk,
  WAIT:   styles.tagWait,
  ERR:    styles.tagErr,
  INFO:   styles.tagInfo,
  SYSTEM: styles.tagSystem,
};

function formatTag(tag: string): string {
  if (tag === 'SYSTEM') {
    const d = new Date().toISOString().slice(0, 10);
    return `[${d} SYSTEM]`;
  }
  return `[ ${tag.padEnd(4)} ]`;
}

function nowTimestamp(): string {
  return new Date().toISOString().slice(0, 19).replace('T', ' ');
}

/* ── Component ───────────────────────────────────────────────────────────── */

interface BootScreenProps {
  status: ConnectionStatus;
}

export default function BootScreen({ status }: BootScreenProps) {
  const showError = status === 'reconnecting';

  const lines: LogLine[] = showError
    ? [
        ...BOOT_LINES,
        { tag: 'ERR', text: 'engine socket lost — reconnecting...' },
      ]
    : BOOT_LINES;

  // Delay before the "systems nominal" block appears (after all boot lines)
  const nominalDelayMs = lines.length * LINE_DELAY_MS + 400;

  // Live log entries — appended one by one at 400ms intervals
  const [visibleLogs, setVisibleLogs] = useState<
    { level: 'INFO' | 'WARN'; message: string; ts: string }[]
  >([]);

  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    let count = 0;
    const startDelay = nominalDelayMs + 600;

    timer = setTimeout(function tick() {
      if (cancelled || count >= LIVE_LOG_ENTRIES.length) return;
      const entry = LIVE_LOG_ENTRIES[count];
      setVisibleLogs((prev) => [
        ...prev,
        { ...entry, ts: nowTimestamp() },
      ]);
      count++;
      if (count < LIVE_LOG_ENTRIES.length) {
        timer = setTimeout(tick, 400);
      }
    }, startDelay);

    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [nominalDelayMs]);

  const nominalDelay = `${nominalDelayMs}ms`;
  const separatorDelay = `${nominalDelayMs - 100}ms`;

  return (
    <div className={styles.screen} role="status" aria-label="System initialising">
      <div className={styles.scanlines} aria-hidden="true" />

      <div className={styles.terminal}>
        {/* Static boot lines */}
        {lines.map((line, i) => (
          <BootLine key={i} line={line} index={i} />
        ))}

        {/* ── Summary block ── */}
        <div
          className={`${styles.line} ${styles.separator}`}
          style={{ animationDelay: separatorDelay }}
        >
          ───────────────────────────────────────────────
        </div>
        <div
          className={`${styles.line} ${styles.nominal}`}
          style={{ animationDelay: nominalDelay }}
        >
          All systems nominal. TradeEngine standing by.
        </div>

        {/* ── Live log entries ── */}
        {visibleLogs.map((entry, i) => (
          <div key={i} className={styles.logEntry}>
            <span className={styles.logTimestamp}>{entry.ts}</span>{' '}
            <span className={entry.level === 'WARN' ? styles.tagWait : styles.tagInfo}>
              [{entry.level.padEnd(4)}]
            </span>
            {'  '}
            <span className={styles.logMessage}>{entry.message}</span>
          </div>
        ))}

        {/* Blinking cursor */}
        <div className={styles.cursorLine}>
          <span className={styles.cursor} aria-hidden="true">▋</span>
        </div>
      </div>
    </div>
  );
}

function BootLine({ line, index }: { line: LogLine; index: number }) {
  const delay = `${index * LINE_DELAY_MS}ms`;

  if (line.kind === 'title') {
    return (
      <div
        className={`${styles.line} ${styles.title}`}
        style={{ animationDelay: delay }}
      >
        {line.tag && (
          <span className={TAG_CLASS[line.tag]}>{formatTag(line.tag)}</span>
        )}{' '}
        {line.text}
      </div>
    );
  }

  if (line.kind === 'separator') {
    return (
      <div
        className={`${styles.line} ${styles.separator}`}
        style={{ animationDelay: delay }}
      >
        {line.text}
      </div>
    );
  }

  return (
    <div className={styles.line} style={{ animationDelay: delay }}>
      {line.tag && (
        <span className={TAG_CLASS[line.tag]}>{formatTag(line.tag)}</span>
      )}
      {'   '}
      <span className={styles.text}>{line.text}</span>
    </div>
  );
}
