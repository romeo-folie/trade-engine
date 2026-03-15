import { useEffect, useState } from 'react';
import { motion, AnimatePresence } from 'framer-motion';

interface DrillDownPanelProps {
  nodeId: string | null;
  onClose: () => void;
}

export function DrillDownPanel({ nodeId, onClose }: DrillDownPanelProps) {
  const [data, setData] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!nodeId) { setData(null); return; }
    setLoading(true);
    setError(null);
    fetchNodeData(nodeId)
      .then(d => { setData(JSON.stringify(d, null, 2)); setLoading(false); })
      .catch(e => { setError(String(e)); setLoading(false); });
  }, [nodeId]);

  return (
    <AnimatePresence>
      {nodeId && (
        <motion.aside
          key="panel"
          initial={{ x: '100%', opacity: 0 }}
          animate={{ x: 0, opacity: 1 }}
          exit={{ x: '100%', opacity: 0 }}
          transition={{ type: 'spring', stiffness: 300, damping: 30 }}
          style={{
            position: 'absolute', right: 0, top: 0, bottom: 0,
            width: 360, background: '#0e0c2a',
            borderLeft: '1px solid #2d2b45',
            padding: '20px 16px',
            overflowY: 'auto',
            fontFamily: 'monospace',
            zIndex: 10,
          }}
        >
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
            <span style={{ color: '#00f5ff', fontSize: 13, fontWeight: 700 }}>{nodeId}</span>
            <button onClick={onClose} style={{ background: 'none', border: 'none', color: '#6b6b8f', cursor: 'pointer', fontSize: 18 }}>×</button>
          </div>

          {loading && <div style={{ color: '#4b4b6f', fontSize: 12 }}>Loading…</div>}
          {error && <div style={{ color: '#ef4444', fontSize: 12 }}>{error}</div>}
          {!loading && !error && data && (
            <pre style={{ color: '#e2e0ff', fontSize: 11, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
              {data}
            </pre>
          )}
          {!loading && !error && !data && (
            <div style={{ color: '#4b4b6f', fontSize: 12 }}>No data available.</div>
          )}
        </motion.aside>
      )}
    </AnimatePresence>
  );
}

async function fetchNodeData(nodeId: string): Promise<unknown> {
  const endpointMap: Record<string, string> = {
    risk_manager:   '/api/node/risk',
    portfolio:      '/api/node/portfolio',
    llm_analysis:   '/api/node/models',
    executor:       '/api/trades/recent?limit=20',
    blocked_signals: '/api/signals/recent?limit=20',
    approvals:      '/api/signals/recent?limit=20',
    consensus:      '/api/node/models',
  };
  const url = endpointMap[nodeId] ?? `/api/node/ticker/${nodeId}`;
  const res = await fetch(url);
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return res.json();
}
