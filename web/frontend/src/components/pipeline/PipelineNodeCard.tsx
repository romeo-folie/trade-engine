import { memo, useMemo } from 'react';
import { motion } from 'framer-motion';
import { Handle, Position, type NodeProps, type Node } from '@xyflow/react';
import type { NodeMetrics } from '../../hooks/useEngineSocket';
import EnsembleConsensus from './EnsembleConsensus';
import { ChamferedBox } from '../common/ChamferedBox';
import { colors } from '../../styles/tokens';
import styles from './PipelineNodeCard.module.css';
import type { WatchlistTicker } from '../../hooks/useWatchlist';
import type { RiskCheckResult, LastTrade, PortfolioState } from '../../hooks/useEngineSocket';

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
  watchlistTickers?: WatchlistTicker[];
  isOrbMode?: boolean;
  isChosen?: boolean;
  [key: string]: unknown;
}

type PipelineNodeType = Node<PipelineNodeData>;

function PipelineNodeCard({ data }: NodeProps<PipelineNodeType>) {
  const d = data;
  const isBypassed = d.eventCount === 0 && d.nodeId !== '__hub__';
  const nodeActive = useMemo(() => {
    // Execution cards (executor, portfolio): treat as active if data is present.
    // Trades/positions are infrequent — a recency gate would show IDLE immediately
    // after the 5s window even though the last trade data is still displayed.
    if (d.kind === 'execution') {
      const allPassed = (d.allRiskPassed as boolean | undefined) !== false;
      if (d.nodeId === 'executor') return !!(d.lastTrade as LastTrade | undefined) && allPassed;
      if (d.nodeId === 'portfolio') return !!(d.portfolioState as PortfolioState | undefined) && allPassed;
    }
    if (!d.lastEventAt) return false;
    const ms = typeof d.lastEventAt === 'string' ? new Date(d.lastEventAt).getTime() : d.lastEventAt;
    return Date.now() - ms < 5_000;
  }, [d.lastEventAt, d.kind, d.nodeId, d.lastTrade, d.portfolioState]);

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
        isActive={false}
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
  const cardBorder = isArbiterFinal ? colors.amber
                   : isAnalyst ? accentColor
                   : colors.border;

  return (
    <ChamferedBox
      isActive={false}
      borderColor={cardBorder}
      className={`${styles.cardBase} ${isBypassed ? styles.inactive : ''} ${isTicker ? styles.tickerCard : ''} ${d.nodeId === 'risk_manager' ? styles.riskCard : ''}`}
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

        {d.kind !== 'arbiter_final' && (
          <div className={styles.nodeTitle}>{d.label}</div>
        )}

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
            {/* Watchlist ticker marquee — Alpaca Feed only, hidden in ORB mode */}
            {d.nodeId === 'alpaca' && !d.isOrbMode && (
              <WatchlistMarquee tickers={d.watchlistTickers ?? []} />
            )}
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

        {/* ── Enricher Template ── */}
        {d.kind === 'enricher' && (
          <div className={styles.enricherContent}>
            <div className={styles.subValue}>SENTIMENT EXTRACTION</div>
            <div className={styles.metricValue} style={{ color: colors.amber }}>
              {d.eventCount > 0 ? `${d.eventCount} ARTICLES` : 'LISTENING'}
            </div>
            <div className={styles.feedStatusRow} style={{ marginTop: 8 }}>
              <span className={nodeActive ? styles.feedBadgeActive : styles.feedBadgeIdle}>
                {nodeActive ? 'ENRICHING' : 'IDLE'}
              </span>
            </div>
          </div>
        )}

        {/* ── Analyst Template ── */}
        {isAnalyst && (
          <div>
            {d.provider && (
              <div className={styles.subValue} style={{ fontSize: 9, marginBottom: 8 }}>
                {d.provider.toUpperCase()}
              </div>
            )}
            {!d.provider && (
              <div className={styles.subValue} style={{ fontSize: 9, marginBottom: 8 }}>LLM ENSEMBLE</div>
            )}
            {/* Per-analyst live vote badge */}
            {(() => {
              const liveVerdict = d.verdict as string | undefined;
              const voteConf = d.voteConfidence as number | undefined;
              const isSkipped = d.skipped as boolean | undefined;
              if (isSkipped) {
                return (
                  <div className={styles.kindLabel} style={{ opacity: 0.4, marginTop: 8, fontSize: 11 }}>
                    SKIPPED
                  </div>
                );
              }
              if (liveVerdict) {
                const vColor = liveVerdict === 'BUY' ? colors.emerald : liveVerdict === 'SELL' ? colors.red : colors.amber;
                const vLabel = liveVerdict === 'BUY' ? 'LONG' : liveVerdict === 'SELL' ? 'SHORT' : liveVerdict;
                return (
                  <div style={{ marginTop: 8, display: 'flex', alignItems: 'center', gap: 6 }}>
                    <span style={{ color: vColor, fontWeight: 700, fontSize: 14, fontFamily: 'var(--font-mono)' }}>
                      {vLabel}
                    </span>
                    {voteConf != null && (
                      <span style={{ color: vColor, opacity: 0.8, fontSize: 12, fontFamily: 'var(--font-mono)' }}>
                        {Math.round(voteConf * 100)}%
                      </span>
                    )}
                  </div>
                );
              }
              return null;
            })()}
            {d.isChosen ? (
              <div className={styles.feedStatusRow} style={{ marginTop: 12 }}>
                <span className={styles.feedBadgeActive} style={{ background: 'rgba(57, 255, 20, 0.15)', border: '1px solid #39FF14', boxShadow: '0 0 10px rgba(57, 255, 20, 0.2)' }}>
                  ACTIVE
                </span>
              </div>
            ) : (
              <div className={styles.feedStatusRow} style={{ marginTop: 12 }}>
                <span className={styles.feedBadgeIdle}>STANDBY</span>
              </div>
            )}
            {!d.verdict && !d.skipped && d.isChosen && (
              <div className={`${styles.analystVerdict} ${verdictClass}`} style={{ fontSize: 16 }}>
                {verdict.includes('LONG') || verdict.includes('BUY') ? 'LONG' :
                 verdict.includes('SHORT') || verdict.includes('SELL') ? 'SHORT' : 'ANALYZING'}
              </div>
            )}
            {d.accuracy !== undefined && d.accuracy > 0 && d.isChosen && (
              <div className={styles.kindLabel} style={{ opacity: 0.7, marginTop: 8 }}>
                ACC: {Math.round(d.accuracy * 100)}%
              </div>
            )}
            {d.eventCount > 0 && d.isChosen && (
              <div className={styles.kindLabel} style={{ opacity: 0.5 }}>
                {d.eventCount} ANALYSES
              </div>
            )}
          </div>
        )}

        {/* ── Synthesis Template ── */}
        {d.kind === 'synthesis' && (
          <div className={styles.ensembleBox}>
            {d.nodeId === 'risk_manager' ? (
              (() => {
                const checks = d.riskChecks as RiskCheckResult[] | undefined;
                const RISK_CHECK_NAMES = [
                  'confidence_threshold',
                  'risk_reward_ratio',
                  'position_size_limit',
                  'max_open_positions',
                  'daily_loss_limit',
                  'max_drawdown',
                  'cooldown_after_loss'
                ];
                
                
                return (
                  <>
                    <div className={styles.subValue} style={{ fontSize: 8, marginBottom: 6 }}>RISK CHECKS</div>
                    {RISK_CHECK_NAMES.map(checkName => {
                      const check = checks?.find(c => c.name === checkName);
                      return (
                        <div key={checkName} style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 3 }} title={check?.reason || undefined}>
                          <span className={styles.kindLabel}>{checkName}</span>
                          <span className={styles.kindLabel} style={{ color: check ? (check.passed ? colors.emerald : colors.red) : colors.textDim }}>
                            {check ? (check.passed ? 'PASS' : 'FAIL') : '—'}
                          </span>
                        </div>
                      );
                    })}
                  </>
                );
              })()
            ) : (
              (() => {
                const PROVIDER_CONFIG = [
                  { id: 'openai', label: 'GPT', color: colors.analystMomentum },
                  { id: 'anthropic', label: 'ANTH', color: colors.analystTechnical },
                  { id: 'google', label: 'GEMI', color: colors.analystSentiment },
                ];

                // Drive consensus display entirely from analystVotes (last AnalystVotesPublished event).
                // analystConsensus accumulates across tickers/ticks and is stale — don't use it here.
                const av = d.analystVotes as { model: string; provider: string; action: string; confidence: number; skipped: boolean }[] | undefined;
                const activeVotes = av ? av.filter(v => !v.skipped) : [];

                const items = PROVIDER_CONFIG.map(conf => {
                  const vote = activeVotes.find(v => v.provider?.toLowerCase() === conf.id);
                  return {
                    label: conf.label,
                    value: vote?.confidence,
                    color: conf.color,
                    isLoading: false,
                  };
                });

                const buys = activeVotes.filter(v => v.action?.toUpperCase() === 'BUY').length;
                const sells = activeVotes.filter(v => v.action?.toUpperCase() === 'SELL').length;
                const consensusVerdict = activeVotes.length === 0 ? null : buys > sells ? 'LONG' : sells > buys ? 'SHORT' : 'HOLD';
                const avgConf = activeVotes.length > 0
                  ? activeVotes.reduce((sum, v) => sum + v.confidence, 0) / activeVotes.length
                  : 0;
                
                const verdictCls = consensusVerdict === 'LONG' ? styles.verdictLong
                  : consensusVerdict === 'SHORT' ? styles.verdictShort
                  : styles.verdictNeutral;
                  
                return (
                  <>
                    <div className={styles.subValue} style={{ fontSize: 8, marginBottom: 4 }}>ENSEMBLE CONSENSUS</div>
                    <EnsembleConsensus items={items} />
                    <div className={`${styles.analystVerdict} ${consensusVerdict ? verdictCls : styles.verdictNeutral}`} style={{ fontSize: 14, marginTop: 8 }}>
                      {consensusVerdict ? <>{consensusVerdict} <span style={{ opacity: 1, fontSize: 14, marginLeft: 8 }}>{avgConf.toFixed(2)}</span></> : (nodeActive ? 'CONSULTING...' : '—')}
                    </div>
                  </>
                );
              })()
            )}
          </div>
        )}

        {/* ── Execution Template ── */}
        {d.kind === 'execution' && (
          <div className={styles.execution}>
            {d.nodeId === 'portfolio' ? (
              (() => {
                const ps = d.portfolioState as PortfolioState | undefined;
                if (ps) {
                  return (
                    <>
                      <div className={styles.subValue} style={{ marginBottom: 6 }}>PORTFOLIO</div>
                      {(() => {
                        // Defensive: payload fields may be missing/undefined for
                        // some publishers (e.g. ORB scheduler). Coerce to safe
                        // numbers so render can never throw.
                        const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
                        const totalValue = num(ps.totalValue);
                        const cash = num(ps.cash);
                        const invested = num(ps.invested);
                        const openPositions = num(ps.openPositions);
                        const unrealizedPnL = num(ps.unrealizedPnL);
                        const realizedPnlToday = num(ps.realizedPnlToday);
                        return (
                          <>
                            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '4px 12px' }}>
                              <div>
                                <div className={styles.subValue}>EQUITY</div>
                                <div className={styles.kindLabel} style={{ color: '#fff' }}>
                                  ${totalValue.toLocaleString('en-US', { maximumFractionDigits: 0 })}
                                </div>
                              </div>
                              <div>
                                <div className={styles.subValue}>CASH</div>
                                <div className={styles.kindLabel} style={{ color: '#fff' }}>
                                  ${cash.toLocaleString('en-US', { maximumFractionDigits: 0 })}
                                </div>
                              </div>
                              <div>
                                <div className={styles.subValue}>INVESTED</div>
                                <div className={styles.kindLabel} style={{ color: '#fff' }}>
                                  ${invested.toLocaleString('en-US', { maximumFractionDigits: 0 })}
                                </div>
                              </div>
                              <div>
                                <div className={styles.subValue}>POSITIONS</div>
                                <div className={styles.kindLabel} style={{ color: '#fff' }}>
                                  {openPositions}
                                </div>
                              </div>
                            </div>
                            <div style={{ marginTop: 6, display: 'flex', gap: 8 }}>
                              <div>
                                <div className={styles.subValue}>UNREAL P&L</div>
                                <div className={styles.kindLabel} style={{
                                  color: unrealizedPnL >= 0 ? colors.acidGreen : colors.red
                                }}>
                                  {unrealizedPnL >= 0 ? '+' : ''}{unrealizedPnL.toFixed(2)}
                                </div>
                              </div>
                              <div>
                                <div className={styles.subValue}>TODAY P&L</div>
                                <div className={styles.kindLabel} style={{
                                  color: realizedPnlToday >= 0 ? colors.acidGreen : colors.red
                                }}>
                                  {realizedPnlToday >= 0 ? '+' : ''}{realizedPnlToday.toFixed(2)}
                                </div>
                              </div>
                            </div>
                          </>
                        );
                      })()}
                      <div className={d.allRiskPassed !== false ? styles.executionBadge : styles.executionBadgeIdle} style={{ width: '100%', textAlign: 'center', marginTop: 6 }}>
                        {d.allRiskPassed !== false ? 'TRACKING' : 'IDLE'}
                      </div>
                    </>
                  );
                }
                return (
                  <>
                    <div className={styles.subValue} style={{ marginBottom: 6 }}>PORTFOLIO</div>
                    <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 8 }}>
                      <div>
                        <div className={styles.subValue}>EQUITY</div>
                        <div className={styles.kindLabel} style={{ color: '#fff' }}>—</div>
                      </div>
                      <div>
                        <div className={styles.subValue}>POSITIONS</div>
                        <div className={styles.kindLabel} style={{ color: '#fff' }}>0</div>
                      </div>
                    </div>
                    <div className={styles.executionBadge} style={{ width: '100%', textAlign: 'center', marginTop: 8 }}>IDLE</div>
                  </>
                );
              })()
            ) : (
              (() => {
                const trade = d.lastTrade as LastTrade | undefined;
                // Defensive: trade fields may be missing for some publishers.
                const dir = trade && typeof trade.direction === 'string' && trade.direction
                  ? trade.direction.toUpperCase()
                  : null;
                const dirLabel = dir === 'BUY' ? 'LONG' : dir === 'SELL' ? 'SHORT' : dir;
                const safeQty = trade && typeof trade.quantity === 'number' && Number.isFinite(trade.quantity)
                  ? trade.quantity
                  : null;
                const safeEntry = trade && typeof trade.entryPrice === 'number' && Number.isFinite(trade.entryPrice)
                  ? trade.entryPrice
                  : null;
                return (
                  <>
                    <div className={`${styles.routeBadge} ${trade ? styles.routeActive : ''}`} style={{ display: 'inline-block', marginBottom: 8 }}>
                      {trade ? dirLabel : '—'}
                    </div>
                    <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 8 }}>
                      <div>
                        <div className={styles.subValue}>SIZE</div>
                        <div className={styles.kindLabel} style={{ color: '#fff' }}>
                          {safeQty !== null ? safeQty.toFixed(1) : '—'}
                        </div>
                      </div>
                      <div>
                        <div className={styles.subValue}>ENTRY</div>
                        <div className={styles.kindLabel} style={{ color: '#fff' }}>
                          {safeEntry !== null ? `$${safeEntry.toFixed(2)}` : '—'}
                        </div>
                      </div>
                    </div>
                    <div className={trade && (d.allRiskPassed !== false) ? styles.executionBadge : styles.executionBadgeIdle} style={{ width: '100%', textAlign: 'center' }}>
                      {trade && (d.allRiskPassed !== false) ? 'EXECUTING' : 'READY'}
                    </div>
                  </>
                );
              })()
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

        {d.kind === 'arbiter_final' && (
          <div>
            <div className={styles.nodeTitle}>
              {((d.meta as Record<string, unknown>)?.model as string) || (d.label && d.label !== 'Arbiter' && d.label !== '-' ? d.label : '-')}
            </div>
            <div className={styles.subValue} style={{ marginTop: 2 }}>{d.provider ? d.provider.toUpperCase() : 'ENSEMBLE RESOLVER'}</div>
            <div className={styles.subValue} style={{ borderTop: `1px solid ${colors.border}`, marginTop: 8, paddingTop: 4, opacity: 0.5 }}>
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

// ── WatchlistMarquee ──────────────────────────────────────────────────────────

interface WatchlistMarqueeProps {
  tickers: WatchlistTicker[];
}

function WatchlistMarquee({ tickers }: WatchlistMarqueeProps) {
  if (tickers.length === 0) {
    return (
      <div className={styles.tickerPillsWrapper}>
        <div className={styles.tickerPillsLabel}>WATCHING</div>
        <div className={styles.kindLabel} style={{ opacity: 0.4 }}>—</div>
      </div>
    );
  }

  // Duplicate for seamless 50% loop
  const items = useMemo(() => [...tickers, ...tickers], [tickers]);
  // Speed — scale duration by set length (e.g. 2.5s per ticker)
  const duration = useMemo(() => Math.max(tickers.length * 2.5, 10), [tickers.length]);

  return (
    <div className={styles.tickerPillsWrapper}>
      <div className={styles.tickerPillsLabel}>WATCHING</div>
      <motion.div
        className={styles.tickerPillsTrack}
        animate={{ x: [0, "-50%"] }}
        transition={{
          duration: duration,
          ease: "linear",
          repeat: Infinity,
        }}
        // Force GPU acceleration to prevent subpixel jitter
        style={{ translateZ: 0 }}
      >
        {items.map((t, i) => (
          <span key={`${t.ticker}-${i}`} className={styles.tickerPill}>
            {t.ticker}
          </span>
        ))}
      </motion.div>
    </div>
  );
}

export default memo(PipelineNodeCard);
