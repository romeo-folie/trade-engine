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

// ── Colour constants — mirrors tokens.css (single source of truth) ────────────
// These are used in JS for dynamic inline styles (strip/bracket colours per node).
// If you change a value in tokens.css, update it here too.
// TODO: consider reading CSS custom properties at runtime if theming is added.

import { TOKENS } from '../../styles/tokens';
const COLORS = TOKENS;

// Node identity → strip/bracket colour.
// Keyed on stable nodeId (from backend protocol.go), NOT on display label.
type NodeKindLabel = 'hub' | 'source' | 'processing' | 'output';

// Structural nodes: left colour strip
const STRIP_BY_ID: Record<string, string> = {
  '__hub__':          COLORS.acidGreen,
  'data_enrichment':  COLORS.slate,
  'indicator_engine': COLORS.slate,
  'prompt_assembly':  COLORS.slate,
  'risk_manager':     COLORS.amber,
  'consensus':        COLORS.acidGreen,
  'executor':         COLORS.emerald,
};

// Analyst/bracket nodes: corner bracket colour (no strip)
const BRACKET_BY_ID: Record<string, string> = {
  'llm_analysis':     COLORS.indigo,
  'blocked_signals':  COLORS.textDim,
  'approvals':        COLORS.amber,
};

// Nodes that display as "bypassed" when they have zero events
const BYPASS_IDS = new Set(['news_api']);

function getNodeColour(nodeId: string): { stripColor: string | null; bracketColor: string | null } {
  if (STRIP_BY_ID[nodeId]) return { stripColor: STRIP_BY_ID[nodeId], bracketColor: null };
  if (BRACKET_BY_ID[nodeId]) return { stripColor: null, bracketColor: BRACKET_BY_ID[nodeId] };
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
  const { stripColor, bracketColor } = getNodeColour(d.nodeId);
  const isBypassed = BYPASS_IDS.has(d.nodeId) && d.eventCount === 0;

  if (d.isHub) {
    return (
      <div
        className={styles.hub}
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
      onClick={e => { e.stopPropagation(); d.onNodeClick(d.nodeId); }}
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
  if (!Number.isFinite(ms)) return '--';
  if (ms < 60_000) return `${Math.floor(ms / 1000)}s ago`;
  if (ms < 3_600_000) return `${Math.floor(ms / 60_000)}m ago`;
  return `${Math.floor(ms / 3_600_000)}h ago`;
}

export default memo(PipelineNodeCard);
