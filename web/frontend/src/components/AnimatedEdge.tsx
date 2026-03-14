import { memo } from 'react';
import { BaseEdge, getBezierPath, type EdgeProps } from '@xyflow/react';
import { motion } from 'framer-motion';

interface AnimatedEdgeData {
  active?: boolean;
  [key: string]: unknown;
}

export const AnimatedEdge = memo(function AnimatedEdge({
  id,
  sourceX, sourceY, targetX, targetY,
  sourcePosition, targetPosition,
  data,
  style,
}: EdgeProps) {
  const [edgePath] = getBezierPath({ sourceX, sourceY, sourcePosition, targetX, targetY, targetPosition });
  const isActive = (data as AnimatedEdgeData | undefined)?.active ?? false;

  return (
    <>
      <BaseEdge id={id} path={edgePath} style={style} />
      {isActive && (
        <motion.circle
          r={4}
          fill="#00f5ff"
          filter="url(#glow)"
          style={{ offsetPath: `path("${edgePath}")` } as React.CSSProperties}
          animate={{ offsetDistance: ['0%', '100%'] }}
          transition={{ duration: 0.9, ease: 'linear', repeat: Infinity }}
        />
      )}
      <defs>
        <filter id="glow">
          <feGaussianBlur stdDeviation="2" result="coloredBlur" />
          <feMerge>
            <feMergeNode in="coloredBlur" />
            <feMergeNode in="SourceGraphic" />
          </feMerge>
        </filter>
      </defs>
    </>
  );
});
