/**
 * PipelineNodeCard — single node in the hub-and-spoke graph.
 *
 * Design spec:
 *   - Analyst nodes (TECHNICAL, MOMENTUM, SENTIMENT, MACRO):
 *     uniform 1px border, no brackets, no left strip.
 *     Active: accent border + glow. Shows category label, name,
 *     signal description, direction (colour-coded), confidence.
 *   - Structural nodes (SIGNAL SOURCE, ARBITER, CONSENSUS, EXECUTOR etc):
 *     left colour strip, corner brackets via CornerCard.
 *   - Hub (ENGINE): acid green border, larger card, centre label.
 *   - Status dot: green <5s, amber <30s, grey ≥30s
 *   - Chamfer top-right 6px on all cards.
 */
import { memo } from 'react';
import { Handle, Position, type NodeProps, type Node } from '@xyflow/react';
import type { NodeMetrics } from '../../hooks/useEngineSocket';
import CornerCard from '../CornerCard';
import styles from './PipelineNodeCard.module.css';

import { TOKENS } from '../../styles/tokens';
const COLORS = TOKENS;

type NodeKindLabel = 'hub' | 'source' | 'processing' | 'output';

// ── Node classification ─────────────────────────────────────────────────────

// Analyst node IDs — these get the analyst card layout (no strip, no brackets)
const ANALYST_IDS = new Set([
  'technical', 'momentum', 'sentiment', 'macro',
  // Fallback: any node whose label contains these
]);

// Category labels per node kind (shown as small category text above name)
const CATEGORY_BY_ID: Record<string, string> = {
  'technical':  'ANALYST',
  'momentum':   'ANALYST',
  'sentiment':  'ANALYST',
  'macro':      'ANALYST',
  'llm_analysis': 'ARBITER',
  'consensus':    'SYNTHESIS',
  'executor':     'EXECUTION',
  'data_enrichment':  'PROCESSING',
  'indicator_engine': 'PROCESSING',
  'prompt_assembly':  'PROCESSING',
  'risk_manager':     'PROCESSING',
  'blocked_signals':  'OUTPUT',
  'approvals':        'OUTPUT',
};

// Structural nodes: left colour strip + brackets
const STRIP_BY_ID: Record<string, string> = {
  '__hub__':          COLORS.acidGreen,
  'data_enrichment':  COLORS.slate,
  'indicator_engine': COLORS.slate,
  'prompt_assembly':  COLORS.slate,
  'risk_manager':     COLORS.amber,
  'consensus':        COLORS.acidGreen,
  'executor':         COLORS.emerald,
  'llm_analysis':     COLORS.indigo,
  'blocked_signals':  COLORS.textDim,
  'approvals':        COLORS.amber,
};

// Nodes that display as "bypassed" when they have zero events
const BYPASS_IDS = new Set(['news_api']);

function isAnalystNode(nodeId: string, label: string): boolean {
  if (ANALYST_IDS.has(nodeId)) return true;
  const l = label.toLowerCase();
  return l === 'technical' || l === 'momentum' || l === 'sentiment' || l === 'macro';
}

function isActive(lastEventAt: string | null | number): boolean {
  if (!lastEventAt) return false;
  const ms = typeof lastEventAt === 'string' ? new Date(lastEventAt).getTime() : lastEventAt;
  return Date.now() - ms < 5_000;
}

// ── Status dot ──────────────────────────────────────────────────────────────

function statusDotClass(lastEventAt: string | null | number): string {
  if (!lastEventAt) return styles.dotGrey;
  const ms = typeof lastEventAt === 'string' ? new Date(lastEventAt).getTime() : lastEventAt;
  const age = Date.now() - ms;
  if (age < 5_000)  return styles.dotGreen;
  if (age < 30_000) return styles.dotAmber;
  return styles.dotGrey;
}

// ── Signal direction helpers ────────────────────────────────────────────────

function getSignalDirection(d: PipelineNodeData): string | null {
  // Try metrics payload first, then lastEventType
  const m = d.metrics as NodeMetrics | null;
  if (m && typeof (m as unknown as Record<string, unknown>).direction === 'string') {
    return ((m as unknown as Record<string, unknown>).direction as string).toUpperCase();
  }
  // Try lastEventType as a direction hint
  if (d.lastEventType) {
    const t = d.lastEventType.toUpperCase();
    if (t.includes('LONG') || t.includes('BUY')) return 'LONG';
    if (t.includes('SHORT') || t.includes('SELL')) return 'SHORT';
    if (t.includes('NEUTRAL') || t.includes('HOLD')) return 'NEUTRAL';
  }
  return null;
}

function getSignalDescription(d: PipelineNodeData): string {
  // Use lastEventType for a descriptive label
  if (d.lastEventType) {
    return d.lastEventType.replace(/_/g, ' ').toUpperCase();
  }
  return 'MONITORING';
}

function directionClass(dir: string): string {
  if (dir === 'LONG') return styles.dirLong;
  if (dir === 'SHORT') return styles.dirShort;
  return styles.dirNeutral;
}

// ── Component ───────────────────────────────────────────────────────────────

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

type PipelineNodeType = Node<PipelineNodeData>;

function PipelineNodeCard({ data }: NodeProps<PipelineNodeType>) {
  const d = data;
  const isBypassed = BYPASS_IDS.has(d.nodeId) && d.eventCount === 0;
  const nodeActive = isActive(d.lastEventAt);

  // Hub
  if (d.isHub) {
    return (
      <div className={styles.hub}>
        <Handle type="source" position={Position.Right} style={{ opacity: 0 }} />
        <Handle type="target" position={Position.Left}  style={{ opacity: 0 }} />
        <span className={styles.hubLabel}>ENGINE</span>
      </div>
    );
  }

  const analyst = isAnalystNode(d.nodeId, d.label);
  const category = CATEGORY_BY_ID[d.nodeId] ?? (analyst ? 'ANALYST' : d.kind.toUpperCase());
  const stripColor = STRIP_BY_ID[d.nodeId] ?? null;

  // Analyst nodes: plain border card, no brackets, no strip
  if (analyst) {
    const direction = getSignalDirection(d);
    const signalDesc = getSignalDescription(d);

    return (
      <div
        className={`${styles.analystCard} ${nodeActive ? styles.analystActive : ''} ${isBypassed ? styles.bypassed : ''}`}
        role="button"
        tabIndex={0}
        onClick={e => { e.stopPropagation(); d.onNodeClick(d.nodeId); }}
        onKeyDown={e => {
          if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); d.onNodeClick(d.nodeId); }
        }}
        aria-label={`${d.label} pipeline node`}
        style={{ opacity: isBypassed ? 0.5 : 1 }}
      >
        <Handle type="target" position={Position.Left}  style={{ opacity: 0 }} />
        <Handle type="source" position={Position.Right} style={{ opacity: 0 }} />

        <div className={styles.analystInner}>
          <div className={styles.analystHeader}>
            <span className={styles.categoryLabel}>{category}</span>
            <span className={`${styles.dot} ${statusDotClass(d.lastEventAt)}`} />
          </div>

          <span className={styles.analystName}>{d.label}</span>

          <span className={styles.signalDesc}>{signalDesc}</span>

          {direction && (
            <span className={`${styles.signalDirection} ${directionClass(direction)}`}>
              {direction}
            </span>
          )}

          {d.eventCount > 0 && (
            <span className={styles.analystConfidence}>
              {d.eventCount.toLocaleString()} events
            </span>
          )}
        </div>
      </div>
    );
  }

  // Structural nodes: CornerCard with left strip and brackets
  return (
    <CornerCard
      bracketColor={stripColor ?? 'var(--border)'}
      bracketSize={10}
      bracketWidth={1}
      className={`${styles.card} ${isBypassed ? styles.bypassed : ''}`}
      role="button"
      tabIndex={0}
      onClick={e => { e.stopPropagation(); d.onNodeClick(d.nodeId); }}
      onKeyDown={e => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); d.onNodeClick(d.nodeId); }
      }}
      aria-label={`${d.label} pipeline node`}
      style={{ opacity: isBypassed ? 0.35 : 1 }}
    >
      <Handle type="target" position={Position.Left}  style={{ opacity: 0 }} />
      <Handle type="source" position={Position.Right} style={{ opacity: 0 }} />

      {stripColor && (
        <div className={styles.strip} style={{ background: stripColor }} />
      )}

      <div className={styles.inner}>
        <div className={styles.header}>
          <span className={styles.categoryLabel}>{category}</span>
          <span className={`${styles.dot} ${statusDotClass(d.lastEventAt)}`} />
        </div>

        <span className={styles.nodeLabel}>{d.label}</span>

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
    </CornerCard>
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
