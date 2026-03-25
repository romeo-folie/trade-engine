import { useEffect, useState } from 'react';

export interface SystemStatus {
  version: string;
  mode: string;
  uptimeSeconds: number;
}

/**
 * useSystemStatus — fetches /api/system/status once on mount.
 * Shared by TopBar and BottomBar to avoid duplicate requests.
 */
export function useSystemStatus(): SystemStatus | null {
  const [status, setStatus] = useState<SystemStatus | null>(null);

  useEffect(() => {
    fetch('/api/system/status')
      .then(r => r.json())
      .then((d: { version?: string; mode?: string; uptime_seconds?: number }) => {
        setStatus({
          version: d.version ?? '—',
          mode: d.mode ?? '—',
          uptimeSeconds: d.uptime_seconds ?? 0,
        });
      })
      .catch(() => {});
  }, []);

  return status;
}
