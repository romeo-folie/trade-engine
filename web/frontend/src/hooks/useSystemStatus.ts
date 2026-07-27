import { useMemo } from 'react';
import useSWR from 'swr';
import { jsonFetcher } from './swr';

export interface SystemStatus {
  version: string;
  mode: string;
  uptimeSeconds: number;
  /** Next engine run-cycle time as an ISO string, or null if not scheduled. */
  nextCycleAt: string | null;
}

interface StatusResponse {
  version?: string;
  mode?: string;
  uptime_seconds?: number;
  next_cycle_at?: string;
}

/**
 * useSystemStatus — engine version, mode, uptime and next-cycle time.
 *
 * On SWR so the several call sites (Shell, HubGraph) share one request rather
 * than each running its own fetch-in-useEffect. The refresh interval matters:
 * next_cycle_at drives a live countdown, which previously froze at whatever
 * value happened to be current when the component mounted and never resynced.
 * SWR's own retry/backoff also replaces the hand-rolled single 3s retry that
 * covered a backend still starting up.
 */
export function useSystemStatus(): SystemStatus | null {
  const { data } = useSWR<StatusResponse>('/api/system/status', jsonFetcher, {
    refreshInterval: 30_000,
  });

  // Stable identity while `data` is unchanged — consumers pass fields of this
  // object into effect dependency lists.
  return useMemo(() => {
    if (!data) return null;
    return {
      version: data.version ?? '—',
      mode: data.mode ?? '—',
      uptimeSeconds: data.uptime_seconds ?? 0,
      nextCycleAt: data.next_cycle_at || null,
    };
  }, [data]);
}
