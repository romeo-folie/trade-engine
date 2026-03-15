import { useState } from 'react';
import { useEngineSocket } from './hooks/useEngineSocket';
import { PipelineGraph } from './components/PipelineGraph';
import { DrillDownPanel } from './components/DrillDownPanel';
import { StatusBar } from './components/StatusBar';

export default function App() {
  const { status, nodes, events, eventsPerSecond, paused, setPaused, nodeMetrics } = useEngineSocket();
  const [selectedNode, setSelectedNode] = useState<string | null>(null);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100vh' }}>
      <StatusBar
        status={status}
        eventsPerSecond={eventsPerSecond}
        paused={paused}
        onTogglePause={() => setPaused(!paused)}
      />

      <div style={{ flex: 1, position: 'relative', overflow: 'hidden' }}>
        <PipelineGraph
          nodes={nodes}
          events={events}
          nodeMetrics={nodeMetrics}
          onNodeClick={id => setSelectedNode(id === selectedNode ? null : id)}
        />
        <DrillDownPanel
          nodeId={selectedNode}
          onClose={() => setSelectedNode(null)}
        />
      </div>
    </div>
  );
}
