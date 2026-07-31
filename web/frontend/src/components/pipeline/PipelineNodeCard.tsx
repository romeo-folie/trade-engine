import { memo, useMemo } from 'react';
import { Handle, Position, type NodeProps, type Node } from '@xyflow/react';
import type { NodeMetrics } from '../../hooks/useEngineSocket';
import EnsembleConsensus from './EnsembleConsensus';
import { ChamferedBox } from '../common/ChamferedBox';
import { colors } from '../../styles/tokens';
import styles from './PipelineNodeCard.module.css';
import type { WatchlistTicker } from '../../hooks/useWatchlist';
import type { RiskCheckResult, LastTrade, PortfolioState, VerdictEntry } from '../../hooks/useEngineSocket';

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


  const isArbiterFinal = d.kind === 'arbiter_final';
  const isRiskSpecialist = d.kind === 'risk_specialist';
  const isTicker = d.kind === 'ticker';
  const cardBorder = (isArbiterFinal || isRiskSpecialist) ? colors.amber
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
          <span className={styles.kindLabel} style={{ color: isAnalyst ? accentColor : (isArbiterFinal || isRiskSpecialist) ? colors.amber : undefined }}>
            {isArbiterFinal ? 'ARBITER' : isRiskSpecialist ? 'RISK SPECIALIST' : d.kind.toUpperCase()}
          </span>
          <div className={`${styles.statusDot} ${statusDot}`} />
        </div>

        {d.kind !== 'arbiter_final' && (
          <div className={styles.nodeTitle}>{d.label}</div>
        )}

        {/* ── Outcome Template ── */}
        {d.kind === 'outcome' && (
          <div className={styles.sourceContent}>
            <div className={styles.subValue}>
              {d.nodeId === 'approvals'
                // Server-supplied: the queue holds signals awaiting a human in
                // manual mode, but first-tick-deferred entries in auto/paper.
                ? ((d.meta as Record<string, unknown> | undefined)?.caption as string | undefined) ?? 'PENDING'
                : 'RISK OUTCOME'}
            </div>
            <div className={styles.metricValue}>
              {d.eventCount > 0 ? d.eventCount.toLocaleString() : '—'}
            </div>
            <div className={styles.feedStatusRow}>
              {/* Neither risk outcome is healthy live activity, so neither takes
                  the acid-green badge: Blocked is a rejection (red), and a
                  populated queue is work parked awaiting release (amber). */}
              <span className={
                !nodeActive ? styles.feedBadgeIdle
                  : d.nodeId === 'blocked_signals' ? styles.feedBadgeBlocked
                    : d.nodeId === 'approvals' ? styles.feedBadgePending
                      : styles.feedBadgeActive
              }>
                {!nodeActive ? 'IDLE'
                  : d.nodeId === 'approvals'
                    ? ((d.meta as Record<string, unknown> | undefined)?.active_label as string | undefined) ?? 'PENDING'
                    : 'BLOCKED'}
              </span>
            </div>
          </div>
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
            {/* Identify the analyst by model, never by provider. Card titles
                differ by mode — specialist cards are titled by role
                ("Technical"), ensemble cards by model — so this line is
                suppressed when the title already carries the model name. */}
            {(() => {
              const model = (d.meta as Record<string, unknown> | undefined)?.model as string | undefined;
              if (!model) {
                return <div className={styles.subValue} style={{ fontSize: 9, marginBottom: 8 }}>LLM ENSEMBLE</div>;
              }
              if (model.toLowerCase() === d.label.toLowerCase()) return null;
              return <div className={styles.subValue} style={{ fontSize: 9, marginBottom: 8 }}>{model}</div>;
            })()}
            {(() => {
              const entries = d.verdictEntries as VerdictEntry[] | undefined;
              const hasVerdicts = entries && entries.length > 0;
              const isActive = d.isChosen || hasVerdicts;
              return (
                <>
                  {isActive ? (
                    <div className={styles.feedStatusRow} style={{ marginTop: 8 }}>
                      <span className={styles.feedBadgeActive} style={{ background: 'rgba(57, 255, 20, 0.15)', border: '1px solid #39FF14', boxShadow: '0 0 10px rgba(57, 255, 20, 0.2)' }}>
                        ACTIVE
                      </span>
                    </div>
                  ) : (
                    <div className={styles.feedStatusRow} style={{ marginTop: 8 }}>
                      <span className={styles.feedBadgeIdle}>STANDBY</span>
                    </div>
                  )}
                  {hasVerdicts ? (
                    <VerdictMarquee entries={entries} />
                  ) : d.eventCount > 0 && d.isChosen ? (
                    <div className={styles.kindLabel} style={{ opacity: 0.5, marginTop: 10 }}>
                      {d.eventCount} ANALYSES
                    </div>
                  ) : null}
                </>
              );
            })()}
          </div>
        )}

        {isRiskSpecialist && (
          <div>
            {/* Model only — the provider line and the static role description
                were redundant with the card's own RISK SPECIALIST heading. */}
            <div className={styles.subValue}>
              {((d.meta as Record<string, unknown>)?.model as string) || 'MODEL NOT CONFIGURED'}
            </div>
            <div className={styles.feedStatusRow} style={{ marginTop: 8 }}>
              <span className={nodeActive ? styles.feedBadgeActive : styles.feedBadgeIdle}>
                {nodeActive ? 'REVIEWING' : 'MANDATORY'}
              </span>
            </div>
            {d.verdict ? (
              <div
                className={`${styles.analystVerdict} ${
                  d.verdict === 'BUY' ? styles.verdictLong :
                  d.verdict === 'SELL' ? styles.verdictShort :
                  styles.verdictNeutral
                }`}
                style={{ fontSize: 13, marginTop: 8 }}
              >
                {typeof d.currentTicker === 'string' && <span style={{ opacity: 0.6, fontSize: 9, marginRight: 6 }}>{d.currentTicker}</span>}
                {d.verdict as string}
                {typeof d.voteConfidence === 'number' && <span style={{ marginLeft: 8 }}>{d.voteConfidence.toFixed(2)}</span>}
              </div>
            ) : null}
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
                const PROVIDER_COLOR: Record<string, string> = {
                  openai:    colors.analystMomentum,
                  anthropic: colors.analystTechnical,
                  google:    colors.analystSentiment,
                  deepseek:  colors.analystMacro,
                };
                const PROVIDER_LABEL: Record<string, string> = {
                  openai:    'GPT',
                  anthropic: 'ANTH',
                  google:    'GEMI',
                  deepseek:  'DEEP',
                };

                // Drive consensus display entirely from analystVotes (last AnalystVotesPublished event).
                // analystConsensus accumulates across tickers/ticks and is stale — don't use it here.
                const av = d.analystVotes as { model: string; provider: string; action: string; confidence: number; skipped: boolean }[] | undefined;
                const activeVotes = av ? av.filter(v => !v.skipped) : [];

                // Aggregate per provider: multiple analysts on the same provider → one bar, averaged confidence.
                const confByProvider = new Map<string, number[]>();
                activeVotes.forEach(vote => {
                  const key = vote.provider?.toLowerCase() || 'unknown';
                  if (!confByProvider.has(key)) confByProvider.set(key, []);
                  confByProvider.get(key)!.push(vote.confidence);
                });
                const items = Array.from(confByProvider.entries()).map(([key, confs]) => ({
                  id: key,
                  label: PROVIDER_LABEL[key] ?? (key.slice(0, 4).toUpperCase() || 'UNKN'),
                  value: confs.reduce((s, c) => s + c, 0) / confs.length,
                  color: PROVIDER_COLOR[key] ?? colors.textDim,
                  isLoading: false,
                }));

                const ticker = d.currentTicker as string | null;
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
                      {consensusVerdict ? <>{ticker && <span style={{ opacity: 0.6, fontSize: 10, marginRight: 6 }}>{ticker}</span>}{consensusVerdict} <span style={{ opacity: 1, fontSize: 14, marginLeft: 8 }}>{avgConf.toFixed(2)}</span></> : (nodeActive ? 'CONSULTING...' : '—')}
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
                      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '4px 12px' }}>
                        <div>
                          <div className={styles.subValue}>EQUITY</div>
                          <div className={styles.kindLabel} style={{ color: '#fff' }}>
                            ${ps.totalValue.toLocaleString('en-US', { maximumFractionDigits: 0 })}
                          </div>
                        </div>
                        <div>
                          <div className={styles.subValue}>CASH</div>
                          <div className={styles.kindLabel} style={{ color: '#fff' }}>
                            ${ps.cash.toLocaleString('en-US', { maximumFractionDigits: 0 })}
                          </div>
                        </div>
                        <div>
                          <div className={styles.subValue}>INVESTED</div>
                          <div className={styles.kindLabel} style={{ color: '#fff' }}>
                            ${ps.invested.toLocaleString('en-US', { maximumFractionDigits: 0 })}
                          </div>
                        </div>
                        <div>
                          <div className={styles.subValue}>POSITIONS</div>
                          {/*
                            Render the canonical distinct-ticker count.
                            When the underlying DB row count diverges, surface it
                            in parentheses so drift is immediately visible to the
                            operator (Issue #315). After Epic A (#311/#312/#313)
                            is fully deployed, the two numbers should agree in
                            steady state and the parenthetical will be hidden.
                          */}
                          <div
                            className={styles.kindLabel}
                            style={{ color: '#fff' }}
                            title="Distinct positions (ticker + direction) / total open trade rows. Divergence indicates pending consolidation work."
                            aria-label="Distinct positions (ticker + direction) / total open trade rows. Divergence indicates pending consolidation work."
                          >
                            {ps.openPositions ?? '—'}
                            {ps.openPositions != null && ps.openTradeRows != null && ps.openTradeRows !== ps.openPositions && (
                              <span style={{ color: colors.acidGreen, marginLeft: 4, fontSize: 9 }}>
                                ({ps.openTradeRows} rows)
                              </span>
                            )}
                          </div>
                        </div>
                      </div>
                      <div style={{ marginTop: 6, display: 'flex', gap: 8 }}>
                        <div>
                          <div className={styles.subValue}>UNREAL P&L</div>
                          <div className={styles.kindLabel} style={{
                            color: ps.unrealizedPnL == null
                              ? colors.textDim
                              : ps.unrealizedPnL >= 0 ? colors.acidGreen : colors.red
                          }}>
                            {ps.unrealizedPnL == null
                              ? '—'
                              : `${ps.unrealizedPnL >= 0 ? '+' : ''}${ps.unrealizedPnL.toFixed(2)}`}
                          </div>
                        </div>
                        <div>
                          <div className={styles.subValue}>TODAY P&L</div>
                          <div className={styles.kindLabel} style={{
                            color: ps.realizedPnlToday >= 0 ? colors.acidGreen : colors.red
                          }}>
                            {ps.realizedPnlToday >= 0 ? '+' : ''}{ps.realizedPnlToday.toFixed(2)}
                          </div>
                        </div>
                      </div>
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
                const dir = trade ? trade.direction.toUpperCase() : null;
                const dirLabel = dir === 'BUY' ? 'LONG' : dir === 'SELL' ? 'SHORT' : dir;
                return (
                  <>
                    <div className={`${styles.routeBadge} ${trade ? styles.routeActive : ''}`} style={{ display: 'inline-block', marginBottom: 8 }}>
                      {trade ? dirLabel : '—'}
                    </div>
                    <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 8 }}>
                      <div>
                        <div className={styles.subValue}>SIZE</div>
                        <div className={styles.kindLabel} style={{ color: '#fff' }}>
                          {trade ? trade.quantity.toFixed(1) : '—'}
                        </div>
                      </div>
                      <div>
                        <div className={styles.subValue}>ENTRY</div>
                        <div className={styles.kindLabel} style={{ color: '#fff' }}>
                          {trade ? `$${trade.entryPrice.toFixed(2)}` : '—'}
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

// ── VerdictMarquee ───────────────────────────────────────────────────────────

interface VerdictMarqueeProps {
  entries: VerdictEntry[];
}

// Pills overflow the card at roughly 6 entries — only scroll beyond that.
const SCROLL_THRESHOLD = 6;

function VerdictMarquee({ entries }: VerdictMarqueeProps) {
  const shouldScroll = entries.length > SCROLL_THRESHOLD;

  // Only build the duplicated set when we actually need to scroll.
  const scrollItems = useMemo(() => {
    if (!shouldScroll) return entries;
    return [...entries, ...entries];
  }, [entries, shouldScroll]);

  const duration = useMemo(() => Math.max(entries.length * 3, 10), [entries.length]);

  const pills = (items: VerdictEntry[], keyPrefix = '') =>
    items.map((e, i) => {
      const pillColor = e.action === 'BUY' ? colors.emerald : e.action === 'SELL' ? colors.red : colors.amber;
      const pillBg = e.action === 'BUY' ? 'rgba(57, 255, 20, 0.1)' : e.action === 'SELL' ? 'rgba(239, 68, 68, 0.1)' : 'rgba(245, 158, 11, 0.1)';
      return (
        <span
          key={`${keyPrefix}${e.ticker}-${i}`}
          className={styles.verdictPill}
          style={{ background: pillBg, borderColor: pillColor, color: '#fff' }}
        >
          {e.ticker}
        </span>
      );
    });

  return (
    <div className={styles.tickerPillsWrapper}>
      <div className={styles.tickerPillsLabel}>VERDICTS</div>
      {shouldScroll ? (
        <div
          className={`${styles.tickerPillsTrack} ${styles.marqueeTrack}`}
          style={{ animationDuration: `${duration}s` }}
        >
          {pills(scrollItems)}
        </div>
      ) : (
        <div className={styles.tickerPillsTrack} style={{ flexWrap: 'wrap' }}>
          {pills(entries)}
        </div>
      )}
    </div>
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
      <div
        className={`${styles.tickerPillsTrack} ${styles.marqueeTrack}`}
        style={{ animationDuration: `${duration}s` }}
      >
        {items.map((t, i) => (
          <span key={`${t.ticker}-${i}`} className={styles.tickerPill}>
            {t.ticker}
          </span>
        ))}
      </div>
    </div>
  );
}

export default memo(PipelineNodeCard);
