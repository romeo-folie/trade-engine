/**
 * DrillDownPanel — right-edge slide-in panel for node inspection.
 *
 * Opens when a node is clicked. Displays live node state from the
 * EngineSocketContext (snapshot nodes + incremental nodeMetrics) —
 * no separate REST fetch required.
 * Design spec §2: 320px wide, #0D0D11 bg, chamfer top-left only.
 */
import { memo, useMemo } from 'react';
import { AnimatePresence, motion } from 'framer-motion';
import { useEngineSocketContext } from '../../contexts/EngineSocketContext';
import styles from './DrillDownPanel.module.css';

interface DrillDownPanelProps {
  nodeId: string | null;
  onClose: () => void;
}

function DrillDownPanel({ nodeId, onClose }: DrillDownPanelProps) {
  const { nodes, nodeMetrics } = useEngineSocketContext();

  const detail = useMemo(() => {
    if (!nodeId) return null;
    const node = nodes.find(n => n.id === nodeId);
    if (!node) return null;
    const metrics = nodeMetrics[nodeId] ?? null;
    return {
      id: node.id,
      label: node.label,
      kind: node.kind,
      event_count: node.event_count,
      last_event_at: metrics?.lastEventAt
        ? new Date(metrics.lastEventAt).toISOString()
        : (node.last_event_at ?? null),
      avg_latency_ms: node.avg_latency_ms ?? null,
      meta: node.meta ?? null,
    };
  }, [nodeId, nodes, nodeMetrics]);

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

            {!detail && (
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
