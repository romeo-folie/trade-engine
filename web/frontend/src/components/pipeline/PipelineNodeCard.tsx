import { memo, useMemo } from 'react';
import { Handle, Position, type NodeProps, type Node } from '@xyflow/react';
import type { NodeMetrics } from '../../hooks/useEngineSocket';
import Sparkline from './Sparkline';
import EnsembleConsensus from './EnsembleConsensus';
import { ChamferedBox } from '../common/ChamferedBox';
import { colors } from '../../styles/tokens';
import styles from './PipelineNodeCard.module.css';

export interface PipelineNodeData {
  nodeId: string;
  label: string;
  kind: string;
  eventCount: number;
  lastEventAt: string | null;
  metrics: NodeMetrics | null;
  isHub?: boolean;
  lastEventType?: string | null;
  onNodeClick: (id: string) => void;
  [key: string]: unknown;
}

type PipelineNodeType = Node<PipelineNodeData>;

function PipelineNodeCard({ data }: NodeProps<PipelineNodeType>) {
  const d = data;
  const isBypassed = d.eventCount === 0 && d.nodeId !== '__hub__';
  const nodeActive = useMemo(() => {
    if (!d.lastEventAt) return false;
    const ms = typeof d.lastEventAt === 'string' ? new Date(d.lastEventAt).getTime() : d.lastEventAt;
    return Date.now() - ms < 5_000;
  }, [d.lastEventAt]);

  const statusDot = useMemo(() => {
    if (!d.lastEventAt) return styles.dotGrey;
    const ms = typeof d.lastEventAt === 'string' ? new Date(d.lastEventAt).getTime() : d.lastEventAt;
    const age = Date.now() - ms;
    if (age < 5_000) return styles.dotGreen;
    if (age < 30_000) return styles.dotAmber;
    return styles.dotGrey;
  }, [d.lastEventAt]);

  // ── Hub Template ──────────────────────────────────────────────────────────
  if (d.isHub || d.nodeId === '__hub__') {
    return (
      <ChamferedBox 
        isActive={nodeActive} 
        chamfer={12}
        backgroundColor="#0D0D11"
        borderColor={colors.acidGreen}
        className={styles.hub}
      >
        <Handle type="target" position={Position.Left} style={{ opacity: 0 }} />
        <Handle type="source" position={Position.Right} style={{ opacity: 0 }} />
        <div className={styles.hubWordmark}>ENGINE</div>
        <div className={`${styles.statusDot} ${statusDot}`} />
      </ChamferedBox>
    );
  }

  // ── Analyst Detection & Accent ───────────────────────────────────────────
  const isAnalyst = d.kind.toLowerCase().includes('analyst') || ['technical', 'momentum', 'sentiment', 'macro'].includes(d.nodeId);
  const accentColor = d.nodeId === 'technical' ? colors.analystTechnical : 
                     d.nodeId === 'momentum' ? colors.analystMomentum :
                     d.nodeId === 'sentiment' ? colors.analystSentiment :
                     d.nodeId === 'macro' ? colors.analystMacro : colors.borderMuted;

  const verdict = (d.lastEventType || 'NEUTRAL').toUpperCase();
  const verdictClass = verdict.includes('LONG') || verdict.includes('BUY') ? styles.verdictLong :
                      verdict.includes('SHORT') || verdict.includes('SELL') ? styles.verdictShort :
                      styles.verdictNeutral;

  return (
    <ChamferedBox 
      isActive={nodeActive}
      activeColor={isAnalyst ? accentColor : colors.acidGreen}
      borderColor={isAnalyst ? accentColor : colors.borderMuted}
      className={`${styles.cardBase} ${isBypassed ? styles.inactive : ''}`}
      style={{ cursor: 'pointer' }}
    >
      <div onClick={() => d.onNodeClick(d.nodeId)} className={styles.cardInner}>
        <Handle type="target" position={Position.Left} style={{ opacity: 0 }} />
        <Handle type="source" position={Position.Right} style={{ opacity: 0 }} />

        <div className={styles.analystHeader}>
          <span className={styles.kindLabel} style={{ color: isAnalyst ? accentColor : undefined }}>
            {d.kind.toUpperCase()}
          </span>
          <div className={`${styles.statusDot} ${statusDot}`} />
        </div>

        <div className={styles.nodeTitle}>{d.label}</div>

        {/* ── Signal Template ── */}
        {d.kind === 'source' && (
          <div className={styles.signalGrid}>
            <div>
              <div className={styles.price}>$84,192.30</div>
              <div className={styles.verdictLong} style={{ fontSize: 10, marginTop: 4 }}>+2.41% <span style={{ color: '#FBBF24', marginLeft: 4 }}>HIGH VOL</span></div>
            </div>
            <Sparkline data={[83900, 84100, 84050, 84150, 84192]} />
          </div>
        )}

        {/* ── Arbiter Template (Signal Router) ── */}
        {d.kind === 'arbiter' && (
          <div className={styles.arbiter}>
            <div className={styles.subValue}>ROUTING TO:</div>
            <div className={styles.routingBox}>
              <div className={`${styles.routeBadge} ${styles.routeActive}`} style={{ color: colors.analystTechnical }}>TECHNICAL</div>
              <div className={`${styles.routeBadge} ${styles.routeActive}`} style={{ color: colors.analystMomentum }}>MOMENTUM</div>
              <div className={styles.routeBadge}>SENTIMENT</div>
            </div>
            <div className={styles.recipeText}>MTL RECIPE: HTEM PERPENDUM</div>
            <div className={styles.kindLabel} style={{ marginTop: 2 }}>2 OF 4 ANALYSTS ROUTED</div>
          </div>
        )}

        {/* ── Analyst Template ── */}
        {isAnalyst && (
          <div>
            <div className={styles.subValue} style={{ fontSize: 9 }}>RSI • MACD • EMA</div>
            <div className={`${styles.analystVerdict} ${verdictClass}`}>
              {verdict.includes('LONG') ? 'LONG' : verdict.includes('SHORT') ? 'SHORT' : 'NEUTRAL'}
            </div>
            <div className={styles.kindLabel} style={{ opacity: 0.6 }}>CONFIDENCE: 84%</div>
            <div className={styles.kindLabel} style={{ opacity: 0.6 }}>WEIGHT: 0.40</div>
          </div>
        )}

        {/* ── Synthesis Template ── */}
        {d.kind === 'synthesis' && (
          <div className={styles.ensembleBox}>
            <div className={styles.subValue} style={{ fontSize: 8, marginBottom: 4 }}>ENSEMBLE CONSENSUS</div>
            <EnsembleConsensus items={[
              { label: 'TECH', value: 0.8, color: colors.analystTechnical },
              { label: 'MOM',  value: 0.4, color: colors.analystMomentum },
              { label: 'SENT', value: 0.9, color: colors.analystSentiment }
            ]} />
            <div className={`${styles.analystVerdict} ${styles.verdictLong}`} style={{ fontSize: 14, marginTop: 8 }}>
              LONG <span style={{ opacity: 1, fontSize: 14, marginLeft: 8 }}>0.81</span>
            </div>
          </div>
        )}

        {/* ── Execution Template ── */}
        {d.kind === 'execution' && (
          <div className={styles.execution}>
            <div className={`${styles.routeBadge} ${styles.routeActive}`} style={{ display: 'inline-block', marginBottom: 8 }}>LONG</div>
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 8 }}>
              <div>
                <div className={styles.subValue}>SIZE</div>
                <div className={styles.kindLabel} style={{ color: '#fff' }}>$12,500</div>
              </div>
              <div>
                <div className={styles.subValue}>ENTRY</div>
                <div className={styles.kindLabel} style={{ color: '#fff' }}>$84,241.50</div>
              </div>
            </div>
            <div className={styles.executionBadge} style={{ width: '100%', textAlign: 'center' }}>EXECUTING</div>
          </div>
        )}
      </div>
    </ChamferedBox>
  );
}

export default memo(PipelineNodeCard);
