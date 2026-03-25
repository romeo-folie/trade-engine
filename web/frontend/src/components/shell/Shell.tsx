import { Outlet } from 'react-router-dom';
import { useEngineSocketContext } from '../../contexts/EngineSocketContext';
import { useSystemStatus } from '../../hooks/useSystemStatus';
import TopBar from './TopBar';
import Sidebar from './Sidebar';
import BottomBar from './BottomBar';
import { colors } from '../../styles/tokens';

export default function Shell() {
  const socket = useEngineSocketContext();
  // Single fetch for both TopBar (version) and BottomBar (uptime) — avoids duplicate requests
  const systemStatus = useSystemStatus();

  return (
    <div style={{
      display: 'flex',
      flexDirection: 'column',
      height: '100vh',
      background: colors.canvas,
      overflow: 'hidden',
    }}>
      <TopBar status={socket.status} eventsPerSecond={socket.eventsPerSecond} version={systemStatus?.version ?? '...'} />
      
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

      <BottomBar
        status={socket.status}
        lastSignalAt={socket.lastSignalAt}
        avgLatencyMs={socket.avgLatencyMs}
        baseUptimeSeconds={systemStatus?.uptimeSeconds ?? null}
      />
    </div>
  );
}
