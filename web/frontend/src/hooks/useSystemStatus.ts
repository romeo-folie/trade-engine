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
    let cancelled = false;

    const fetchStatus = (attempt: number) => {
      fetch('/api/system/status')
        .then(r => r.json())
        .then((d: { version?: string; mode?: string; uptime_seconds?: number }) => {
          if (cancelled) return;
          setStatus({
            version: d.version ?? '—',
            mode: d.mode ?? '—',
            uptimeSeconds: d.uptime_seconds ?? 0,
          });
        })
        .catch(() => {
          if (cancelled || attempt >= 1) return;
          // Retry once after 3s on first failure (backend may still be starting up)
          setTimeout(() => fetchStatus(attempt + 1), 3000);
        });
    };

    fetchStatus(0);
    return () => { cancelled = true; };
  }, []);

  return status;
}
