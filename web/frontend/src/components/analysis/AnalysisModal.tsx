import { useEffect, useRef, useState } from 'react';
import type { AnalysisRecord, AnalystResult } from '../../screens/Analysis';
import { ChamferedBox } from '../common/ChamferedBox';
import CornerCard from '../CornerCard';
import { colors } from '../../styles/tokens';
import { verdictColor } from '../../utils/verdict';
import {
  ANALYSIS_MODAL_FOCUSABLE_SELECTOR,
  decisionSummary,
  formatIndicator,
  shouldShowEntryChecks,
  titleCase,
} from './analysisDetailFormat';
import styles from './AnalysisModal.module.css';

const ANALYST_PALETTE = [
  colors.analystTechnical,
  colors.analystMomentum,
  colors.analystSentiment,
  colors.analystMacro,
];

function analystBracketColor(index: number, active: boolean): string {
  if (!active) return 'var(--border)';
  return ANALYST_PALETTE[index % ANALYST_PALETTE.length];
}

function analystBarColor(index: number): string {
  return ANALYST_PALETTE[index % ANALYST_PALETTE.length];
}

interface AnalysisModalProps {
  analysis: AnalysisRecord;
  onClose: () => void;
}

function actionColors(action: string) {
  const a = action.toUpperCase();
  if (a === 'BUY') return colors.acidGreen;
  if (a === 'SELL') return colors.red;
  return colors.amber;
}

function confClass(action: string): string {
  const a = action.toUpperCase();
  if (a === 'BUY') return styles.confBuy;
  if (a === 'SELL') return styles.confSell;
  return styles.confHold;
}

function barClass(action: string): string {
  const a = action.toUpperCase();
  if (a === 'BUY') return styles.barBuy;
  if (a === 'SELL') return styles.barSell;
  return styles.barHold;
}

function fmtDateTime(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  const y = d.getFullYear();
  const mo = String(d.getMonth() + 1).padStart(2, '0');
  const da = String(d.getDate()).padStart(2, '0');
  const h = String(d.getHours()).padStart(2, '0');
  const mi = String(d.getMinutes()).padStart(2, '0');
  const s = String(d.getSeconds()).padStart(2, '0');
  return `${y}-${mo}-${da} ${h}:${mi}:${s}`;
}

function fmtPrice(v: number): string {
  if (!v) return '—';
  return v.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

interface AnalysisDetail {
  ArbiterReasoning?: string;
  AnalystResults?: AnalystResult[] | null;
  Indicators?: IndicatorSnapshot;
  SelectedLens?: string;
  RejectedLenses?: Record<string, string>;
  Regime?: string;
  LensSelectionStatus?: string;
  TimeframeWorkflow?: TimeframeWorkflow;
  RelatedSignalAction?: string;
  RelatedSignalStatus?: string;
  RelatedSignalBlockReason?: string;
  EntrySetup?: string;
  EntrySetupEvidence?: string;
  EntryGateDecisions?: EntryGateDecision[];
}

interface IndicatorSnapshot {
  EMA20?: number;
  EMA50?: number;
  EMA200?: number;
  SMA20?: number;
  SMA50?: number;
  SMA200?: number;
  RSI?: number;
  MACDLine?: number;
  MACDSignal?: number;
  MACDHistogram?: number;
  BBUpper?: number;
  BBMiddle?: number;
  BBLower?: number;
  ADX?: number;
  ATR?: number;
  OBV?: number;
  StochK?: number;
  StochD?: number;
  VWAP?: number;
  VolSMA20?: number;
  VolRatio?: number;
}

interface TimeframePass {
  timeframe: string;
  status?: string;
  action?: string;
  confidence?: number;
  selected_lens?: string;
  reason?: string;
}

interface TimeframeWorkflow {
  routing_source: string;
  mode: string;
  primary: TimeframePass;
  confirmation?: TimeframePass;
  final_action: string;
}

interface EntryGateDecision {
  name: string;
  status: string;
  reason?: string;
}

function formatConfidence(value?: number): string {
  return value == null ? '' : ` · ${(value * 100).toFixed(0)}%`;
}


export default function AnalysisModal({ analysis, onClose }: AnalysisModalProps) {
  const panelRef = useRef<HTMLDivElement>(null);
  // Heavy JSONB fields (ArbiterReasoning, AnalystResults) are trimmed from
  // /api/analyses/recent for payload efficiency — we lazy-fetch them via
  // /api/analyses/{id} on modal open. detailLoading drives the "loading…"
  // placeholders; null detail means we haven't fetched yet for this id.
  const [detail, setDetail] = useState<AnalysisDetail | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);

  useEffect(() => {
    const controller = new AbortController();
    setDetail(null);
    setDetailLoading(true);
    fetch(`/api/analyses/${encodeURIComponent(analysis.ID)}`, { signal: controller.signal })
      .then(r => (r.ok ? r.json() : null))
      .then((data: AnalysisDetail | null) => {
        if (data) setDetail(data);
        setDetailLoading(false);
      })
      .catch(err => {
        // Aborts (closing the modal or switching analyses mid-flight) are
        // expected — only surface real failures.
        if ((err as { name?: string }).name !== 'AbortError') setDetailLoading(false);
      });
    return () => controller.abort();
  }, [analysis.ID]);

  useEffect(() => {
    function handleKey(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        onClose();
        return;
      }
      if (e.key === 'Tab' && panelRef.current) {
        const focusable = panelRef.current.querySelectorAll<HTMLElement>(
          ANALYSIS_MODAL_FOCUSABLE_SELECTOR,
        );
        if (focusable.length === 0) return;
        const first = focusable[0];
        const last = focusable[focusable.length - 1];
        if (e.shiftKey) {
          if (document.activeElement === first) { e.preventDefault(); last?.focus(); }
        } else {
          if (document.activeElement === last) { e.preventDefault(); first?.focus(); }
        }
      }
    }
    document.addEventListener('keydown', handleKey);
    panelRef.current?.focus();
    return () => document.removeEventListener('keydown', handleKey);
  }, [onClose]);

  const confPct = (analysis.Confidence * 100).toFixed(0);
  const action = analysis.Action.toUpperCase();
  const analystResults = detail?.AnalystResults ?? [];
  const decisionReasoning = decisionSummary(
    analysis.Reasoning,
    detail?.ArbiterReasoning,
    analystResults,
  );
  const rejectedLenses = Object.entries(detail?.RejectedLenses ?? {});
  const availableEntryChecks = detail?.EntryGateDecisions ?? [];
  const entryChecks = shouldShowEntryChecks(detail?.RelatedSignalAction, availableEntryChecks.length)
    ? availableEntryChecks
    : [];

  // DissentSummary is not independent content: the arbiter prompt asks the
  // model to BEGIN its reasoning with "DISSENT:" (ensemble.go), and
  // extractDissent stores everything after that marker. It is therefore always
  // a substring of the reasoning body rendered below — printing it again
  // duplicated the entire summary. Surface it as a header badge instead, so the
  // "analysts disagreed" signal survives without the repetition.
  const hasDissent = !!analysis.DissentSummary;

  return (
    <>
      <div className={styles.backdrop} onClick={onClose} />
      <div
        className={styles.panel}
        ref={panelRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-label={`Analysis detail for ${analysis.Ticker}`}
      >
        {/* Header */}
        <div className={styles.header}>
          <div className={styles.headerLeft}>
            <div className={styles.headerTopRow}>
              <span className={styles.title}>{analysis.Ticker}</span>
              <ChamferedBox
                chamfer={6}
                centerContent
                backgroundColor={actionColors(analysis.Action)}
                borderColor={actionColors(analysis.Action)}
                style={{
                  padding: '2px 10px',
                  display: 'inline-flex',
                  alignItems: 'center',
                  justifyContent: 'center'
                }}
              >
                <span className={styles.badgeText}>
                  {action}
                </span>
              </ChamferedBox>
            </div>
            {analysis.CompanyName && (
              <span className={styles.companyName}>{analysis.CompanyName}</span>
            )}
            <span className={styles.headerMeta}>
              {fmtDateTime(analysis.CreatedAt)} · {analysis.Strategy.replace(/_/g, ' ')} · {analysis.ModelProvider} / {analysis.ModelName}
            </span>
          </div>
          <button type="button" className={styles.closeBtn} onClick={onClose} aria-label="Close panel">
            ×
          </button>
        </div>

        {/* Confidence */}
        <div className={styles.confSection}>
          <span className={styles.confLabel}>CONFIDENCE</span>
          <span className={`${styles.confValue} ${confClass(analysis.Action)}`}>{confPct}%</span>
          <div className={styles.confBarTrack}>
            <div
              className={`${styles.confBarFill} ${barClass(analysis.Action)}`}
              style={{ width: `${Math.min(Number(confPct), 100)}%` }}
            />
          </div>
        </div>

        {/* Body */}
        <div className={styles.body}>
          <div className={styles.section}>
            <div className={styles.sectionLabel}>
              DECISION SUMMARY
              {hasDissent && <span className={styles.dissentLabel}>DISSENT</span>}
            </div>
            <div className={styles.decisionMeta}>
              {detail?.SelectedLens && (
                <span className={styles.metaChip}>Lens: {titleCase(detail.SelectedLens)}</span>
              )}
              {detail?.Regime && (
                <span className={styles.metaChip}>Regime: {titleCase(detail.Regime)}</span>
              )}
              {detail?.LensSelectionStatus && detail.LensSelectionStatus !== 'absent' && (
                <span className={styles.metaChip}>Selection: {titleCase(detail.LensSelectionStatus)}</span>
              )}
            </div>
            <div className={styles.reasoning}>{decisionReasoning || 'No decision reasoning was recorded.'}</div>
            {detailLoading && !detail && (
              <div className={styles.detailLoading} aria-live="polite">Loading structured evidence…</div>
            )}
            {detail?.TimeframeWorkflow && (
              <div className={styles.workflow}>
                <div className={styles.workflowRow}>
                  <span>Primary · {detail.TimeframeWorkflow.primary.timeframe}</span>
                  <strong>
                    {detail.TimeframeWorkflow.primary.action || action}
                    {formatConfidence(detail.TimeframeWorkflow.primary.confidence)}
                  </strong>
                </div>
                {detail.TimeframeWorkflow.confirmation && (
                  <div className={styles.workflowRow}>
                    <span>Entry · {detail.TimeframeWorkflow.confirmation.timeframe}</span>
                    <strong>{titleCase(detail.TimeframeWorkflow.confirmation.status || 'not requested')}</strong>
                  </div>
                )}
              </div>
            )}
            {rejectedLenses.length > 0 && (
              <details className={styles.disclosure}>
                <summary>{rejectedLenses.length} rejected {rejectedLenses.length === 1 ? 'lens' : 'lenses'}</summary>
                <div className={styles.rejectionList}>
                  {rejectedLenses.map(([lens, reason]) => (
                    <div key={lens} className={styles.rejectionRow}>
                      <strong>{titleCase(lens)}</strong>
                      <span>{reason}</span>
                    </div>
                  ))}
                </div>
              </details>
            )}
          </div>

          {detail?.Indicators && (
            <div className={styles.section}>
              <div className={styles.sectionLabel}>TECHNICAL EVIDENCE</div>
              <div className={styles.evidenceGroups}>
                <div className={styles.evidenceGroup}>
                  <span className={styles.evidenceLabel}>Trend</span>
                  <span>EMA 20/50/200</span>
                  <strong>{formatIndicator(detail.Indicators.EMA20)} / {formatIndicator(detail.Indicators.EMA50)} / {formatIndicator(detail.Indicators.EMA200)}</strong>
                  <span>SMA 20/50/200</span>
                  <strong>{formatIndicator(detail.Indicators.SMA20)} / {formatIndicator(detail.Indicators.SMA50)} / {formatIndicator(detail.Indicators.SMA200)}</strong>
                  <span>ADX 14</span>
                  <strong>{formatIndicator(detail.Indicators.ADX)}</strong>
                </div>
                <div className={styles.evidenceGroup}>
                  <span className={styles.evidenceLabel}>Momentum</span>
                  <span>RSI 14</span>
                  <strong>{formatIndicator(detail.Indicators.RSI)}</strong>
                  <span>MACD line / signal / histogram</span>
                  <strong>{formatIndicator(detail.Indicators.MACDLine, 4)} / {formatIndicator(detail.Indicators.MACDSignal, 4)} / {formatIndicator(detail.Indicators.MACDHistogram, 4)}</strong>
                  <span>Stochastic K / D</span>
                  <strong>{formatIndicator(detail.Indicators.StochK)} / {formatIndicator(detail.Indicators.StochD)}</strong>
                </div>
                <div className={styles.evidenceGroup}>
                  <span className={styles.evidenceLabel}>Volatility and volume</span>
                  <span>Bollinger upper / middle / lower</span>
                  <strong>{formatIndicator(detail.Indicators.BBUpper)} / {formatIndicator(detail.Indicators.BBMiddle)} / {formatIndicator(detail.Indicators.BBLower)}</strong>
                  <span>ATR 14</span>
                  <strong>{formatIndicator(detail.Indicators.ATR, 4)}</strong>
                  <span>VWAP</span>
                  <strong>{formatIndicator(detail.Indicators.VWAP)}</strong>
                  <span>OBV</span>
                  <strong>{formatIndicator(detail.Indicators.OBV, 0)}</strong>
                  <span>Volume SMA 20 / ratio</span>
                  <strong>{formatIndicator(detail.Indicators.VolSMA20, 0)} / {formatIndicator(detail.Indicators.VolRatio)}×</strong>
                </div>
              </div>
            </div>
          )}

          {detail?.EntrySetupEvidence && (
            <div className={styles.section}>
              <div className={styles.sectionLabel}>MARKET STRUCTURE</div>
              <div className={styles.structureFinding}>{detail.EntrySetupEvidence}</div>
            </div>
          )}

          {/* Per-analyst breakdown — lazy-loaded with the rest of the detail payload. */}
          {analystResults.length > 0 && (
            <div className={styles.section}>
              <div className={styles.sectionLabel}>
                ANALYST BREAKDOWN{analysis.FastPathed ? ' · FAST-PATH' : ''}
              </div>
              <div className={styles.analystList}>
                {analystResults.map((a, idx) => (
                  <CornerCard
                    key={a.name}
                    bracketColor={analystBracketColor(idx, a.active)}
                    bracketSize={10}
                    bracketWidth={1}
                    className={`${styles.analystCard} ${!a.active ? styles.bypassed : ''}`}
                  >
                    <div className={styles.analystHeader}>
                      <div>
                        <div className={styles.analystName}>{a.name}</div>
                        {a.model && (
                          <div className={styles.analystModel}>{a.model}</div>
                        )}
                      </div>
                      <span
                        className={styles.analystVerdict}
                        style={{ color: a.active ? verdictColor(a.verdict) : 'var(--text-faint)' }}
                      >
                        {a.active ? a.verdict : 'BYPASSED'}
                      </span>
                    </div>
                    {a.active && (
                      <div className={styles.analystBarTrack}>
                        <div
                          className={styles.analystBar}
                          style={{ background: analystBarColor(idx), width: `${a.confidence * 100}%` }}
                        />
                      </div>
                    )}
                    {a.description && (
                      <details className={styles.disclosure}>
                        <summary>View analyst reasoning</summary>
                        <div className={styles.analystDesc}>{a.description}</div>
                      </details>
                    )}
                  </CornerCard>
                ))}
              </div>
            </div>
          )}

          {entryChecks.length > 0 && (
            <div className={styles.section}>
              <div className={styles.sectionLabel}>ENTRY CHECKS</div>
              <div className={styles.entryContext}>
                <span>{titleCase(detail?.RelatedSignalStatus || 'evaluated')}</span>
                {detail?.RelatedSignalBlockReason && <span>{detail.RelatedSignalBlockReason}</span>}
              </div>
              <div className={styles.checkList}>
                {entryChecks.map((check, index) => (
                  <div key={`${check.name}-${index}`} className={styles.checkRow}>
                    <span className={`${styles.checkStatus} ${styles[`check_${check.status}`] ?? ''}`}>
                      {titleCase(check.status)}
                    </span>
                    <div>
                      <strong>{titleCase(check.name)}</strong>
                      {check.reason && <span>{check.reason}</span>}
                    </div>
                  </div>
                ))}
              </div>
            </div>
          )}

          {/* Signal Parameters */}
          <div className={styles.section}>
            <div className={styles.sectionLabel}>SIGNAL PARAMETERS</div>
            <div className={styles.kvList}>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Action</span>
                <ChamferedBox
                  chamfer={4}
                  centerContent
                  backgroundColor={actionColors(analysis.Action)}
                  borderColor={actionColors(analysis.Action)}
                  style={{
                    padding: '1px 8px',
                    display: 'inline-flex',
                    alignItems: 'center',
                    justifyContent: 'center'
                  }}
                >
                  <span className={styles.kvBadgeText}>{action}</span>
                </ChamferedBox>
              </div>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Stop Loss</span>
                <span className={`${styles.kvVal} ${styles.valRed}`}>
                  {analysis.StopLoss ? fmtPrice(analysis.StopLoss) : '—'}
                </span>
              </div>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Take Profit</span>
                <span className={`${styles.kvVal} ${styles.valGreen}`}>
                  {analysis.TakeProfit ? fmtPrice(analysis.TakeProfit) : '—'}
                </span>
              </div>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Size %</span>
                <span className={styles.kvVal}>{analysis.SizePct.toFixed(1)}%</span>
              </div>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Strategy</span>
                <span className={styles.kvVal}>{titleCase(detail?.SelectedLens || analysis.Strategy)}</span>
              </div>
            </div>
          </div>

          {/* Model Info */}
          <div className={styles.section}>
            <div className={styles.sectionLabel}>MODEL INFO</div>
            <div className={styles.kvList}>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Model</span>
                <span className={styles.kvVal}>{analysis.ModelName}</span>
              </div>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Input tokens</span>
                <span className={styles.kvVal}>{analysis.InputTokens.toLocaleString()}</span>
              </div>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Output tokens</span>
                <span className={styles.kvVal}>{analysis.OutputTokens.toLocaleString()}</span>
              </div>
              <div className={styles.kvRow}>
                <span className={styles.kvKey}>Latency</span>
                <span className={styles.kvVal}>{analysis.LatencyMs.toLocaleString()}ms</span>
              </div>
            </div>
          </div>
        </div>
      </div>
    </>
  );
}
