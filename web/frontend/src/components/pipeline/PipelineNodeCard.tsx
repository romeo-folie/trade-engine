/**
 * PipelineNodeCard — single node in the hub-and-spoke graph.
 *
 * Design spec (designer-tradeengine-frontend-spec-20260316.md §2):
 *   - Chamfered rectangle, top-right corner only (6px cut)
 *   - Left strip: structural nodes (SIGNAL SOURCE, ARBITER, SYNTHESIS) only
 *   - Analyst nodes: corner bracket colour, no left strip
 *   - Status dot: green <5s, amber <30s, grey ≥30s
 *   - Hub (ENGINE): acid green border, larger card, centre label
 */
import { memo } from 'react';
import { Handle, Position, type NodeProps } from '@xyflow/react';
import type { NodeMetrics } from '../../hooks/useEngineSocket';
import styles from './PipelineNodeCard.module.css';

// ── Colour constants (from design spec tokens) ───────────────────────────────

const COLORS = {
  acidGreen:  '#39FF14',
  amber:      '#F59E0B',
  red:        '#EF4444',
  slate:      '#4A5568',
  indigo:     '#6366F1',
  violet:     '#8B5CF6',
  sky:        '#0EA5E9',
  emerald:    '#22C55E',
  border:     '#2A2A35',
  cardBg:     '#111115',
  textPrimary:'#F0F0F0',
  textMuted:  '#9CA3AF',
  textDim:    '#6B7280',
};

// Node identity → strip/bracket colour
type NodeKindLabel = 'hub' | 'source' | 'processing' | 'output';

const STRIP_COLORS: Record<string, string> = {
  // Structural nodes with left strips
  'SIGNAL SOURCE': COLORS.slate,
  'ARBITER':       COLORS.acidGreen,
  'SYNTHESIS':     COLORS.amber,
  'EXECUTION':     COLORS.emerald, // status-driven; default emerald
  // Hub
  '__hub__':       COLORS.acidGreen,
};

// Analyst bracket colours (no strips)
const ANALYST_COLORS: Record<string, string> = {
  'TECHNICAL':  COLORS.indigo,
  'MOMENTUM':   COLORS.amber,
  'SENTIMENT':  COLORS.violet,
  'MACRO':      COLORS.sky,
};

function getNodeColour(label: string): { stripColor: string | null; bracketColor: string | null } {
  const upper = label.toUpperCase();
  if (STRIP_COLORS[upper]) return { stripColor: STRIP_COLORS[upper], bracketColor: null };
  if (ANALYST_COLORS[upper]) return { stripColor: null, bracketColor: ANALYST_COLORS[upper] };
  return { stripColor: null, bracketColor: COLORS.textDim };
}

// ── Status dot ───────────────────────────────────────────────────────────────

function statusDotClass(lastEventAt: string | null | number): string {
  if (!lastEventAt) return styles.dotGrey;
  const ms = typeof lastEventAt === 'string' ? new Date(lastEventAt).getTime() : lastEventAt;
  const age = Date.now() - ms;
  if (age < 5_000)  return styles.dotGreen;
  if (age < 30_000) return styles.dotAmber;
  return styles.dotGrey;
}

// ── Component ────────────────────────────────────────────────────────────────

export interface PipelineNodeData {
  nodeId: string;
  label: string;
  kind: NodeKindLabel | string;
  eventCount: number;
  lastEventAt: string | null;
  metrics: NodeMetrics | null;
  isHub: boolean;
  lastEventType: string | null;
  onNodeClick: (id: string) => void;
  [key: string]: unknown;
}

function PipelineNodeCard({ data }: NodeProps) {
  const d = data as PipelineNodeData;
  const { stripColor, bracketColor } = getNodeColour(d.label);
  const isBypassed = d.label.toUpperCase() === 'MACRO' && d.eventCount === 0;

  if (d.isHub) {
    return (
      <div
        className={styles.hub}
        aria-label="ENGINE hub node"
      >
        <Handle type="source" position={Position.Right} style={{ opacity: 0 }} />
        <Handle type="target" position={Position.Left}  style={{ opacity: 0 }} />
        <span className={styles.hubLabel}>ENGINE</span>
      </div>
    );
  }

  return (
    <div
      className={`${styles.card} ${isBypassed ? styles.bypassed : ''}`}
      role="button"
      tabIndex={0}
      onKeyDown={e => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          d.onNodeClick(d.nodeId);
        }
      }}
      aria-label={`${d.label} pipeline node`}
      style={{
        opacity: isBypassed ? 0.35 : 1,
      }}
    >
      <Handle type="target" position={Position.Left}   style={{ opacity: 0 }} />
      <Handle type="source" position={Position.Right}  style={{ opacity: 0 }} />

      {/* Left colour strip (structural nodes only) */}
      {stripColor && (
        <div className={styles.strip} style={{ background: stripColor }} />
      )}

      {/* Corner brackets (analyst nodes only) */}
      {bracketColor && !stripColor && (
        <>
          <span className={styles.bracketTL} aria-hidden="true" style={{ color: bracketColor, opacity: isBypassed ? 0.35 : 0.6 }}>⌐</span>
          <span className={styles.bracketBR} aria-hidden="true" style={{ color: bracketColor, opacity: isBypassed ? 0.35 : 0.6 }}>¬</span>
        </>
      )}

      <div className={styles.inner}>
        <div className={styles.header}>
          <span className={styles.nodeLabel}>{d.label}</span>
          <span className={`${styles.dot} ${statusDotClass(d.lastEventAt)}`} />
        </div>

        <div className={styles.meta}>
          <span className={styles.metaValue}>{d.eventCount.toLocaleString()}</span>
          <span className={styles.metaLabel}> events</span>
        </div>

        {d.lastEventAt && (
          <div className={styles.timestamp}>
            {formatRelative(d.lastEventAt)}
          </div>
        )}

        {isBypassed && (
          <div className={styles.bypassedLabel}>BYPASSED</div>
        )}
      </div>
    </div>
  );
}

function formatRelative(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime();
  if (ms < 60_000) return `${Math.floor(ms / 1000)}s ago`;
  if (ms < 3_600_000) return `${Math.floor(ms / 60_000)}m ago`;
  return `${Math.floor(ms / 3_600_000)}h ago`;
}

export default memo(PipelineNodeCard);
