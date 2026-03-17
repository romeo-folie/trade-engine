/**
 * Pipeline screen — hub-and-spoke live data-flow graph.
 *
 * Replaces the Task 1 stub. Wires useEngineSocket → HubGraph + EventFeed + DrillDownPanel.
 * Socket is owned by Shell; this screen consumes it via useEngineSocketContext()
 * (context provider/consumer pattern).
 *
 * Task 3 of the web redesign (closes #203).
 */
import { useState, useCallback, useMemo } from 'react';
import { useEngineSocketContext } from '../contexts/EngineSocketContext';
import HubGraph from '../components/pipeline/HubGraph';
import EventFeed from '../components/pipeline/EventFeed';
import DrillDownPanel from '../components/pipeline/DrillDownPanel';
import styles from './Pipeline.module.css';

export default function PipelineScreen() {
  const { nodes, events, nodeMetrics } = useEngineSocketContext();
  const [selectedNodeId, setSelectedNodeId] = useState<string | null>(null);

  const handleNodeClick = useCallback((id: string) => {
    setSelectedNodeId(prev => prev === id ? null : id);
  }, []);

  const handleClose = useCallback(() => setSelectedNodeId(null), []);

  // Build map of nodeId → event signature for particle spawning.
  // Uses type + timestamp so back-to-back events of the same type still spawn particles.
  // Build map of nodeId → event signature for particle spawning.
  // Uses _seqId (monotonic) so back-to-back events with identical timestamps still spawn particles.
  const recentEventSignatures = useMemo<Record<string, { type: string; ts: number }>>(() => {
    const result: Record<string, { type: string; ts: number }> = {};
    for (const ev of events) {
      if (ev.source && !result[ev.source]) result[ev.source] = { type: ev.type, ts: ev._seqId };
      if (ev.target && !result[ev.target]) result[ev.target] = { type: ev.type, ts: ev._seqId };
    }
    return result;
  }, [events]);

  return (
    <div className={styles.screen}>
      <HubGraph
        nodes={nodes}
        nodeMetrics={nodeMetrics}
        recentEventSignatures={recentEventSignatures}
        onNodeClick={handleNodeClick}
      />
      <EventFeed events={events} />
      <DrillDownPanel nodeId={selectedNodeId} onClose={handleClose} />
    </div>
  );
}
