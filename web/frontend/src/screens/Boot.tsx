import { useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import { useEngineSocket } from '../hooks/useEngineSocket';
import styles from './Boot.module.css';

/* ── Boot log content ─────────────────────────────────────────────── */

interface LogLine {
  tag?: 'OK' | 'INFO' | 'WAIT' | 'ERR' | 'WARN';
  timestamp?: string;
  message: string;
  type: 'title' | 'hr' | 'log' | 'blank';
}

const BOOT_LINES: LogLine[] = [
  { type: 'title', message: 'TRADEENGINE v2.4.1-alpha' },
  { type: 'hr', message: '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━' },
  { type: 'log', tag: 'INFO', timestamp: '2026-03-18 14:30:01', message: 'Initialising engine subsystems...' },
  { type: 'log', tag: 'INFO', timestamp: '2026-03-18 14:30:01', message: 'Loading configuration from config.yaml' },
  { type: 'log', tag: 'OK',   timestamp: '2026-03-18 14:30:02', message: 'Risk manager loaded — max_drawdown: <v>8%</v>, max_open: <v>5</v>' },
  { type: 'log', tag: 'OK',   timestamp: '2026-03-18 14:30:02', message: 'Database connection established — <v>127.0.0.1:5432</v>' },
  { type: 'log', tag: 'OK',   timestamp: '2026-03-18 14:30:02', message: 'Redis connection established — <v>127.0.0.1:6379</v>' },
  { type: 'log', tag: 'WAIT', timestamp: '2026-03-18 14:30:03', message: 'Connecting to market data feed...' },
  { type: 'log', tag: 'OK',   timestamp: '2026-03-18 14:30:04', message: 'Market data feed connected — polygon.io' },
  { type: 'log', tag: 'INFO', timestamp: '2026-03-18 14:30:04', message: 'Loading model registry...' },
  { type: 'log', tag: 'OK',   timestamp: '2026-03-18 14:30:05', message: 'OpenAI API — gpt-4o [ready]' },
  { type: 'log', tag: 'OK',   timestamp: '2026-03-18 14:30:05', message: 'Anthropic API — claude-sonnet [ready]' },
  { type: 'log', tag: 'OK',   timestamp: '2026-03-18 14:30:06', message: 'Ensemble engine ready — <v>3</v> analysts, <v>1</v> arbiter' },
  { type: 'log', tag: 'INFO', timestamp: '2026-03-18 14:30:06', message: 'Starting WebSocket server on <v>:8080</v>...' },
  { type: 'log', tag: 'OK',   timestamp: '2026-03-18 14:30:07', message: 'TradeEngine is live.' },
  { type: 'hr', message: '━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━' },
  { type: 'blank', message: '' },
];

const TAG_STYLE: Record<string, string> = {
  OK:   styles.tagOk,
  INFO: styles.tagInfo,
  WAIT: styles.tagWait,
  ERR:  styles.tagErr,
  WARN: styles.tagWarn,
};

const TAG_LABEL: Record<string, string> = {
  OK:   '[ OK ]',
  INFO: '[INFO]',
  WAIT: '[WAIT]',
  ERR:  '[ ERR]',
  WARN: '[WARN]',
};

/* ── Helpers ──────────────────────────────────────────────────────── */

/** Parse <v>...</v> tokens to highlight numeric values inline. */
function renderMessage(raw: string) {
  const parts = raw.split(/(<v>.*?<\/v>)/g);
  return parts.map((part, i) => {
    const m = part.match(/^<v>(.*)<\/v>$/);
    if (m) return <span key={i} className={styles.numericValue}>{m[1]}</span>;
    return <span key={i} className={styles.message}>{part}</span>;
  });
}

/* ── Component ────────────────────────────────────────────────────── */

export default function BootScreen() {
  const navigate = useNavigate();
  const { status } = useEngineSocket();

  useEffect(() => {
    if (status === 'connected') {
      navigate('/pipeline', { replace: true });
    }
  }, [status, navigate]);

  return (
    <div className={styles.boot}>
      <div className={styles.column}>
        {BOOT_LINES.map((line, i) => {
          const delay = `${i * 80}ms`;

          if (line.type === 'title') {
            return (
              <div key={i} className={styles.line} style={{ animationDelay: delay }}>
                <span className={styles.title}>{line.message}</span>
              </div>
            );
          }

          if (line.type === 'hr') {
            return (
              <div key={i} className={styles.line} style={{ animationDelay: delay }}>
                <span className={styles.hr}>{line.message}</span>
              </div>
            );
          }

          if (line.type === 'blank') {
            return <div key={i} className={styles.line} style={{ animationDelay: delay }}>&nbsp;</div>;
          }

          // log line
          return (
            <div key={i} className={styles.line} style={{ animationDelay: delay }}>
              <span className={styles.timestamp}>{line.timestamp} </span>
              <span className={TAG_STYLE[line.tag!]}>{TAG_LABEL[line.tag!]}</span>
              {' '}
              {renderMessage(line.message)}
            </div>
          );
        })}

        {/* Prompt line */}
        <div
          className={styles.line}
          style={{ animationDelay: `${BOOT_LINES.length * 80}ms` }}
        >
          <span className={styles.prompt}>TRADEENGINE {'>'} </span>
          <span className={styles.cursor} />
        </div>
      </div>
    </div>
  );
}
