import React, { useMemo, useState, useEffect, useRef } from 'react';
import { colors } from '../../styles/tokens';

interface ChamferedBoxProps {
  children: React.ReactNode;
  chamfer?: number;
  className?: string;
  style?: React.CSSProperties;
  borderColor?: string;
  backgroundColor?: string;
  isActive?: boolean;
  activeColor?: string;
  onClick?: React.MouseEventHandler<HTMLDivElement>;
  title?: string;
}

/**
 * ChamferedBox — utility for the 6px top-right chamfer.
 * Rebuilt using SVG for pixel-perfect borders that follow the cut.
 */
export function ChamferedBox({ 
  children, 
  chamfer = 12, 
  className = '', 
  style = {}, 
  borderColor = colors.borderMuted, 
  backgroundColor = colors.cardBg,
  isActive = false,
  activeColor = colors.acidGreen,
  onClick,
  title
}: ChamferedBoxProps) {
  const [dims, setDims] = useState({ w: 0, h: 0 });
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!ref.current) return;
    const update = () => {
      if (ref.current) {
        setDims({ w: ref.current.offsetWidth, h: ref.current.offsetHeight });
      }
    };
    update();
    const obs = new ResizeObserver(update);
    obs.observe(ref.current);
    return () => obs.disconnect();
  }, []);

  const strokeColor = isActive ? activeColor : borderColor;
  const strokeWidth = isActive ? 2 : 1.2;

  const pathData = useMemo(() => {
    const { w, h } = dims;
    if (w === 0 || h === 0) return '';
    
    // Safety: ensure chamfer doesn't exceed dimensions
    const c = Math.min(chamfer, w - 2, h - 2);
    const inset = strokeWidth / 2;

    return `
      M ${inset} ${inset}
      L ${w - c - inset} ${inset}
      L ${w - inset} ${c + inset}
      L ${w - inset} ${h - inset}
      L ${c + inset} ${h - inset}
      L ${inset} ${h - c - inset}
      Z
    `;
  }, [dims, chamfer, strokeWidth]);

  return (
    <div 
      ref={ref}
      className={className}
      onClick={onClick}
      title={title}
      style={{
        position: 'relative',
        display: style?.display || 'block',
        boxSizing: 'border-box',
        cursor: onClick ? 'pointer' : undefined,
        ...style
      }}
    >
      <svg 
        style={{
          position: 'absolute',
          top: 0,
          left: 0,
          width: '100%',
          height: '100%',
          pointerEvents: 'none',
          zIndex: 0,
          display: 'block',
        }}
      >
        <path
          d={pathData}
          fill={backgroundColor}
          stroke={strokeColor}
          strokeWidth={strokeWidth}
          strokeLinejoin="round"
          style={{
            filter: isActive ? `drop-shadow(0 0 6px ${activeColor}66)` : 'none',
            transition: 'stroke 0.2s, filter 0.2s'
          }}
        />
      </svg>
      <div style={{ 
        position: 'relative', 
        zIndex: 1, 
        width: '100%', 
        height: '100%',
        display: style?.display === 'flex' ? 'flex' : undefined,
        flexDirection: style?.flexDirection as any || (style?.display === 'flex' ? 'row' : undefined),
        alignItems: style?.alignItems || (style?.display === 'flex' ? 'center' : undefined),
        justifyContent: style?.justifyContent || (style?.display === 'flex' ? 'center' : undefined),
      }}>
        {children}
      </div>
    </div>
  );
}
