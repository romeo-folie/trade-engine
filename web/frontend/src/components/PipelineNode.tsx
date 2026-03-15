import { memo, useState } from 'react';
import { Handle, Position, type NodeProps } from '@xyflow/react';
import { motion, AnimatePresence } from 'framer-motion';
import type { NodeState } from '../types/protocol';
import type { NodeMetrics } from '../hooks/useEngineSocket';

interface PipelineNodeData {
  nodeState: NodeState;
  isActive: boolean;
  metrics?: NodeMetrics;
  [key: string]: unknown;
}

const KIND_COLORS: Record<string, { border: string; glow: string; accent: string }> = {
  source:     { border: '#00b4ff', glow: '0 0 12px #00b4ff44', accent: '#00b4ff' },
  processing: { border: '#a855f7', glow: '0 0 12px #a855f744', accent: '#a855f7' },
  output:     { border: '#22c55e', glow: '0 0 12px #22c55e44', accent: '#22c55e' },
};

export const PipelineNode = memo(function PipelineNode({ data }: NodeProps) {
  const { nodeState, isActive, metrics } = data as PipelineNodeData;
  const colors = KIND_COLORS[nodeState.kind] ?? KIND_COLORS.processing;
  const [hovered, setHovered] = useState(false);

  const eventCount = metrics?.eventCount ?? nodeState.event_count;
  const lastEventAt = metrics?.lastEventAt
    ? new Date(metrics.lastEventAt).toLocaleTimeString()
    : nodeState.last_event_at
      ? new Date(nodeState.last_event_at).toLocaleTimeString()
      : null;

  return (
    <motion.div
      onMouseEnter={() => setHovered(true)}
      onMouseLeave={() => setHovered(false)}
      animate={isActive
        ? { boxShadow: [colors.glow, `0 0 24px ${colors.border}66`, colors.glow] }
        : { boxShadow: '0 0 0px transparent' }
      }
      transition={isActive ? { duration: 1.2, repeat: Infinity, ease: 'easeInOut' } : {}}
      style={{
        background: isActive
          ? 'linear-gradient(135deg, #16133e 0%, #1a1648 100%)'
          : '#12103a',
        border: `1.5px solid ${isActive ? colors.border : '#2d2b45'}`,
        borderRadius: 10,
        padding: '12px 18px',
        minWidth: 180,
        cursor: 'pointer',
        transition: 'border-color 0.3s, background 0.3s',
        position: 'relative',
      }}
    >
      <Handle type="target" position={Position.Left} style={{ background: colors.border, width: 8, height: 8 }} />

      {/* Active indicator dot */}
      {isActive && (
        <motion.div
          animate={{ opacity: [1, 0.3, 1] }}
          transition={{ duration: 1.5, repeat: Infinity }}
          style={{
            position: 'absolute', top: 8, right: 10,
            width: 6, height: 6, borderRadius: '50%',
            background: colors.accent,
            boxShadow: `0 0 6px ${colors.accent}`,
          }}
        />
      )}

      <div style={{
        color: '#e2e0ff',
        fontFamily: '"JetBrains Mono", "Fira Code", monospace',
        fontSize: 12,
        fontWeight: 600,
        letterSpacing: '0.02em',
      }}>
        {nodeState.label}
      </div>

      <div style={{
        display: 'flex', gap: 8, alignItems: 'center',
        marginTop: 5,
        fontFamily: '"JetBrains Mono", "Fira Code", monospace',
        fontSize: 10,
      }}>
        <span style={{ color: '#6b6b8f', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
          {nodeState.kind}
        </span>
        {eventCount > 0 && (
          <span style={{ color: colors.accent, fontWeight: 500 }}>
            {eventCount.toLocaleString()} ev
          </span>
        )}
      </div>

      {/* Hover tooltip with metadata */}
      <AnimatePresence>
        {hovered && (
          <motion.div
            initial={{ opacity: 0, y: 4 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: 4 }}
            transition={{ duration: 0.15 }}
            style={{
              position: 'absolute',
              bottom: 'calc(100% + 8px)',
              left: '50%',
              transform: 'translateX(-50%)',
              background: '#1a1848',
              border: '1px solid #2d2b55',
              borderRadius: 6,
              padding: '8px 12px',
              minWidth: 160,
              zIndex: 20,
              fontFamily: '"JetBrains Mono", "Fira Code", monospace',
              fontSize: 10,
              boxShadow: '0 4px 20px rgba(0,0,0,0.5)',
              pointerEvents: 'none',
            }}
          >
            <div style={{ color: '#e2e0ff', fontWeight: 600, marginBottom: 4 }}>{nodeState.label}</div>
            <div style={{ color: '#6b6b8f' }}>Events: {eventCount.toLocaleString()}</div>
            {lastEventAt && (
              <div style={{ color: '#6b6b8f' }}>Last: {lastEventAt}</div>
            )}
            {nodeState.avg_latency_ms != null && nodeState.avg_latency_ms > 0 && (
              <div style={{ color: '#6b6b8f' }}>Avg latency: {nodeState.avg_latency_ms.toFixed(1)}ms</div>
            )}
          </motion.div>
        )}
      </AnimatePresence>

      <Handle type="source" position={Position.Right} style={{ background: colors.border, width: 8, height: 8 }} />
    </motion.div>
  );
});
