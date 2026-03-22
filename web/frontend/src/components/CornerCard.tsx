/**
 * CornerCard — shared wrapper that renders a card with CSS corner brackets.
 *
 * Design spec: TL + BR diagonal bracket pair (┌ … ┘) at the outer boundary of the card.
 * NO separate outer border — the brackets ARE the card edge definition.
 *
 * `active` variant: full perimeter green border (replaces brackets).
 * `bracketColor` prop: override the bracket colour (default: #39FF14).
 */
import { type HTMLAttributes, type CSSProperties } from 'react';
import styles from './CornerCard.module.css';

interface CornerCardProps extends HTMLAttributes<HTMLDivElement> {
  /** Full green border instead of brackets (selected/active state) */
  active?: boolean;
  /** Override bracket colour — any CSS colour string */
  bracketColor?: string;
  /** Length of each bracket arm in px (default 10) */
  bracketSize?: number;
  /** Thickness of each bracket arm in px (default 1) */
  bracketWidth?: number;
  /** Suppress bracket rendering entirely (e.g. structural nodes with a left strip) */
  hideBrackets?: boolean;
}

export default function CornerCard({
  active,
  bracketColor,
  bracketSize = 10,
  bracketWidth = 1,
  hideBrackets,
  className,
  style,
  children,
  ...rest
}: CornerCardProps) {
  const cls = [
    styles.card,
    active ? styles.active : '',
    className ?? '',
  ].filter(Boolean).join(' ');

  // CSS custom props drive bracket size/colour so all 4 spans share one rule
  const cssVars: CSSProperties = {
    '--bracket-color': bracketColor ?? '#39FF14',
    '--bracket-size': `${bracketSize}px`,
    '--bracket-width': `${bracketWidth}px`,
    ...style,
  } as CSSProperties;

  return (
    <div className={cls} style={cssVars} {...rest}>
      {/* Diagonal corner bracket pair: TL + BR — omitted when hideBrackets or active */}
      {!hideBrackets && !active && (
        <>
          <span className={`${styles.b} ${styles.bTL}`} aria-hidden="true" />
          <span className={`${styles.b} ${styles.bBR}`} aria-hidden="true" />
        </>
      )}
      {children}
    </div>
  );
}
