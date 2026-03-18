/**
 * Boot Screen — terminal-style loading animation shown while the engine
 * WebSocket is in the 'connecting' state.
 *
 * Lines animate in via CSS animation-delay stagger (no JS setTimeout).
 * Transitions to the Shell once socket reaches 'connected'.
 */
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

const TAG_CLASS: Record<string, string> = {
  OK:     styles.tagOk,
  WAIT:   styles.tagWait,
  ERR:    styles.tagErr,
  INFO:   styles.tagInfo,
  SYSTEM: styles.tagSystem,
};

function formatTag(tag: string): string {
  if (tag === 'SYSTEM') { const d = new Date().toISOString().slice(0, 10); return `[${d} SYSTEM]`; }
  return `[ ${tag.padEnd(4)} ]`;
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

  const cursorDelay = `${lines.length * LINE_DELAY_MS}ms`;

  return (
    <div className={styles.screen} role="status" aria-label="System initialising">
      <div className={styles.scanlines} aria-hidden="true" />

      <div className={styles.terminal}>
        {lines.map((line, i) => (
          <BootLine key={i} line={line} index={i} />
        ))}
        <div
          className={styles.line}
          style={{ animationDelay: cursorDelay }}
        >
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
