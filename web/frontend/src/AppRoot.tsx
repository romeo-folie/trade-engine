/**
 * AppRoot — top-level wrapper that handles the boot sequence.
 *
 * Renders the BootScreen while the engine WebSocket is initialising,
 * then mounts the full Shell+Router once 'connected'.
 *
 * The socket is created here (not inside Shell) so Boot can observe its status.
 */
import { useState } from 'react';
import { RouterProvider } from 'react-router-dom';
import { useEngineSocket } from './hooks/useEngineSocket';
import { EngineSocketContext } from './contexts/EngineSocketContext';
import BootScreen from './screens/Boot';
import { router } from './router';

// VITE_SKIP_BOOT=true bypasses the boot gate for design preview without a running backend.
const SKIP_BOOT = import.meta.env.VITE_SKIP_BOOT === 'true';

export default function AppRoot() {
  const socket = useEngineSocket();

  // The boot screen gates the *first* connection only.
  //
  // It used to gate every 'reconnecting' status too, which unmounted the whole
  // RouterProvider each time the engine restarted — discarding route state, and
  // forcing the remounted router to re-fetch route chunks at exactly the moment
  // the restarted server had swapped in new content hashes. Reconnects are now
  // reported in place by TopBar/BottomBar, which already receive socket.status.
  const [hasConnected, setHasConnected] = useState(false);
  if (!hasConnected && socket.status === 'connected') {
    // Adjusting state during render (rather than in an effect) so the app is
    // never painted with a stale boot screen for one frame.
    setHasConnected(true);
  }

  const booting = !SKIP_BOOT && !hasConnected
    && (socket.status === 'connecting' || socket.status === 'reconnecting');

  if (booting) {
    return <BootScreen status={socket.status} />;
  }

  return (
    <EngineSocketContext.Provider value={socket}>
      <RouterProvider router={router} />
    </EngineSocketContext.Provider>
  );
}
