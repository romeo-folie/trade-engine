import { Outlet } from 'react-router-dom';
import { useEngineSocketContext } from '../../contexts/EngineSocketContext';
import TopBar from './TopBar';
import Sidebar from './Sidebar';
import BottomBar from './BottomBar';
import { colors } from '../../styles/tokens';

export default function Shell() {
  const socket = useEngineSocketContext();

  return (
    <div style={{
      display: 'flex',
      flexDirection: 'column',
      height: '100vh',
      background: colors.canvas,
      overflow: 'hidden',
    }}>
      <TopBar status={socket.status} eventsPerSecond={socket.eventsPerSecond} />
      
      <div style={{ 
        display: 'flex', 
        flex: 1, 
        minHeight: 0,
        /* The "outer border" frame as requested — it includes the Sidebar area */
        borderLeft: '1px solid rgba(255, 255, 255, 0.15)',
        borderRight: '1px solid rgba(255, 255, 255, 0.15)',
      }}>
        <Sidebar />
        <main style={{ 
          flex: 1, 
          marginLeft: 48, 
          marginTop: 40, 
          position: 'relative',
          display: 'flex',
          flexDirection: 'column',
          minWidth: 0,
        }}>
          <Outlet />
        </main>
      </div>

      <BottomBar />
    </div>
  );
}
