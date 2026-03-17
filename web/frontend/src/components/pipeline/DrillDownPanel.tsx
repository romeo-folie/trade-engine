/**
 * DrillDownPanel — right-edge slide-in panel for node inspection.
 *
 * Opens when a node is clicked. Shows live node state from REST API.
 * Design spec §2: 320px wide, #0D0D11 bg, chamfer top-left only.
 */
import { useEffect, useState, memo } from 'react';
import { AnimatePresence, motion } from 'framer-motion';
import styles from './DrillDownPanel.module.css';

interface DrillDownPanelProps {
  nodeId: string | null;
  onClose: () => void;
}

interface NodeDetail {
  id: string;
  label: string;
  kind: string;
  event_count: number;
  last_event_at?: string;
  avg_latency_ms?: number;
  meta?: Record<string, unknown>;
}

async function fetchNodeDetail(nodeId: string, signal?: AbortSignal): Promise<NodeDetail | null> {
  try {
    const resp = await fetch(`/api/nodes/${encodeURIComponent(nodeId)}`, { signal });
    if (!resp.ok) return null;
    return await resp.json() as NodeDetail;
  } catch {
    return null;
  }
}

function DrillDownPanel({ nodeId, onClose }: DrillDownPanelProps) {
  const [detail, setDetail] = useState<NodeDetail | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!nodeId) { setDetail(null); return; }

    const ac = new AbortController();
    setLoading(true);
    fetchNodeDetail(nodeId, ac.signal).then(d => {
      if (ac.signal.aborted) return;
      setDetail(d);
      setLoading(false);
    });

    // Re-fetch every 5s while open
    const iv = setInterval(() => {
      fetchNodeDetail(nodeId, ac.signal).then(d => {
        if (ac.signal.aborted) return;
        setDetail(d);
      });
    }, 5000);

    return () => { ac.abort(); clearInterval(iv); };
  }, [nodeId]);

  return (
    <AnimatePresence>
      {nodeId && (
        <motion.aside
          key="drilldown"
          className={styles.panel}
          initial={{ x: '100%' }}
          animate={{ x: 0 }}
          exit={{ x: '100%' }}
          transition={{ type: 'tween', duration: 0.22, ease: 'easeOut' }}
          aria-label={`Node detail: ${detail?.label ?? nodeId}`}
        >
          <div className={styles.header}>
            <span className={styles.nodeLabel}>{detail?.label ?? nodeId}</span>
            <button
              className={styles.close}
              onClick={onClose}
              aria-label="Close panel"
            >×</button>
          </div>

          <div className={styles.body}>
            {loading && !detail && (
              <p className={styles.muted}>Loading…</p>
            )}

            {detail && (
              <>
                <Row label="KIND"         value={detail.kind.toUpperCase()} />
                <Row label="EVENTS"       value={detail.event_count.toLocaleString()} />
                {detail.avg_latency_ms != null && (
                  <Row label="AVG LATENCY" value={`${detail.avg_latency_ms.toFixed(1)}ms`} />
                )}
                {detail.last_event_at && (
                  <Row label="LAST EVENT"  value={formatTs(detail.last_event_at)} />
                )}

                {detail.meta && Object.keys(detail.meta).length > 0 && (
                  <>
                    <div className={styles.divider} />
                    <div className={styles.sectionLabel}>META</div>
                    {Object.entries(detail.meta).map(([k, v]) => (
                      <Row key={k} label={k.toUpperCase()} value={String(v)} />
                    ))}
                  </>
                )}
              </>
            )}

            {!loading && !detail && (
              <p className={styles.muted}>No data available.</p>
            )}
          </div>

          <div className={styles.footer}>
            <button className={styles.footerBtn} onClick={onClose}>
              CLOSE
            </button>
          </div>
        </motion.aside>
      )}
    </AnimatePresence>
  );
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className={styles.row}>
      <span className={styles.rowLabel}>{label}</span>
      <span className={styles.rowValue}>{value}</span>
    </div>
  );
}

function formatTs(iso: string): string {
  try {
    return new Date(iso).toLocaleTimeString('en-GB', {
      hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false,
    });
  } catch {
    return '--:--:--';
  }
}

export default memo(DrillDownPanel);
