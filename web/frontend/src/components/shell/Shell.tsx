import { Outlet } from 'react-router-dom';
import TopBar from './TopBar';
import Sidebar from './Sidebar';
import styles from './Shell.module.css';

export default function Shell() {
  return (
    <div className={styles.shell}>
      <TopBar />
      <Sidebar />
      <main className={styles.content}>
        <Outlet />
      </main>
    </div>
  );
}
