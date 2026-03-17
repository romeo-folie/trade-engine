/**
 * Pipeline screen — hub-and-spoke live data-flow graph.
 *
 * Replaces the Task 1 stub. Wires useEngineSocket → HubGraph + EventFeed + DrillDownPanel.
 * Socket is owned by Shell; this screen consumes it via props passed from Shell
 * via the EngineSocketContext.
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

  // Build map of nodeId → most recent event type for particle spawning
  const recentEventTypes = useMemo<Record<string, string>>(() => {
    // Rebuild only when events array changes length (new event arrived)
    const result: Record<string, string> = {};
    for (const ev of events) {
      if (!result[ev.source]) result[ev.source] = ev.type;
      if (!result[ev.target]) result[ev.target] = ev.type;
    }
    return result;
  }, [events]);

  return (
    <div className={styles.screen}>
      <HubGraph
        nodes={nodes}
        nodeMetrics={nodeMetrics}
        recentEventTypes={recentEventTypes}
        onNodeClick={handleNodeClick}
      />
      <EventFeed events={events} />
      <DrillDownPanel nodeId={selectedNodeId} onClose={handleClose} />
    </div>
  );
}
