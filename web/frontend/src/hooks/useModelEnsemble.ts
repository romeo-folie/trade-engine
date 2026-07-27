import { useMemo } from 'react';
import useSWR from 'swr';
import { jsonFetcher } from './swr';

export interface ModelNode {
  id: string;
  provider: string;
  modelName: string;
  totalSignals: number;
  accuracy: number;
}

interface ModelsResponse {
  model_performance?: Array<Record<string, unknown>>;
}

const EMPTY_MODELS: ModelNode[] = [];

/**
 * useModelEnsemble — per-model performance used to enrich analyst cards.
 *
 * Uses SWR rather than a raw fetch inside useEffect: SWR dedupes across the
 * several components that read this endpoint, drops the response when the
 * component unmounts mid-flight (the previous version could setState after
 * unmount), and revalidates on focus instead of keeping whatever was true at
 * mount for the lifetime of the tab.
 */
export function useModelEnsemble(): ModelNode[] {
  const { data } = useSWR<ModelsResponse>('/api/node/models', jsonFetcher);

  // Memoised on SWR's stable `data` identity. Callers use the result as a
  // useMemo dependency (HubGraph builds the whole node set from it), so a fresh
  // array on every render would defeat their memoisation.
  return useMemo(() => {
    const perf = data?.model_performance;
    if (!perf || perf.length === 0) return EMPTY_MODELS;
    return perf.map(m => ({
      id: `model-${m.Provider}-${m.Model}`.replace(/[^a-z0-9-]/gi, '-').toLowerCase(),
      provider: String(m.Provider || ''),
      modelName: String(m.Model || ''),
      totalSignals: Number(m.TotalRecommendations) || 0,
      accuracy: Number(m.WinRate) || 0,
    }));
  }, [data]);
}
