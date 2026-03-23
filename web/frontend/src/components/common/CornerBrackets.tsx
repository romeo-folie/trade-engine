import React from 'react';
import { typography } from '../../styles/tokens';

interface CornerBracketsProps {
  color: string;
  opacity?: number;
  children: React.ReactNode;
  active?: boolean;
}

/**
 * CornerBrackets component as per DESIGN_SPEC.md:
 * "The ⌐ (top-left) and ¬ (bottom-right) bracket pair is used to frame card content
 * as a type/direction indicator. This replaces left color border strips entirely."
 */
export function CornerBrackets({ color, opacity = 0.6, children, active = false }: CornerBracketsProps) {
  const finalOpacity = active ? 1 : opacity;
  
  return (
    <div style={{ position: 'relative', display: 'inline-block', width: '100%' }}>
      <span style={{
        position: 'absolute', top: -2, left: -2,
        color, opacity: finalOpacity, fontFamily: typography.mono, fontSize: 14,
        lineHeight: 1, userSelect: 'none',
        pointerEvents: 'none',
        fontWeight: 700,
      }}>⌐</span>
      
      <div style={{ padding: '0 8px' }}>
        {children}
      </div>
      
      <span style={{
        position: 'absolute', bottom: -2, right: -2,
        color, opacity: finalOpacity, fontFamily: typography.mono, fontSize: 14,
        lineHeight: 1, userSelect: 'none',
        pointerEvents: 'none',
        fontWeight: 700,
      }}>¬</span>
    </div>
  );
}
