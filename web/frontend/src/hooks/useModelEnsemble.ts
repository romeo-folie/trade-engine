import { useState, useEffect } from 'react';

export interface ModelNode {
  id: string;
  provider: string;
  modelName: string;
  totalSignals: number;
  accuracy: number;
}

export function useModelEnsemble(): ModelNode[] {
  const [models, setModels] = useState<ModelNode[]>([]);

  useEffect(() => {
    fetch('/api/node/models')
      .then(r => { if (!r.ok) throw new Error(r.statusText); return r.json(); })
      .then(d => {
        const perf = d.model_performance || [];
        setModels(perf.map((m: Record<string, unknown>) => ({
          id: `model-${m.Provider}-${m.Model}`.replace(/[^a-z0-9-]/gi, '-').toLowerCase(),
          provider: String(m.Provider || ''),
          modelName: String(m.Model || ''),
          totalSignals: Number(m.TotalRecommendations) || 0,
          accuracy: Number(m.WinRate) || 0,
        })));
      })
      .catch(() => {});
  }, []);

  return models;
}
