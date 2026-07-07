/**
 * Performance screen — signal-provenance study.
 *
 * Every closed equity entry joined back to the analysis that produced it, sliced
 * by confidence band, decision mode (fast-path / arbitrated / consensus),
 * strategy, direction and analyst count. Fetches GET
 * /api/analyses/entry-performance every 60s (closed-trade stats move slowly).
 *
 * Win rate here is ledger-direct (wins / all-closed × 100) — the same definition
 * the Portfolio screen now uses, so the two never disagree.
 */
import { useMemo } from 'react';
import useSWR from 'swr';
import {
  BarChart, Bar, Cell, XAxis, YAxis, CartesianGrid, Tooltip,
  ResponsiveContainer, LabelList, ReferenceArea,
} from 'recharts';
import { jsonFetcher } from '../hooks/swr';
import CornerCard from '../components/CornerCard';
import { TOKENS } from '../styles/tokens';
import styles from './Performance.module.css';

/* ── Types (mirror internal/store/analyses.go EntryPerformance JSON) ─────────── */

interface Bucket {
  label: string;
  n: number;
  win_rate_pct: number;
  avg_pnl_pct: number | null;
  net_pnl: number | null;
  avg_conf: number | null;
}
interface Overall {
  n: number;
  wins: number;
  losses: number;
  breakeven: number;
  win_rate_pct: number;
  avg_pnl_pct: number | null;
  median_pnl_pct: number | null;
  net_pnl: number | null;
  avg_conf: number | null;
  window_start: string | null;
  window_end: string | null;
}
interface Concentration {
  net_pnl: number | null;
  top3_pnl: number | null;
  top_ticker: string | null;
  top_strategy: string | null;
  top_pnl: number | null;
  top_pnl_pct: number | null;
  top_conf: number | null;
}
interface EntryPerformance {
  overall: Overall;
  by_confidence: Bucket[];
  by_mode: Bucket[];
  by_strategy: Bucket[];
  by_direction: Bucket[];
  by_analysts: Bucket[];
  concentration: Concentration;
  confidence_gate: number;
}

/* ── Formatting ──────────────────────────────────────────────────────────────── */

const reducedMotion =
  typeof window !== 'undefined' &&
  window.matchMedia?.('(prefers-reduced-motion: reduce)').matches;

function fmtUsd(n: number | null | undefined): string {
  if (n == null) return '—';
  const sign = n > 0 ? '+' : n < 0 ? '−' : '';
  return `${sign}$${Math.abs(n).toLocaleString('en-US', { maximumFractionDigits: 0 })}`;
}
function fmtPct(n: number | null | undefined, signed = false): string {
  if (n == null) return '—';
  const sign = signed && n > 0 ? '+' : n < 0 ? '−' : '';
  return `${sign}${Math.abs(n).toFixed(1)}%`;
}
function pnlClass(n: number | null | undefined): string {
  if (n == null) return '';
  if (n > 0) return styles.pos;
  if (n < 0) return styles.neg;
  return '';
}
function signColor(n: number | null | undefined): string {
  if (n == null) return 'var(--border)';
  return n >= 0 ? 'var(--accent)' : 'var(--red)';
}

/** Lower bound of a confidence band label ("0.68-0.75" → 0.68, "<0.60" → 0, ">=0.85" → 0.85). */
function bandLowerBound(label: string): number {
  if (label.startsWith('<')) return 0;
  if (label.startsWith('>=')) return parseFloat(label.slice(2));
  return parseFloat(label.split('-')[0]) || 0;
}

const MODE_LABELS: Record<string, string> = {
  fast_path: 'Fast-path',
  arbitrated: 'Arbitrated',
  consensus: 'Consensus',
};
function prettyLabel(dim: 'mode' | 'strategy' | 'direction' | 'analysts', raw: string): string {
  if (dim === 'mode') return MODE_LABELS[raw] ?? raw;
  if (dim === 'analysts') return `${raw} analyst${raw === '1' ? '' : 's'}`;
  return raw.charAt(0).toUpperCase() + raw.slice(1);
}

const swrOpts = { refreshInterval: 60_000 } as const;

/* ── Chart tooltip ───────────────────────────────────────────────────────────── */

interface TooltipProps {
  active?: boolean;
  payload?: { payload: Bucket & { edge: boolean } }[];
}
function ConfTooltip({ active, payload }: TooltipProps) {
  if (!active || !payload?.length) return null;
  const d = payload[0].payload;
  return (
    <div className={styles.tooltip}>
      <div className={styles.tooltipHead}>conf {d.label}</div>
      <div><span className={styles.tooltipKey}>win rate</span> {fmtPct(d.win_rate_pct)}</div>
      <div><span className={styles.tooltipKey}>trades</span> {d.n}</div>
      <div><span className={styles.tooltipKey}>avg P&amp;L</span> {fmtPct(d.avg_pnl_pct, true)}</div>
    </div>
  );
}

/* ── Breakdown table ─────────────────────────────────────────────────────────── */

function BreakdownTable({
  title, caption, rows, dim, highlightBest,
}: {
  title: string;
  caption: string;
  rows: Bucket[];
  dim: 'mode' | 'strategy' | 'direction' | 'analysts';
  highlightBest?: boolean;
}) {
  // Meter scaled to a 70% ceiling so the ~60% max reads near-full without implying 100%.
  const bestLabel = highlightBest && rows.length
    ? rows.reduce((a, b) => (b.win_rate_pct > a.win_rate_pct ? b : a)).label
    : null;
  return (
    <CornerCard bracketColor="var(--border)" className={styles.tableCard}>
      <div className={styles.tableTitle}>{title}</div>
      <table className={styles.table}>
        <thead>
          <tr>
            <th>{dim === 'mode' ? 'Decision mode' : dim === 'strategy' ? 'Strategy' : dim === 'direction' ? 'Direction' : 'Analysts'}</th>
            <th className={styles.num}>n</th>
            <th className={styles.win}>Win rate</th>
            <th className={styles.num}>Net</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.label} className={r.label === bestLabel ? styles.bestRow : undefined}>
              <td>{prettyLabel(dim, r.label)}</td>
              <td className={styles.num}>{r.n}</td>
              <td className={styles.win}>
                <span className={styles.meter}>
                  <span className={styles.meterTrack}>
                    <span
                      className={styles.meterFill}
                      style={{ width: `${Math.min(100, (r.win_rate_pct / 70) * 100)}%` }}
                    />
                  </span>
                  <span className={styles.meterNum}>{fmtPct(r.win_rate_pct)}</span>
                </span>
              </td>
              <td className={`${styles.num} ${pnlClass(r.net_pnl)}`}>{fmtUsd(r.net_pnl)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className={styles.caption}>{caption}</p>
    </CornerCard>
  );
}

/* ── Screen ──────────────────────────────────────────────────────────────────── */

export default function Performance() {
  const { data, error, isLoading } = useSWR<EntryPerformance>(
    '/api/analyses/entry-performance', jsonFetcher, swrOpts,
  );

  const gate = data?.confidence_gate ?? 0.68;

  const confData = useMemo(
    () => (data?.by_confidence ?? []).map((b) => ({ ...b, edge: bandLowerBound(b.label) >= gate })),
    [data, gate],
  );
  // Shade the sub-threshold ("dead money") bands as one region.
  const deadBands = confData.filter((d) => !d.edge);
  const deadStart = deadBands[0]?.label;
  const deadEnd = deadBands[deadBands.length - 1]?.label;

  const o = data?.overall;
  const c = data?.concentration;
  // Concentration warning: is the single best trade carrying most of the net book?
  const topShare = c?.top_pnl && c?.net_pnl && c.net_pnl > 0 ? c.top_pnl / c.net_pnl : 0;
  const topHeavy = topShare >= 0.5;

  return (
    <div className={styles.screen}>
      <div className={styles.headerRow}>
        <div className={styles.breadcrumb}>
          <span className={styles.breadcrumbBrand}>~/</span>
          <span className={styles.breadcrumbPage}>PERFORMANCE</span>
        </div>
        <div className={styles.autoRefresh}>
          <span className={styles.greenDot} />
          AUTO-REFRESH 60s
        </div>
      </div>

      <h1 className={styles.pageTitle}>PERFORMANCE</h1>
      <p className={styles.subtitle}>
        Closed equity entries by signal provenance — confidence, decision mode, strategy and analyst count.
        {o?.window_start && (
          <> {' '}<span className={styles.subDim}>
            {o.n} trades · {o.window_start} → {o.window_end} · entry gate {gate.toFixed(2)}
          </span></>
        )}
      </p>

      {error && (
        <div className={styles.banner} role="alert">⚠ Failed to load performance data</div>
      )}
      {isLoading && !data && (
        <div className={styles.banner} role="status" aria-label="Loading performance">Loading…</div>
      )}
      {data && o && o.n === 0 && (
        <div className={styles.banner} role="status">No closed equity trades yet — nothing to measure.</div>
      )}

      {data && o && o.n > 0 && (
        <>
          {topHeavy && c && (
            <CornerCard bracketColor="var(--amber)" className={styles.warnCard}>
              <span className={styles.warnTag}>READ FIRST</span>
              <span className={styles.warnText}>
                Top-heavy book — <strong>{c.top_ticker}</strong> ({prettyLabel('strategy', c.top_strategy ?? '')},
                conf {c.top_conf?.toFixed(3)}) is {fmtUsd(c.top_pnl)} of {fmtUsd(c.net_pnl)} net
                ({Math.round(topShare * 100)}%). Trust win rate over dollar totals at this sample size.
              </span>
            </CornerCard>
          )}

          {/* Headline KPIs */}
          <div className={styles.summaryRow}>
            <CornerCard bracketColor={o.win_rate_pct >= 50 ? 'var(--accent)' : 'var(--amber)'} className={styles.summaryCard}>
              <span className={styles.summaryLabel}>WIN RATE</span>
              <span className={styles.summaryValue}>{fmtPct(o.win_rate_pct)}</span>
              <span className={styles.summaryFoot}>{o.wins}W · {o.losses}L{o.breakeven ? ` · ${o.breakeven}BE` : ''} of {o.n}</span>
            </CornerCard>
            <CornerCard bracketColor={signColor(o.avg_pnl_pct)} className={styles.summaryCard}>
              <span className={styles.summaryLabel}>AVG P&amp;L / TRADE</span>
              <span className={`${styles.summaryValue} ${pnlClass(o.avg_pnl_pct)}`}>{fmtPct(o.avg_pnl_pct, true)}</span>
              <span className={styles.summaryFoot}>median {fmtPct(o.median_pnl_pct, true)}</span>
            </CornerCard>
            <CornerCard bracketColor={signColor(o.net_pnl)} className={styles.summaryCard}>
              <span className={styles.summaryLabel}>NET REALIZED</span>
              <span className={`${styles.summaryValue} ${pnlClass(o.net_pnl)}`}>{fmtUsd(o.net_pnl)}</span>
              <span className={styles.summaryFoot}>top 3 = {fmtUsd(c?.top3_pnl)}</span>
            </CornerCard>
            <CornerCard bracketColor="var(--border)" className={styles.summaryCard}>
              <span className={styles.summaryLabel}>AVG CONFIDENCE</span>
              <span className={styles.summaryValue}>{o.avg_conf?.toFixed(3) ?? '—'}</span>
              <span className={styles.summaryFoot}>gate {gate.toFixed(2)}</span>
            </CornerCard>
          </div>

          {/* Confidence → win rate */}
          <CornerCard bracketColor="var(--border)" className={styles.chartCard}>
            <div className={styles.tableTitle}>Win rate by confidence band</div>
            <div
              className={styles.chartWrap}
              role="img"
              aria-label={`Win rate by confidence: ${confData.map((d) => `${d.label} ${d.win_rate_pct.toFixed(0)} percent`).join(', ')}. Entry gate ${gate.toFixed(2)}.`}
            >
              <ResponsiveContainer width="100%" height={260}>
                <BarChart data={confData} margin={{ top: 24, right: 12, left: -8, bottom: 4 }}>
                  <CartesianGrid stroke={TOKENS.borderMuted} vertical={false} />
                  {deadStart && (
                    <ReferenceArea x1={deadStart} x2={deadEnd} fill={TOKENS.red} fillOpacity={0.05} strokeOpacity={0} />
                  )}
                  <XAxis
                    dataKey="label"
                    tick={{ fontFamily: 'var(--font-mono)', fontSize: 10, fill: TOKENS.textFaint }}
                    axisLine={false} tickLine={false}
                  />
                  <YAxis
                    unit="%" width={38} domain={[0, (max: number) => Math.ceil((max + 10) / 10) * 10]}
                    tick={{ fontFamily: 'var(--font-mono)', fontSize: 10, fill: TOKENS.textFaint }}
                    axisLine={false} tickLine={false}
                  />
                  <Tooltip content={<ConfTooltip />} cursor={{ fill: 'rgba(255,255,255,0.03)' }} />
                  <Bar dataKey="win_rate_pct" radius={[3, 3, 0, 0]} isAnimationActive={!reducedMotion} maxBarSize={90}>
                    {confData.map((d) => (
                      <Cell key={d.label} fill={d.edge ? TOKENS.emerald : TOKENS.red} fillOpacity={d.edge ? 0.9 : 0.55} />
                    ))}
                    <LabelList
                      dataKey="win_rate_pct" position="top"
                      formatter={(v: number) => `${v.toFixed(0)}%`}
                      style={{ fontFamily: 'var(--font-mono)', fontSize: 11, fill: 'var(--text-muted)' }}
                    />
                  </Bar>
                </BarChart>
              </ResponsiveContainer>
            </div>
            <div className={styles.legendRow}>
              <span className={styles.legendItem}><span className={styles.swatchDead} /> below gate · dead money</span>
              <span className={styles.legendItem}><span className={styles.swatchEdge} /> at/above gate · edge zone</span>
            </div>
            <p className={styles.caption}>
              Win rate climbs monotonically with the arbiter's final confidence. Bands left of the {gate.toFixed(2)} entry
              gate have historically been dead money; the edge turns on at the gate.
            </p>
          </CornerCard>

          {/* Breakdowns */}
          <div className={styles.grid2}>
            <BreakdownTable
              title="By decision mode" dim="mode" rows={data.by_mode}
              caption="Arbitration fires on analyst disagreement, so arbitrated trades carry the lowest confidence — mode is a symptom, confidence is the cause."
            />
            <BreakdownTable
              title="By strategy" dim="strategy" rows={data.by_strategy} highlightBest
              caption="Momentum leads on win rate; news_sentiment is high-volume and low-accuracy."
            />
            <BreakdownTable
              title="By direction" dim="direction" rows={data.by_direction}
              caption="Long vs short entries."
            />
            <BreakdownTable
              title="By analyst count" dim="analysts" rows={data.by_analysts}
              caption="Almost every entry runs the 3-analyst ensemble — too little variance to compare depth retroactively."
            />
          </div>

          <p className={styles.footNote}>
            Ledger-direct: win rate = wins / all-closed × 100 over closed equity trades joined to their analysis.
            ORB/forex excluded. Open positions not counted. Small sample — treat non-confidence cuts as suggestive.
          </p>
        </>
      )}
    </div>
  );
}
