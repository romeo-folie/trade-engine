import { useEffect, useState } from 'react';
import type { ConnectionStatus } from '../../hooks/useEngineSocket';
import styles from './TopBar.module.css';

interface TopBarProps {
  status: ConnectionStatus;
  eventsPerSecond: number;
}

export default function TopBar({ status, eventsPerSecond }: TopBarProps) {
  const [clock, setClock] = useState(() => formatClock());

  useEffect(() => {
    const id = setInterval(() => setClock(formatClock()), 1000);
    return () => clearInterval(id);
  }, []);

  const dotClass = STATUS_DOT[status];
  const textClass = STATUS_TEXT[status];
  const label = STATUS_LABEL[status];

  return (
    <header className={styles.topbar}>
      <div className={styles.left}>
        <span className={styles.wordmark}>[TRADE ENGINE]</span>
      </div>

      <div className={styles.right}>
        <span className={styles.status}>
          <span className={`${styles.dot} ${dotClass}`} />
          <span className={textClass}>{label}</span>
        </span>

        <span className={styles.clock}>{clock}</span>

        <span className={styles.rate}>
          {eventsPerSecond.toFixed(1)} evt/s
        </span>
      </div>
    </header>
  );
}

const STATUS_DOT: Record<ConnectionStatus, string> = {
  connecting: styles.dotIntermediate,
  reconnecting: styles.dotIntermediate,
  connected: styles.dotConnected,
  disconnected: styles.dotDisconnected,
};

const STATUS_TEXT: Record<ConnectionStatus, string> = {
  connecting: styles.statusIntermediate,
  reconnecting: styles.statusIntermediate,
  connected: styles.statusConnected,
  disconnected: styles.statusDisconnected,
};

const STATUS_LABEL: Record<ConnectionStatus, string> = {
  connecting: 'CONNECTING\u2026',
  reconnecting: 'RECONNECTING\u2026',
  connected: 'CONNECTED',
  disconnected: 'DISCONNECTED',
};

function formatClock(): string {
  return new Date().toLocaleTimeString('en-GB', {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
    timeZoneName: 'short',
  });
}
