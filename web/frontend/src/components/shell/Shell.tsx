import { Outlet } from 'react-router-dom';
import { useEngineSocketContext } from '../../contexts/EngineSocketContext';
import TopBar from './TopBar';
import Sidebar from './Sidebar';
import styles from './Shell.module.css';

/**
 * Shell — main application chrome (TopBar + Sidebar + content area).
 *
 * The WebSocket is initialised in AppRoot and published via EngineSocketContext.
 * Shell consumes it here — no duplicate connections.
 */
export default function Shell() {
  const socket = useEngineSocketContext();

  return (
    <div className={styles.shell}>
      <TopBar status={socket.status} eventsPerSecond={socket.eventsPerSecond} />
      <Sidebar />
      <main className={styles.content}>
        <Outlet />
      </main>
    </div>
  );
}
