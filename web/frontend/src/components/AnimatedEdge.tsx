import { memo, type CSSProperties } from 'react';
import { BaseEdge, getBezierPath, type EdgeProps } from '@xyflow/react';
import { motion, AnimatePresence } from 'framer-motion';
import { colors } from '../styles/tokens';

export interface ParticleInfo {
  id: string;
  color: string;
  timestamp: number;
}

interface AnimatedEdgeData {
  active?: boolean;
  particles?: ParticleInfo[];
  edgeColor?: string;
  [key: string]: unknown;
}

/**
 * GlowDefs renders the shared SVG <defs> for edge glow and particle bloom.
 * Mount once at the top level.
 */
export function GlowDefs() {
  return (
    <svg style={{ position: 'absolute', width: 0, height: 0 }}>
      <defs>
        <filter id="edge-glow">
          <feGaussianBlur stdDeviation="2" result="coloredBlur" />
          <feMerge>
            <feMergeNode in="coloredBlur" />
            <feMergeNode in="SourceGraphic" />
          </feMerge>
        </filter>
        <filter id="particle-bloom">
          <feGaussianBlur stdDeviation="3" result="blur" />
          <feMerge>
            <feMergeNode in="blur" />
            <feMergeNode in="blur" />
            <feMergeNode in="SourceGraphic" />
          </feMerge>
        </filter>
      </defs>
    </svg>
  );
}

export const AnimatedEdge = memo(function AnimatedEdge({
  id,
  sourceX, sourceY, targetX, targetY,
  sourcePosition, targetPosition,
  data,
  style,
}: EdgeProps) {
  const [edgePath] = getBezierPath({ sourceX, sourceY, sourcePosition, targetX, targetY, targetPosition });
  const edgeData = data as AnimatedEdgeData | undefined;
  const isActive = edgeData?.active ?? false;
  const particles = edgeData?.particles ?? [];
  const edgeColor = edgeData?.edgeColor ?? (isActive ? '#00f5ff' : '#2d2b45');

  return (
    <>
      <BaseEdge id={id} path={edgePath} style={{
        ...style,
        stroke: isActive ? colors.acidGreen : colors.borderMuted,
        strokeWidth: 1,
        strokeDasharray: isActive ? '5 5' : 'none',
        animation: isActive ? 'edge-dash 0.5s linear infinite' : 'none',
        filter: isActive ? 'url(#edge-glow)' : undefined,
        transition: 'stroke 0.3s',
      }} />

      {/* Global CSS for the dash animation would be needed in global.css or similar */}

      {/* Color-coded particles flowing along the edge */}
      <AnimatePresence>
        {particles.map((p, i) => (
          <motion.circle
            key={p.id}
            r={isActive ? 5 : 4}
            fill={p.color}
            filter="url(#particle-bloom)"
            style={{ offsetPath: `path("${edgePath}")` } as CSSProperties}
            initial={{ offsetDistance: '0%', opacity: 0 }}
            animate={{ offsetDistance: '100%', opacity: [0, 1, 1, 0] }}
            exit={{ opacity: 0 }}
            transition={{
              duration: 1.2,
              delay: i * 0.15,
              ease: 'easeInOut',
            }}
          />
        ))}
      </AnimatePresence>

      {/* Persistent dim particle for active edges (ambient flow feel) */}
      {isActive && particles.length === 0 && (
        <motion.circle
          r={3}
          fill={edgeColor}
          opacity={0.4}
          filter="url(#edge-glow)"
          style={{ offsetPath: `path("${edgePath}")` } as CSSProperties}
          animate={{ offsetDistance: ['0%', '100%'] }}
          transition={{ duration: 2, ease: 'linear', repeat: Infinity }}
        />
      )}
    </>
  );
});
