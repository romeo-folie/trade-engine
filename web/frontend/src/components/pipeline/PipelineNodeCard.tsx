import { memo, useMemo } from 'react';
import { Handle, Position, type NodeProps, type Node } from '@xyflow/react';
import type { NodeMetrics } from '../../hooks/useEngineSocket';
import EnsembleConsensus from './EnsembleConsensus';
import { ChamferedBox } from '../common/ChamferedBox';
import { colors } from '../../styles/tokens';
import styles from './PipelineNodeCard.module.css';

function lastTickLabel(lastEventAt: string | null): string {
  if (!lastEventAt) return '—';
  const ms = Date.now() - new Date(lastEventAt).getTime();
  if (ms < 60_000) return `${Math.floor(ms / 1000)}s ago`;
  if (ms < 3_600_000) return `${Math.floor(ms / 60_000)}m ago`;
  return `${Math.floor(ms / 3_600_000)}h ago`;
}

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
  provider?: string;
  accuracy?: number;
  accentColor?: string;
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
  const isAnalyst = d.kind.toLowerCase().includes('analyst') || ['technical', 'momentum', 'sentiment', 'macro', 'llm_analysis'].includes(d.nodeId);
  const accentColor = d.accentColor ??
                     (d.nodeId === 'technical' ? colors.analystTechnical :
                     d.nodeId === 'momentum' ? colors.analystMomentum :
                     d.nodeId === 'sentiment' ? colors.analystSentiment :
                     d.nodeId === 'macro' ? colors.analystMacro :
                     d.nodeId === 'llm_analysis' ? colors.acidGreen : colors.borderMuted);

  const verdict = (d.lastEventType || 'NEUTRAL').toUpperCase();
  const verdictClass = verdict.includes('LONG') || verdict.includes('BUY') ? styles.verdictLong :
                      verdict.includes('SHORT') || verdict.includes('SELL') ? styles.verdictShort :
                      styles.verdictNeutral;

  const isArbiterFinal = d.kind === 'arbiter_final';
  const isTicker = d.kind === 'ticker';
  const cardAccent = isArbiterFinal ? colors.amber
                   : isAnalyst ? accentColor
                   : colors.acidGreen;
  const cardBorder = isArbiterFinal ? colors.amber
                   : isAnalyst ? accentColor
                   : colors.borderMuted;

  return (
    <ChamferedBox
      isActive={nodeActive}
      activeColor={cardAccent}
      borderColor={cardBorder}
      className={`${styles.cardBase} ${isBypassed ? styles.inactive : ''} ${isTicker ? styles.tickerCard : ''}`}
      style={{ cursor: 'pointer' }}
    >
      <div onClick={() => d.onNodeClick(d.nodeId)} className={styles.cardInner}>
        <Handle type="target" position={Position.Left} style={{ opacity: 0 }} />
        <Handle type="source" position={Position.Right} style={{ opacity: 0 }} />

        <div className={styles.analystHeader}>
          <span className={styles.kindLabel} style={{ color: isAnalyst ? accentColor : isArbiterFinal ? colors.amber : undefined }}>
            {d.kind === 'arbiter_final' ? 'ARBITER' : d.kind.toUpperCase()}
          </span>
          <div className={`${styles.statusDot} ${statusDot}`} />
        </div>

        <div className={styles.nodeTitle}>{d.label}</div>

        {/* ── Source Template ── */}
        {d.kind === 'source' && (
          <div className={styles.sourceContent}>
            <div className={styles.sourceRow}>
              <div>
                <div className={styles.subValue}>EVENTS</div>
                <div className={styles.metricValue}>{d.eventCount.toLocaleString()}</div>
              </div>
              <div>
                <div className={styles.subValue}>LAST TICK</div>
                <div className={styles.metricValue}>{lastTickLabel(d.lastEventAt)}</div>
              </div>
            </div>
            <div className={styles.feedStatusRow}>
              <span className={nodeActive ? styles.feedBadgeActive : styles.feedBadgeIdle}>
                {nodeActive ? 'LIVE' : 'IDLE'}
              </span>
            </div>
          </div>
        )}

        {/* ── Arbiter Template (Indicator Engine) ── */}
        {d.kind === 'arbiter' && (
          <div className={styles.arbiter}>
            <div className={styles.subValue}>INDICATOR ENGINE</div>
            <div className={styles.routingBox}>
              {['RSI', 'EMA', 'MACD', 'ADX', 'ATR', 'OBV'].map(ind => (
                <div key={ind} className={`${styles.routeBadge} ${nodeActive ? styles.routeActive : ''}`}>{ind}</div>
              ))}
            </div>
            <div className={styles.subValue} style={{ marginTop: 6 }}>
              {d.eventCount > 0 ? `${d.eventCount.toLocaleString()} COMPUTATIONS` : 'WAITING FOR DATA'}
            </div>
          </div>
        )}

        {/* ── Analyst Template ── */}
        {isAnalyst && (
          <div>
            {d.provider && (
              <div className={styles.subValue} style={{ fontSize: 9, marginBottom: 2 }}>
                {d.provider.toUpperCase()}
              </div>
            )}
            {!d.provider && (
              <div className={styles.subValue} style={{ fontSize: 9 }}>LLM ENSEMBLE</div>
            )}
            <div className={`${styles.analystVerdict} ${verdictClass}`} style={{ fontSize: 16 }}>
              {verdict.includes('LONG') || verdict.includes('BUY') ? 'LONG' :
               verdict.includes('SHORT') || verdict.includes('SELL') ? 'SHORT' : 'ANALYZING'}
            </div>
            {d.accuracy !== undefined && d.accuracy > 0 && (
              <div className={styles.kindLabel} style={{ opacity: 0.7 }}>
                ACC: {Math.round(d.accuracy * 100)}%
              </div>
            )}
            <div className={styles.kindLabel} style={{ opacity: 0.5 }}>
              {d.eventCount > 0 ? `${d.eventCount} SIGNALS` : 'PENDING'}
            </div>
          </div>
        )}

        {/* ── Synthesis Template ── */}
        {d.kind === 'synthesis' && (
          <div className={styles.ensembleBox}>
            {d.nodeId === 'risk_manager' ? (
              <>
                <div className={styles.subValue} style={{ fontSize: 8, marginBottom: 6 }}>RISK CHECKS</div>
                {['CONFIDENCE', 'R/R RATIO', 'POSITION SIZE', 'DRAWDOWN'].map(check => (
                  <div key={check} style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 3 }}>
                    <span className={styles.kindLabel}>{check}</span>
                    <span className={styles.kindLabel} style={{ color: nodeActive ? colors.emerald : colors.textDim }}>
                      {nodeActive ? 'PASS' : '—'}
                    </span>
                  </div>
                ))}
              </>
            ) : (
              <>
                <div className={styles.subValue} style={{ fontSize: 8, marginBottom: 4 }}>ENSEMBLE CONSENSUS</div>
                <EnsembleConsensus items={[
                  { label: 'TECH', value: 0.8, color: colors.analystTechnical },
                  { label: 'MOM',  value: 0.4, color: colors.analystMomentum },
                  { label: 'SENT', value: 0.9, color: colors.analystSentiment }
                ]} />
                <div className={`${styles.analystVerdict} ${styles.verdictLong}`} style={{ fontSize: 14, marginTop: 8 }}>
                  LONG <span style={{ opacity: 1, fontSize: 14, marginLeft: 8 }}>0.81</span>
                </div>
              </>
            )}
          </div>
        )}

        {/* ── Execution Template ── */}
        {d.kind === 'execution' && (
          <div className={styles.execution}>
            {d.nodeId === 'portfolio' ? (
              <>
                <div className={styles.subValue} style={{ marginBottom: 6 }}>PORTFOLIO</div>
                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 8 }}>
                  <div>
                    <div className={styles.subValue}>EQUITY</div>
                    <div className={styles.kindLabel} style={{ color: '#fff' }}>—</div>
                  </div>
                  <div>
                    <div className={styles.subValue}>POSITIONS</div>
                    <div className={styles.kindLabel} style={{ color: '#fff' }}>
                      {d.eventCount > 0 ? d.eventCount : '0'}
                    </div>
                  </div>
                </div>
                <div className={styles.executionBadge} style={{ width: '100%', textAlign: 'center', marginTop: 8 }}>
                  {nodeActive ? 'TRACKING' : 'IDLE'}
                </div>
              </>
            ) : (
              <>
                <div className={`${styles.routeBadge} ${styles.routeActive}`} style={{ display: 'inline-block', marginBottom: 8 }}>LONG</div>
                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 8 }}>
                  <div>
                    <div className={styles.subValue}>SIZE</div>
                    <div className={styles.kindLabel} style={{ color: '#fff' }}>—</div>
                  </div>
                  <div>
                    <div className={styles.subValue}>ENTRY</div>
                    <div className={styles.kindLabel} style={{ color: '#fff' }}>—</div>
                  </div>
                </div>
                <div className={styles.executionBadge} style={{ width: '100%', textAlign: 'center' }}>
                  {nodeActive ? 'EXECUTING' : 'READY'}
                </div>
              </>
            )}
          </div>
        )}

        {/* ── Ticker Template ── */}
        {d.kind === 'ticker' && (
          <div style={{ padding: '2px 0' }}>
            <div style={{
              fontFamily: 'var(--font-mono)',
              fontSize: 18,
              fontWeight: 800,
              color: colors.textPrimary,
              letterSpacing: '0.05em',
              marginBottom: 6,
            }}>
              {d.label}
            </div>
            <div className={styles.feedStatusRow}>
              <span className={nodeActive ? styles.feedBadgeActive : styles.feedBadgeIdle}>
                {nodeActive ? 'LIVE' : 'IDLE'}
              </span>
            </div>
          </div>
        )}

        {/* ── Arbiter Final Template ── */}
        {d.kind === 'arbiter_final' && (
          <div>
            <div className={styles.nodeTitle}>Ensemble Resolver</div>
            <div className={styles.subValue} style={{ marginTop: 4 }}>
              RESOLVES DISSENT
            </div>
            <div className={styles.feedStatusRow} style={{ marginTop: 8 }}>
              <span className={nodeActive ? styles.feedBadgeActive : styles.feedBadgeIdle}>
                {nodeActive ? 'ACTIVE' : 'STANDBY'}
              </span>
            </div>
          </div>
        )}
      </div>
    </ChamferedBox>
  );
}

export default memo(PipelineNodeCard);
