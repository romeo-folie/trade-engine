import { type ReactNode } from 'react';
import { motion, AnimatePresence } from 'framer-motion';
import useSWR from 'swr';

// ── Node display names (hoisted — used before function definitions below) ─────

const NODE_LABELS: Record<string, string> = {
  alpaca: 'Alpaca',
  polymarket: 'Polymarket',
  kalshi: 'Kalshi',
  news_api: 'News Feed',
  data_enrichment: 'Data Enrichment',
  indicator_engine: 'Indicator Computation',
  prompt_assembly: 'Prompt Assembly',
  llm_analysis: 'LLM Analysis',
  consensus: 'Consensus Resolution',
  risk_manager: 'Risk Manager',
  executor: 'Trade Executor',
  blocked_signals: 'Blocked Signals',
  approvals: 'Queued Approvals',
  portfolio: 'Portfolio',
  engine: 'Engine Core',
  error_bus: 'Error Bus',
};

const NODE_DESCRIPTIONS: Record<string, string> = {
  risk_manager: 'Active risk checks — daily loss, drawdown, position limits',
  portfolio: 'Current portfolio value, positions, and P&L',
  llm_analysis: 'Model performance — accuracy, latency, trade count',
  consensus: 'Multi-model consensus resolution statistics',
  executor: 'Recent executed trades and fills',
  blocked_signals: 'Signals blocked by risk checks',
  approvals: 'Signals queued for manual approval',
};

// Pipeline node IDs that route to dedicated panels — must NOT fall through to TickerPanel
const PIPELINE_NODE_IDS = new Set(Object.keys(NODE_LABELS));

// ── Types aligned with the actual Go API wire format ──────────────────────────
// Envelope keys (open_trades, open_trade_count, model_performance, ticker, signals)
// are snake_case — explicitly set by the Go handlers in handlers.go.
// Nested struct fields are PascalCase — Go's default JSON serialization for
// exported fields with no json struct tags (e.g. Trade, Signal, ModelPerformanceRow,
// PortfolioSnapshot).
// ──────────────────────────────────────────────────────────────────────────────

interface RiskData {
  open_trades: Array<{
    Ticker: string;
    Direction: string;
    PnLPct: number;
    EntryPrice: number;
    Status: string;
  }>;
  open_trade_count: number;
  // Distinct (ticker, direction) count — equals open_trade_count in steady state
  // after Epic A consolidation (#311/#312/#315). Optional for backwards compat.
  open_positions?: number;
}

interface PortfolioData {
  TotalValue?: number;
  Cash?: number;
  Invested?: number;
  UnrealizedPnL?: number;
  RealizedPnLToday?: number;
  OpenPositions?: number;
  // Count of DB rows where status='open'. Equals OpenPositions in steady state
  // after Epic A consolidation. Divergence is surfaced in the card below to
  // make pending consolidation immediately visible.
  // Backend returns *int which serializes to JSON null when nil (omitempty
   // would also drop the key entirely), so the wire format is number | null |
   // undefined. Consumers below use loose `== null` checks to handle both.
   OpenTradeRows?: number | null;
}

interface ModelPerf {
  Provider: string;
  Model: string;
  TotalRecommendations: number;
  AvgConfidence: number;
  TradeCount: number;
  AvgPnLPct?: number;
  WinningTrades: number;
  LosingTrades: number;
  WinRate?: number;
}

interface TickerData {
  ticker: string;
  // Go handler always emits these keys; values are null when no data exists yet.
  // The analysis object is a store.StoredAnalysis — its verdict field is Action
  // (buy/sell/wait/…), NOT Direction (that's a trades-only field).
  analysis: {
    Action: string;
    Confidence: number;
    Reasoning?: string;
    CreatedAt?: string;
  } | null;
  signals: Array<{
    Ticker: string;
    Action: string;
    Confidence: number;
    Status: string;
    CreatedAt: string;
  }> | null;
}

interface SignalItem {
  Ticker: string;
  Action: string;
  Confidence: number;
  Status: string;
  CreatedAt: string;
}

interface TradeItem {
  Ticker: string;
  Direction: string;
  Quantity: number;
  EntryPrice: number;
  PnLPct: number;
  CreatedAt: string;
}

// ── Panel component ──────────────────────────────────────────────────────────

interface DrillDownPanelProps {
  nodeId: string | null;
  onClose: () => void;
}

export function DrillDownPanel({ nodeId, onClose }: DrillDownPanelProps) {
  // Conditional fetch: null key skips the fetch entirely when no node selected.
  // SWR pauses the 10s interval when the tab is hidden.
  const { data, error, isLoading: loading } = useSWR<unknown>(
    nodeId || null,
    (id: string) => fetchNodeData(id),
    { refreshInterval: 10_000 },
  );
  const errorMsg = error ? String(error) : null;

  return (
    <AnimatePresence>
      {nodeId && (
        <motion.aside
          key="panel"
          initial={{ x: '100%', opacity: 0 }}
          animate={{ x: 0, opacity: 1 }}
          exit={{ x: '100%', opacity: 0 }}
          transition={{ type: 'spring', stiffness: 300, damping: 30 }}
          style={{
            position: 'absolute', right: 0, top: 0, bottom: 0,
            width: 380,
            background: 'linear-gradient(180deg, #0e0c2a 0%, #0a0820 100%)',
            borderLeft: '1px solid #2d2b55',
            overflowY: 'auto',
            fontFamily: '"JetBrains Mono", "Fira Code", monospace',
            zIndex: 10,
          }}
        >
          {/* Header */}
          <div style={{
            display: 'flex', justifyContent: 'space-between', alignItems: 'center',
            padding: '16px 20px',
            borderBottom: '1px solid #1e1b35',
            position: 'sticky', top: 0,
            background: '#0e0c2a',
            zIndex: 1,
          }}>
            <div>
              <div style={{ color: '#00f5ff', fontSize: 13, fontWeight: 700, letterSpacing: '0.03em' }}>
                {NODE_LABELS[nodeId] ?? nodeId}
              </div>
              <div style={{ color: '#4b4b6f', fontSize: 10, marginTop: 2 }}>
                {NODE_DESCRIPTIONS[nodeId] ?? 'Pipeline node'}
              </div>
            </div>
            <button
              onClick={onClose}
              style={{
                background: '#1a1848',
                border: '1px solid #2d2b55',
                borderRadius: 6,
                color: '#6b6b8f',
                cursor: 'pointer',
                fontSize: 14,
                width: 28, height: 28,
                display: 'flex', alignItems: 'center', justifyContent: 'center',
              }}
            >
              ×
            </button>
          </div>

          {/* Content */}
          <div style={{ padding: '16px 20px' }}>
            {loading && <Skeleton />}
            {errorMsg && <ErrorCard message={errorMsg} />}
            {!loading && !errorMsg && data != null && renderNodeContent(nodeId, data)}
            {!loading && !errorMsg && !data && (
              <div style={{ color: '#4b4b6f', fontSize: 12, textAlign: 'center', marginTop: 40 }}>
                No data available
              </div>
            )}
          </div>
        </motion.aside>
      )}
    </AnimatePresence>
  );
}

// ── Node-specific renderers ──────────────────────────────────────────────────

function renderNodeContent(nodeId: string, data: unknown): ReactNode {
  switch (nodeId) {
    case 'risk_manager':
      return <RiskPanel data={data as RiskData} />;
    case 'portfolio':
      return <PortfolioPanel data={data as PortfolioData} />;
    case 'executor':
      return <TradesPanel data={(data as { trades: TradeItem[] }).trades} />;
    case 'blocked_signals':
    case 'approvals':
      return <SignalsPanel data={(data as { signals: SignalItem[] }).signals} />;
    case 'llm_analysis':
    case 'consensus':
      return <ModelsPanel data={(data as { model_performance: ModelPerf[] }).model_performance} />;
    default:
      // Ticker nodes fall through here.
      if (isTicker(nodeId, data)) return <TickerPanel data={data as TickerData} />;
      return <RawPanel data={data} />;
  }
}

// ── Risk Panel ────────────────────────────────────────────────────────────────

function RiskPanel({ data }: { data: RiskData }) {
  // Prefer the distinct-ticker count when available — that's the gate's
  // canonical denominator. Show the row count in parentheses only when the two
  // diverge so steady state stays a single number (Issue #315).
  const distinct = data.open_positions ?? data.open_trade_count;
  const rows = data.open_trade_count;
  const value = distinct === rows
    ? String(distinct)
    : `${distinct} (${rows} rows)`;
  return (
    <div>
      <StatCard
        label="Open Positions"
        value={value}
        color="#a855f7"
        title="Distinct positions (ticker + direction) / total open trade rows. Divergence indicates pending consolidation work."
      />
      {data.open_trades?.length > 0 && (
        <div style={{ marginTop: 16 }}>
          <SectionLabel>Active Trades</SectionLabel>
          {data.open_trades.map((t, i) => (
            <TradeRow key={i} ticker={t.Ticker} direction={t.Direction} pnl={t.PnLPct} price={t.EntryPrice} />
          ))}
        </div>
      )}
    </div>
  );
}

// ── Portfolio Panel ──────────────────────────────────────────────────────────

function PortfolioPanel({ data }: { data: PortfolioData }) {
  // Mirror the canvas card: show plain count when distinct tickers and DB rows
  // agree (Epic A steady state); show "N (M rows)" when they diverge so
  // pending consolidation is immediately visible (Issue #315).
  const distinct = data.OpenPositions ?? 0;
  const rows = data.OpenTradeRows;
  const positionsValue = (rows == null || rows === distinct)
    ? String(distinct)
    : `${distinct} (${rows} rows)`;
  return (
    <div>
      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10 }}>
        <StatCard label="Total Value" value={fmt$(data.TotalValue)} color="#22c55e" />
        <StatCard label="Cash" value={fmt$(data.Cash)} color="#00b4ff" />
        <StatCard label="Invested" value={fmt$(data.Invested)} color="#a855f7" />
        <StatCard label="Unrealized P&L" value={fmtPnl(data.UnrealizedPnL)} color={pnlColor(data.UnrealizedPnL)} />
      </div>
      <div style={{ marginTop: 10 }}>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10 }}>
          <StatCard label="Realized Today" value={fmtPnl(data.RealizedPnLToday)} color={pnlColor(data.RealizedPnLToday)} />
          <StatCard
            label="Open Positions"
            value={positionsValue}
            color="#6b6b8f"
            title="Distinct positions (ticker + direction) / total open trade rows. Divergence indicates pending consolidation work."
          />
        </div>
      </div>
    </div>
  );
}

// ── Models Panel ─────────────────────────────────────────────────────────────

function ModelsPanel({ data }: { data: ModelPerf[] }) {
  if (!data?.length) return <EmptyState text="No model performance data yet" />;
  return (
    <div>
      {data.map((m, i) => {
        const winRate = m.WinRate != null ? m.WinRate * 100 : undefined;
        return (
          <div key={i} style={{
            background: '#12103a',
            border: '1px solid #2d2b45',
            borderRadius: 8,
            padding: '12px 14px',
            marginBottom: 10,
          }}>
            <div style={{ color: '#e2e0ff', fontSize: 12, fontWeight: 600 }}>
              {m.Provider}/{m.Model}
            </div>
            <div style={{ display: 'flex', gap: 16, marginTop: 8 }}>
              <MiniStat label="Win Rate" value={fmtPct(winRate)} color={accuracyColor(winRate)} />
              <MiniStat label="Trades" value={String(m.TradeCount)} color="#6b6b8f" />
              <MiniStat label="Avg Conf" value={`${(m.AvgConfidence * 100).toFixed(0)}%`} color="#a855f7" />
            </div>
            <div style={{ display: 'flex', gap: 16, marginTop: 4 }}>
              <MiniStat label="W/L" value={`${m.WinningTrades}/${m.LosingTrades}`} color="#6b6b8f" />
              {m.AvgPnLPct != null && (
                <MiniStat label="Avg P&L" value={fmtPct(m.AvgPnLPct)} color={pnlColor(m.AvgPnLPct)} />
              )}
            </div>
            {/* Win rate bar */}
            {winRate != null && (
              <div style={{
                marginTop: 8,
                height: 4,
                background: '#1e1b35',
                borderRadius: 2,
                overflow: 'hidden',
              }}>
                <motion.div
                  initial={{ width: 0 }}
                  animate={{ width: `${Math.min(100, winRate)}%` }}
                  transition={{ duration: 0.8, ease: 'easeOut' }}
                  style={{
                    height: '100%',
                    background: accuracyColor(winRate),
                    borderRadius: 2,
                  }}
                />
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}

// ── Ticker Panel ─────────────────────────────────────────────────────────────

function TickerPanel({ data }: { data: TickerData }) {
  return (
    <div>
      {data.analysis && (
        <div style={{
          background: '#12103a',
          border: '1px solid #2d2b45',
          borderRadius: 8,
          padding: '14px',
          marginBottom: 16,
        }}>
          <SectionLabel>Latest Analysis</SectionLabel>
          <div style={{ display: 'flex', gap: 12, marginTop: 8 }}>
            <DirectionBadge direction={data.analysis.Action} />
            <div>
              <div style={{ color: '#e2e0ff', fontSize: 11 }}>
                Confidence: <span style={{ color: '#a855f7' }}>{(data.analysis.Confidence * 100).toFixed(0)}%</span>
              </div>
              {data.analysis.CreatedAt && (
                <div style={{ color: '#4b4b6f', fontSize: 10, marginTop: 2 }}>
                  {new Date(data.analysis.CreatedAt).toLocaleString()}
                </div>
              )}
            </div>
          </div>
          {data.analysis.Reasoning && (
            <div style={{
              color: '#8b8aaf',
              fontSize: 10,
              marginTop: 10,
              lineHeight: 1.5,
              maxHeight: 120,
              overflow: 'auto',
            }}>
              {data.analysis.Reasoning}
            </div>
          )}
        </div>
      )}

      {data.signals && data.signals.length > 0 && (
        <div>
          <SectionLabel>Recent Signals</SectionLabel>
          {data.signals.map((s, i) => (
            <div key={i} style={{
              display: 'flex', justifyContent: 'space-between', alignItems: 'center',
              padding: '6px 0',
              borderBottom: '1px solid #1e1b3520',
            }}>
              <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                <DirectionBadge direction={s.Action} small />
                <span style={{ color: '#6b6b8f', fontSize: 10 }}>{(s.Confidence * 100).toFixed(0)}%</span>
              </div>
              <StatusBadge status={s.Status} />
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

// ── Trades Panel ─────────────────────────────────────────────────────────────

function TradesPanel({ data }: { data: TradeItem[] }) {
  if (!data?.length) return <EmptyState text="No recent trades" />;
  return (
    <div>
      {data.map((t, i) => (
        <TradeRow key={i} ticker={t.Ticker} direction={t.Direction} pnl={t.PnLPct} price={t.EntryPrice} qty={t.Quantity} />
      ))}
    </div>
  );
}

// ── Signals Panel ────────────────────────────────────────────────────────────

function SignalsPanel({ data }: { data: SignalItem[] }) {
  if (!data?.length) return <EmptyState text="No recent signals" />;
  return (
    <div>
      {data.map((s, i) => (
        <div key={i} style={{
          display: 'flex', justifyContent: 'space-between', alignItems: 'center',
          padding: '8px 0',
          borderBottom: '1px solid #1e1b3520',
        }}>
          <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            <span style={{ color: '#e2e0ff', fontSize: 12, fontWeight: 600 }}>{s.Ticker}</span>
            <DirectionBadge direction={s.Action} small />
          </div>
          <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            <span style={{ color: '#6b6b8f', fontSize: 10 }}>{(s.Confidence * 100).toFixed(0)}%</span>
            <StatusBadge status={s.Status} />
          </div>
        </div>
      ))}
    </div>
  );
}

// ── Raw fallback ─────────────────────────────────────────────────────────────

function RawPanel({ data }: { data: unknown }) {
  return (
    <pre style={{
      color: '#8b8aaf',
      fontSize: 10,
      whiteSpace: 'pre-wrap',
      wordBreak: 'break-word',
      background: '#12103a',
      border: '1px solid #2d2b45',
      borderRadius: 8,
      padding: 14,
    }}>
      {JSON.stringify(data, null, 2)}
    </pre>
  );
}

// ── Shared UI atoms ──────────────────────────────────────────────────────────

function StatCard({ label, value, color, title }: { label: string; value: string | number; color: string; title?: string }) {
  return (
    <div
      style={{
        background: '#12103a',
        border: '1px solid #2d2b45',
        borderRadius: 8,
        padding: '12px 14px',
      }}
      title={title}
      aria-label={title}
    >
      <div style={{ color: '#6b6b8f', fontSize: 9, textTransform: 'uppercase', letterSpacing: '0.06em' }}>
        {label}
      </div>
      <div style={{ color, fontSize: 20, fontWeight: 700, marginTop: 4 }}>
        {value}
      </div>
    </div>
  );
}

function MiniStat({ label, value, color }: { label: string; value: string; color: string }) {
  return (
    <div>
      <div style={{ color: '#4b4b6f', fontSize: 9, textTransform: 'uppercase' }}>{label}</div>
      <div style={{ color, fontSize: 11, fontWeight: 600 }}>{value}</div>
    </div>
  );
}

function SectionLabel({ children }: { children: ReactNode }) {
  return (
    <div style={{
      color: '#6b6b8f',
      fontSize: 10,
      textTransform: 'uppercase',
      letterSpacing: '0.08em',
      fontWeight: 600,
      marginBottom: 8,
    }}>
      {children}
    </div>
  );
}

function DirectionBadge({ direction, small }: { direction: string; small?: boolean }) {
  const d = direction?.toLowerCase();
  const isBuy = d === 'buy' || d === 'long';
  return (
    <span style={{
      background: isBuy ? '#22c55e20' : '#ef444420',
      color: isBuy ? '#22c55e' : '#ef4444',
      border: `1px solid ${isBuy ? '#22c55e40' : '#ef444440'}`,
      borderRadius: 4,
      padding: small ? '1px 6px' : '2px 8px',
      fontSize: small ? 9 : 10,
      fontWeight: 600,
      textTransform: 'uppercase',
      letterSpacing: '0.04em',
    }}>
      {direction}
    </span>
  );
}

function StatusBadge({ status }: { status: string }) {
  const colors: Record<string, string> = {
    active: '#22c55e', executed: '#22c55e', filled: '#22c55e',
    blocked: '#6b7280', expired: '#6b7280',
    pending: '#f59e0b', queued: '#f59e0b',
  };
  const c = colors[status?.toLowerCase()] ?? '#6b6b8f';
  return (
    <span style={{ color: c, fontSize: 9, textTransform: 'uppercase', fontWeight: 500 }}>
      {status}
    </span>
  );
}

function TradeRow({ ticker, direction, pnl, price, qty }: {
  ticker: string; direction: string; pnl?: number; price?: number; qty?: number;
}) {
  return (
    <div style={{
      display: 'flex', justifyContent: 'space-between', alignItems: 'center',
      padding: '8px 0',
      borderBottom: '1px solid #1e1b3520',
    }}>
      <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
        <span style={{ color: '#e2e0ff', fontSize: 12, fontWeight: 600 }}>{ticker}</span>
        <DirectionBadge direction={direction} small />
        {qty != null && <span style={{ color: '#4b4b6f', fontSize: 10 }}>×{qty}</span>}
      </div>
      <div style={{ textAlign: 'right' }}>
        {price != null && <div style={{ color: '#6b6b8f', fontSize: 10 }}>${price.toFixed(2)}</div>}
        {pnl != null && <div style={{ color: pnlColor(pnl), fontSize: 10, fontWeight: 600 }}>{fmtPct(pnl)}</div>}
      </div>
    </div>
  );
}

function Skeleton() {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 12, marginTop: 8 }}>
      {[1, 2, 3].map(i => (
        <motion.div
          key={i}
          animate={{ opacity: [0.3, 0.6, 0.3] }}
          transition={{ duration: 1.2, repeat: Infinity, delay: i * 0.15 }}
          style={{ height: 48, background: '#1e1b35', borderRadius: 8 }}
        />
      ))}
    </div>
  );
}

function ErrorCard({ message }: { message: string }) {
  return (
    <div style={{
      background: '#ef444415',
      border: '1px solid #ef444440',
      borderRadius: 8,
      padding: '12px 14px',
      color: '#ef4444',
      fontSize: 11,
    }}>
      {message}
    </div>
  );
}

function EmptyState({ text }: { text: string }) {
  return (
    <div style={{ color: '#4b4b6f', fontSize: 12, textAlign: 'center', marginTop: 40 }}>
      {text}
    </div>
  );
}

// ── Data helpers ─────────────────────────────────────────────────────────────

function fmt$(n?: number): string {
  if (n == null) return '—';
  return '$' + n.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

function fmtPnl(n?: number): string {
  if (n == null) return '—';
  const sign = n >= 0 ? '+' : '';
  return `${sign}$${n.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}

function fmtPct(n?: number): string {
  if (n == null) return '—';
  const sign = n >= 0 ? '+' : '';
  return `${sign}${n.toFixed(2)}%`;
}

function pnlColor(n?: number): string {
  if (n == null) return '#6b6b8f';
  return n >= 0 ? '#22c55e' : '#ef4444';
}

function accuracyColor(n?: number): string {
  if (n == null) return '#6b6b8f';
  if (n >= 60) return '#22c55e';
  if (n >= 45) return '#f59e0b';
  return '#ef4444';
}

function isTicker(nodeId: string, data: unknown): boolean {
  // Only treat as a ticker if the node is NOT a known pipeline node AND
  // the response carries a `ticker` key (i.e. it came from the ticker endpoint).
  return !PIPELINE_NODE_IDS.has(nodeId)
    && typeof data === 'object'
    && data !== null
    && 'ticker' in data;
}

// ── API ──────────────────────────────────────────────────────────────────────

async function fetchNodeData(nodeId: string, signal?: AbortSignal): Promise<unknown> {
  const endpointMap: Record<string, string> = {
    risk_manager:    '/api/node/risk',
    portfolio:       '/api/node/portfolio',
    llm_analysis:    '/api/node/models',
    executor:        '/api/trades/recent?limit=20',
    blocked_signals: '/api/signals/recent?limit=20',
    approvals:       '/api/signals/recent?limit=20',
    consensus:       '/api/node/models',
  };
  const url = endpointMap[nodeId] ?? `/api/node/ticker/${nodeId}`;
  const res = await fetch(url, { signal });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return res.json();
}
