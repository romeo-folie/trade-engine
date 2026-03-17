import { Outlet } from 'react-router-dom';
import { useEngineSocket } from '../../hooks/useEngineSocket';
import { EngineSocketContext } from '../../contexts/EngineSocketContext';
import TopBar from './TopBar';
import Sidebar from './Sidebar';
import styles from './Shell.module.css';

export default function Shell() {
  const socket = useEngineSocket();

  return (
    <EngineSocketContext.Provider value={socket}>
      <div className={styles.shell}>
        <TopBar status={socket.status} eventsPerSecond={socket.eventsPerSecond} />
        <Sidebar />
        <main className={styles.content}>
          <Outlet />
        </main>
      </div>
    </EngineSocketContext.Provider>
  );
}
