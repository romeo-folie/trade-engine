/**
 * Boot Screen — terminal-style loading animation shown while the engine
 * WebSocket is in the 'connecting' state.
 *
 * Design reference: .claude/designs/Boot Screen — TradeEngine.png
 * Spec: ~/superteam-ops/outbox/designer-tradeengine-frontend-spec-20260316.md §1
 *
 * Transitions to the Shell (pipeline route) once socket reaches 'connected'.
 */
import { useEffect, useState, useRef } from 'react';
import type { ConnectionStatus } from '../hooks/useEngineSocket';
import styles from './Boot.module.css';

/* ── Boot log sequence ───────────────────────────────────────────────────── */

interface LogLine {
  timestamp: string;
  tag: 'OK' | 'WAIT' | 'ERR' | 'INFO' | 'WARN' | 'HR' | 'TITLE';
  text: string;
}

function ts(offset: number): string {
  const d = new Date(Date.now() - offset);
  return d.toTimeString().slice(0, 8);
}

const BOOT_SEQUENCE: Omit<LogLine, 'timestamp'>[] = [
  { tag: 'TITLE', text: 'TRADE ENGINE v2.0 — SYSTEM INIT' },
  { tag: 'HR',    text: '' },
  { tag: 'INFO',  text: 'loading configuration from config.yaml' },
  { tag: 'OK',    text: 'config validated — 0 errors' },
  { tag: 'INFO',  text: 'initialising database connection pool' },
  { tag: 'OK',    text: 'postgres pool ready (max_conns=20)' },
  { tag: 'INFO',  text: 'connecting to redis' },
  { tag: 'OK',    text: 'redis ready' },
  { tag: 'HR',    text: '' },
  { tag: 'INFO',  text: 'starting market data feed' },
  { tag: 'WAIT',  text: 'waiting for alpaca websocket...' },
  { tag: 'OK',    text: 'market feed active' },
  { tag: 'INFO',  text: 'loading model registry (4 analysts + 1 arbiter)' },
  { tag: 'OK',    text: 'analysts: technical, momentum, sentiment, macro' },
  { tag: 'OK',    text: 'arbiter: ready' },
  { tag: 'HR',    text: '' },
  { tag: 'INFO',  text: 'engine tick interval: 5m' },
  { tag: 'INFO',  text: 'mode: PAPER — no live orders' },
  { tag: 'WAIT',  text: 'establishing engine websocket...' },
];

const LINE_INTERVAL_MS = 90;
const ERROR_LINES: Omit<LogLine, 'timestamp'>[] = [
  { tag: 'ERR', text: 'engine websocket unreachable' },
  { tag: 'WARN', text: 'retrying connection...' },
];

/* ── Component ───────────────────────────────────────────────────────────── */

interface BootScreenProps {
  status: ConnectionStatus;
}

export default function BootScreen({ status }: BootScreenProps) {
  const [visibleLines, setVisibleLines] = useState<LogLine[]>([]);
  const [cursorVisible, setCursorVisible] = useState(true);
  const [showError, setShowError] = useState(false);
  const bottomRef = useRef<HTMLDivElement>(null);
  const lineIndexRef = useRef(0);
  const startTimeRef = useRef(Date.now());

  // Animate boot lines one by one.
  useEffect(() => {
    lineIndexRef.current = 0;
    startTimeRef.current = Date.now();

    const interval = setInterval(() => {
      const idx = lineIndexRef.current;
      if (idx >= BOOT_SEQUENCE.length) {
        clearInterval(interval);
        return;
      }
      const line = BOOT_SEQUENCE[idx];
      const elapsed = Date.now() - startTimeRef.current;
      setVisibleLines(prev => [
        ...prev,
        { ...line, timestamp: ts(elapsed) },
      ]);
      lineIndexRef.current = idx + 1;
    }, LINE_INTERVAL_MS);

    return () => clearInterval(interval);
  }, []);

  // Show error lines if still connecting after all lines rendered.
  useEffect(() => {
    if (status === 'disconnected') {
      setShowError(true);
    }
  }, [status]);

  // Auto-scroll to bottom as lines appear.
  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [visibleLines, showError]);

  // Blinking cursor.
  useEffect(() => {
    const id = setInterval(() => setCursorVisible(v => !v), 500);
    return () => clearInterval(id);
  }, []);

  return (
    <div className={styles.screen} role="status" aria-label="System initialising">
      {/* Optional CRT scanline overlay */}
      <div className={styles.scanlines} aria-hidden="true" />

      <div className={styles.terminal}>
        {visibleLines.map((line, i) => (
          <BootLine key={i} line={line} />
        ))}

        {showError && ERROR_LINES.map((line, i) => (
          <BootLine key={`err-${i}`} line={{ ...line, timestamp: ts(0) }} />
        ))}

        <span
          className={styles.cursor}
          style={{ opacity: cursorVisible ? 1 : 0 }}
          aria-hidden="true"
        >
          █
        </span>
      </div>

      <div ref={bottomRef} />
    </div>
  );
}

function BootLine({ line }: { line: LogLine }) {
  if (line.tag === 'HR') {
    return <div className={styles.hr} aria-hidden="true" />;
  }
  if (line.tag === 'TITLE') {
    return <div className={styles.title}>{line.text}</div>;
  }

  return (
    <div className={styles.line}>
      <span className={styles.timestamp}>{line.timestamp}</span>
      <Tag tag={line.tag} />
      <span className={styles.text}>{line.text}</span>
    </div>
  );
}

function Tag({ tag }: { tag: LogLine['tag'] }) {
  const clsMap: Partial<Record<LogLine['tag'], string>> = {
    OK:   styles.tagOk,
    WAIT: styles.tagWait,
    ERR:  styles.tagErr,
    INFO: styles.tagInfo,
    WARN: styles.tagWarn,
  };
  const labelMap: Partial<Record<LogLine['tag'], string>> = {
    OK:   '[ OK ]',
    WAIT: '[WAIT]',
    ERR:  '[ ERR]',
    INFO: '[INFO]',
    WARN: '[WARN]',
  };
  const cls = clsMap[tag] ?? '';
  const label = labelMap[tag] ?? '';
  return <span className={`${styles.tag} ${cls}`}>{label}</span>;
}
