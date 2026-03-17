import { Outlet } from 'react-router-dom';
import { useEngineSocket } from '../../hooks/useEngineSocket';
import TopBar from './TopBar';
import Sidebar from './Sidebar';
import styles from './Shell.module.css';

export default function Shell() {
  const { status, eventsPerSecond } = useEngineSocket();

  return (
    <div className={styles.shell}>
      <TopBar status={status} eventsPerSecond={eventsPerSecond} />
      <Sidebar />
      <main className={styles.content}>
        <Outlet />
      </main>
    </div>
  );
}
