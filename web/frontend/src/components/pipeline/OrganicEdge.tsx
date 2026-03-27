import { BaseEdge, getBezierPath, type EdgeProps } from '@xyflow/react';
import { colors } from '../../styles/tokens';

export default function OrganicEdge({
  id,
  sourceX,
  sourceY,
  targetX,
  targetY,
  sourcePosition,
  targetPosition,
  style = {},
  markerEnd,
  data,
}: EdgeProps) {
  const [edgePath] = getBezierPath({
    sourceX,
    sourceY,
    sourcePosition,
    targetX,
    targetY,
    targetPosition,
    curvature: 0.8, // Increased tension for that "loose" organic flow
  });

  const isActive = data?.active === true;
  const strokeColor = style.stroke || (isActive ? colors.acidGreen : colors.textFaint);
  const opacity = style.opacity || (isActive ? 1 : 0.4);

  return (
    <>
      {/* Background Glow (Outer) */}
      {isActive && (
        <path
          id={`${id}-glow-outer`}
          d={edgePath}
          fill="none"
          stroke={colors.acidGreen}
          strokeWidth={4}
          strokeOpacity={0.15}
          style={{ filter: 'blur(6px)' }}
        />
      )}
      
      {/* Mid Glow (Inner) */}
      {isActive && (
        <path
          id={`${id}-glow-inner`}
          d={edgePath}
          fill="none"
          stroke={colors.acidGreen}
          strokeWidth={2}
          strokeOpacity={0.3}
          style={{ filter: 'blur(3px)' }}
        />
      )}

      {/* Main Path */}
      <BaseEdge
        id={id}
        path={edgePath}
        markerEnd={markerEnd}
        style={{
          ...style,
          stroke: strokeColor,
          strokeWidth: 1.5,
          opacity: opacity,
          transition: 'stroke 0.3s, stroke-width 0.3s, opacity 0.3s',
        }}
      />
    </>
  );
}
