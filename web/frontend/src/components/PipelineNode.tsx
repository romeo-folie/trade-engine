import { memo } from 'react';
import { Handle, Position, type NodeProps } from '@xyflow/react';
import { motion } from 'framer-motion';
import type { NodeState } from '../types/protocol';

interface PipelineNodeData {
  nodeState: NodeState;
  isActive: boolean;
  [key: string]: unknown;
}

const KIND_COLORS = {
  source:     { border: '#00b4ff', glow: '0 0 12px #00b4ff44' },
  processing: { border: '#a855f7', glow: '0 0 12px #a855f744' },
  output:     { border: '#22c55e', glow: '0 0 12px #22c55e44' },
};

export const PipelineNode = memo(function PipelineNode({ data }: NodeProps) {
  const { nodeState, isActive } = data as PipelineNodeData;
  const colors = KIND_COLORS[nodeState.kind] ?? KIND_COLORS.processing;

  return (
    <motion.div
      animate={isActive
        ? { boxShadow: [colors.glow, '0 0 20px ' + colors.border + '88', colors.glow] }
        : { boxShadow: '0 0 0px transparent' }
      }
      transition={isActive ? { duration: 0.8, repeat: Infinity } : {}}
      style={{
        background: '#12103a',
        border: `1.5px solid ${isActive ? colors.border : '#2d2b45'}`,
        borderRadius: 8,
        padding: '10px 16px',
        minWidth: 160,
        cursor: 'pointer',
        transition: 'border-color 0.3s',
      }}
    >
      <Handle type="target" position={Position.Left} style={{ background: colors.border }} />

      <div style={{ color: '#e2e0ff', fontFamily: 'monospace', fontSize: 12, fontWeight: 600 }}>
        {nodeState.label}
      </div>
      <div style={{ color: '#6b6b8f', fontFamily: 'monospace', fontSize: 10, marginTop: 4 }}>
        {nodeState.kind}
        {nodeState.event_count > 0 && ` · ${nodeState.event_count} events`}
      </div>
      {nodeState.last_event_at && (
        <div style={{ color: '#4b4b6f', fontFamily: 'monospace', fontSize: 9, marginTop: 2 }}>
          last: {new Date(nodeState.last_event_at).toLocaleTimeString()}
        </div>
      )}

      <Handle type="source" position={Position.Right} style={{ background: colors.border }} />
    </motion.div>
  );
});
