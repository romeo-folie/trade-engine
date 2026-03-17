import { useEffect, useState } from 'react';
import { useEngineSocket } from '../../hooks/useEngineSocket';
import styles from './TopBar.module.css';

export default function TopBar() {
  const { status, eventsPerSecond } = useEngineSocket();
  const [clock, setClock] = useState(() => formatClock());

  useEffect(() => {
    const id = setInterval(() => setClock(formatClock()), 1000);
    return () => clearInterval(id);
  }, []);

  const connected = status === 'connected';

  return (
    <header className={styles.topbar}>
      <div className={styles.left}>
        <span className={styles.wordmark}>[TRADE ENGINE]</span>
      </div>

      <div className={styles.right}>
        <span className={styles.status}>
          <span
            className={`${styles.dot} ${connected ? styles.dotConnected : styles.dotDisconnected}`}
          />
          <span className={connected ? styles.statusConnected : styles.statusDisconnected}>
            {connected ? 'CONNECTED' : 'DISCONNECTED'}
          </span>
        </span>

        <span className={styles.clock}>{clock}</span>

        <span className={styles.rate}>
          {eventsPerSecond.toFixed(1)} evt/s
        </span>
      </div>
    </header>
  );
}

function formatClock(): string {
  return new Date().toLocaleTimeString('en-GB', {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
    timeZoneName: 'short',
  });
}
