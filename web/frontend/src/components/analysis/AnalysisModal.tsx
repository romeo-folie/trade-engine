import { useEffect, useRef, useState } from 'react';
import type { AnalysisRecord, AnalystResult } from '../../screens/Analysis';
import { ChamferedBox } from '../common/ChamferedBox';
import CornerCard from '../CornerCard';
import { colors } from '../../styles/tokens';
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

function verdictColor(verdict: string): string {
  if (verdict === 'LONG') return 'var(--accent)';
  if (verdict === 'SHORT') return 'var(--red)';
  return 'var(--amber)';
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
          'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
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
          {/* Full Reasoning — prefer arbiter text (loaded from /api/analyses/{id});
              fall back to top-level Reasoning while detail is loading or absent. */}
          <div className={styles.section}>
            <div className={styles.sectionLabel}>
              FULL REASONING
              {hasDissent && <span className={styles.dissentLabel}>DISSENT</span>}
            </div>
            {detail?.ArbiterReasoning ? (
              <div className={styles.reasoning}>{detail.ArbiterReasoning}</div>
            ) : detailLoading ? (
              <div className={styles.reasoning} aria-busy="true">Loading reasoning…</div>
            ) : (
              <div className={styles.reasoning}>{analysis.Reasoning}</div>
            )}
          </div>

          {/* Per-analyst breakdown — lazy-loaded with the rest of the detail payload. */}
          {detail?.AnalystResults && detail.AnalystResults.length > 0 && (
            <div className={styles.section}>
              <div className={styles.sectionLabel}>
                ANALYST BREAKDOWN{analysis.FastPathed ? ' · FAST-PATH' : ''}
              </div>
              <div className={styles.analystList}>
                {detail.AnalystResults.map((a, idx) => (
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
                    {a.description && <div className={styles.analystDesc}>{a.description}</div>}
                  </CornerCard>
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
                <span className={styles.kvVal}>{analysis.Strategy}</span>
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
