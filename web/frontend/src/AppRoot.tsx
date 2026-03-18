/**
 * AppRoot — top-level wrapper that handles the boot sequence.
 *
 * Renders the BootScreen while the engine WebSocket is initialising,
 * then mounts the full Shell+Router once 'connected'.
 *
 * The socket is created here (not inside Shell) so Boot can observe its status.
 */
import { RouterProvider } from 'react-router-dom';
import { useEngineSocket } from './hooks/useEngineSocket';
import { EngineSocketContext } from './contexts/EngineSocketContext';
import BootScreen from './screens/Boot';
import { router } from './router';

export default function AppRoot() {
  const socket = useEngineSocket();

  // Show boot screen until first successful connection.
  const booting = socket.status === 'connecting' || socket.status === 'reconnecting';

  if (booting) {
    return <BootScreen status={socket.status} />;
  }

  return (
    <EngineSocketContext.Provider value={socket}>
      <RouterProvider router={router} />
    </EngineSocketContext.Provider>
  );
}
