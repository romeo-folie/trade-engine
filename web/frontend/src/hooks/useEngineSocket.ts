import { useEffect, useRef, useState, useCallback } from 'react';
import type { WebEvent, SnapshotEvent, NodeState } from '../types/protocol';

/** WebEvent with a stable monotonic ID for React keys. */
export interface IndexedWebEvent extends WebEvent {
  _seqId: number;
}

export type ConnectionStatus = 'connecting' | 'connected' | 'reconnecting' | 'disconnected';

interface UseEngineSocketOptions {
  url?: string;
  reconnectDelayMs?: number;
}

export interface NodeMetrics {
  eventCount: number;
  lastEventAt: number | null; // epoch ms
}

export interface AnalystConsensusEntry {
  verdict: string;
  confidence: number;
  provider: string;
}

export interface AnalystVote {
  provider: string;
  model: string;
  action: string;   // 'BUY' | 'SELL' | 'HOLD' | ''
  confidence: number;
  skipped: boolean;
  latency_ms: number;
}

/** A single ticker verdict for the scrolling verdict marquee. */
export interface VerdictEntry {
  ticker: string;
  action: string;   // 'BUY' | 'SELL' | 'HOLD'
  confidence: number;
}

/** Per-analyst verdict history, keyed by analyst model name. */
export type VerdictHistory = Record<string, VerdictEntry[]>;


export interface RiskCheckResult {
  name: string;
  passed: boolean;
  reason: string;
}

export interface LastTrade {
  direction: string;
  entryPrice: number;
  quantity: number;
  ticker: string;
}

export interface PortfolioState {
  totalValue: number;
  cash: number;
  invested: number;
  unrealizedPnL: number;
  realizedPnlToday: number;
  openPositions: number;
  // Count of DB rows with status='open'. Equals openPositions in steady state
  // (after Epic A consolidation work). Divergence is rendered in the canvas
  // portfolio card to surface drift or pending consolidation. May be null when
  // the source (e.g. WebSocket PortfolioSnapshot event) doesn't supply it.
  openTradeRows: number | null;
}

export interface EngineSocketState {
  status: ConnectionStatus;
  nodes: NodeState[];
  events: IndexedWebEvent[];
  eventsPerSecond: number;
  paused: boolean;
  setPaused: (p: boolean) => void;
  nodeMetrics: Record<string, NodeMetrics>;
  lastSignalAt: number | null;   // epoch ms of most recent SignalGenerated event
  avgLatencyMs: number | null;   // rolling avg latency_ms from AnalysisCompleted payloads
  analystConsensus: Record<string, AnalystConsensusEntry>;
  lastRiskChecks: RiskCheckResult[];
  lastTrade: LastTrade | null;
  portfolioState: PortfolioState | null;
  analystVotes: AnalystVote[];
  currentTicker: string | null;
  selectedAnalysts: string[];
  verdictHistory: VerdictHistory;
  consensusHistory: VerdictEntry[];
}

const MAX_EVENTS = 200;
const MAX_VERDICT_HISTORY = 50; // per analyst and for consensus
const RATE_WINDOW_MS = 5000;

export function useEngineSocket(opts: UseEngineSocketOptions = {}): EngineSocketState {
  const url = opts.url ?? buildWsUrl();
  const reconnectDelay = opts.reconnectDelayMs ?? 3000;

  const [status, setStatus] = useState<ConnectionStatus>('connecting');
  const [nodes, setNodes] = useState<NodeState[]>([]);
  const nodesRef = useRef<NodeState[]>([]);
  const nodeIdByLabelRef = useRef<Map<string, string>>(new Map());
  const [events, setEvents] = useState<IndexedWebEvent[]>([]);
  const seqRef = useRef(0);
  const [eventsPerSecond, setEventsPerSecond] = useState(0);
  const [paused, setPaused] = useState(false);
  const [nodeMetrics, setNodeMetrics] = useState<Record<string, NodeMetrics>>({});
  const [lastSignalAt, setLastSignalAt] = useState<number | null>(null);
  const latencySamplesRef = useRef<number[]>([]);
  const [avgLatencyMs, setAvgLatencyMs] = useState<number | null>(null);
  const [analystConsensus, setAnalystConsensus] = useState<Record<string, AnalystConsensusEntry>>({});
  const [lastRiskChecks, setLastRiskChecks] = useState<RiskCheckResult[]>([]);
  const [lastTrade, setLastTrade] = useState<LastTrade | null>(null);
  const [portfolioState, setPortfolioState] = useState<PortfolioState | null>(null);
  const [analystVotes, setAnalystVotes] = useState<AnalystVote[]>([]);
  const [currentTicker, setCurrentTicker] = useState<string | null>(null);
  const [selectedAnalysts, setSelectedAnalysts] = useState<string[]>([]);
  const [verdictHistory, setVerdictHistory] = useState<VerdictHistory>({});
  const [consensusHistory, setConsensusHistory] = useState<VerdictEntry[]>([]);

  const wsRef = useRef<WebSocket | null>(null);
  const pausedRef = useRef(paused);
  const recentTimestamps = useRef<number[]>([]);
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const shouldReconnectRef = useRef(true);
  // Tracks tickers seen in the current analysis cycle. When a ticker repeats,
  // it signals a new cycle has begun (works even when TickStarted is not emitted).
  const cycleTickersRef = useRef<Set<string>>(new Set());

  useEffect(() => { pausedRef.current = paused; }, [paused]);
  useEffect(() => {
    nodesRef.current = nodes;
    const next = new Map<string, string>();
    for (const node of nodes) {
      if (node.label) {
        next.set(node.label, node.id);
      }
    }
    nodeIdByLabelRef.current = next;
  }, [nodes]);

  // Seed portfolio state from REST API so the card shows live values even
  // before the first WebSocket PortfolioSnapshot event arrives.
  useEffect(() => {
    const controller = new AbortController();
    fetch('/api/node/portfolio', { signal: controller.signal })
      .then(r => r.json())
      .then(p => {
        setPortfolioState({
          totalValue: p.TotalValue ?? 0,
          cash: p.Cash ?? 0,
          invested: p.Invested ?? 0,
          unrealizedPnL: p.UnrealizedPnL ?? 0,
          realizedPnlToday: p.DayPnL ?? 0,
          openPositions: p.OpenPositions ?? 0,
          openTradeRows: p.OpenTradeRows ?? null,
        });
      })
      .catch(() => { /* non-critical */ });
    return () => controller.abort();
  }, []);

  // Rolling event-rate calculation.
  useEffect(() => {
    const id = setInterval(() => {
      const now = Date.now();
      recentTimestamps.current = recentTimestamps.current.filter(t => now - t < RATE_WINDOW_MS);
      setEventsPerSecond(recentTimestamps.current.length / (RATE_WINDOW_MS / 1000));
    }, 1000);
    return () => clearInterval(id);
  }, []);

  const connect = useCallback(() => {
    setStatus('connecting');
    const ws = new WebSocket(url);
    wsRef.current = ws;

    ws.onopen = () => setStatus('connected');

    ws.onmessage = (ev) => {
      if (pausedRef.current) return;
      try {
        const msg = JSON.parse(ev.data as string) as WebEvent | SnapshotEvent;
        if (msg.type === 'snapshot') {
          const snapNodes = (msg as SnapshotEvent).nodes;
          setNodes(snapNodes);
          // Reset incremental metrics on snapshot (e.g. reconnect) to avoid
          // double-counting with the snapshot's event_count baseline.
          setNodeMetrics({});
          // Seed portfolio state from snapshot meta if available.
          const portfolioNode = snapNodes.find(n => n.id === 'portfolio');
          if (portfolioNode?.meta) {
            const m = portfolioNode.meta as Record<string, unknown>;
            if (m.total_value != null) {
              setPortfolioState({
                totalValue: m.total_value as number,
                cash: m.cash as number,
                invested: m.invested as number,
                unrealizedPnL: m.unrealized_pnl as number,
                // day_pnl is 0 when broker unavailable (PaperExecutor); fall through to realized_pnl_today.
                realizedPnlToday: (m.day_pnl != null ? m.day_pnl as number : null) || (m.realized_pnl_today as number) || 0,
                openPositions: m.open_positions as number,
                // Snapshot meta doesn't include trade-row count today; null hides divergence in the card.
                openTradeRows: (m.open_trade_rows != null ? m.open_trade_rows as number : null),
              });
            }
          }
          return;
        }

        const webEv = msg as WebEvent;
        const now = Date.now();
        recentTimestamps.current.push(now);

        const indexed: IndexedWebEvent = { ...webEv, _seqId: ++seqRef.current };
        setEvents(prev => [indexed, ...prev].slice(0, MAX_EVENTS));

        // Update per-node metrics for source and target nodes.
        // Use a Set to avoid double-counting self-loop events (source === target).
        setNodeMetrics(prev => {
          const next = { ...prev };
          for (const nodeId of new Set([webEv.source, webEv.target])) {
            if (!nodeId) continue;
            const existing = next[nodeId] ?? { eventCount: 0, lastEventAt: null };
            next[nodeId] = {
              eventCount: existing.eventCount + 1,
              lastEventAt: now,
            };
          }
          return next;
        });

        // Track last signal time
        if (webEv.type === 'SignalGenerated') {
          setLastSignalAt(now);
        }

        // Track selected analysts for fanned-out pulses.
        // Replace (not append) on AnalysisStarted to clear stale badges from the prior cycle.
        if (webEv.type === 'AnalysisStarted') {
          setSelectedAnalysts(prev => {
            const next = new Set(prev);
            next.add(webEv.target);
            return Array.from(next);
          });
        }
        // Clear selected analysts when a new analysis cycle begins (SignalGenerated or DataFetched).
        if (webEv.type === 'SignalGenerated') {
          setSelectedAnalysts([]);
        }

        // Track avg LLM latency (rolling window of last 10 samples)
        if (webEv.type === 'AnalysisCompleted' && webEv.payload) {
          try {
            const p = typeof webEv.payload === 'string'
              ? JSON.parse(webEv.payload)
              : (webEv.payload as Record<string, unknown>);
            if (p.latency_ms != null && (p.latency_ms as number) > 0) {
              latencySamplesRef.current = [...latencySamplesRef.current, p.latency_ms as number].slice(-10);
              const avg = latencySamplesRef.current.reduce((a, b) => a + b, 0) / latencySamplesRef.current.length;
              setAvgLatencyMs(Math.round(avg));
            }
            // Accumulate analyst consensus per provider/model
            const key = (p.model as string) || (p.provider as string) || 'unknown';
            setAnalystConsensus(prev => ({
              ...prev,
              [key]: {
                verdict: p.action as string,
                confidence: p.confidence as number,
                provider: p.provider as string,
              },
            }));
          } catch { /* ignore parse errors */ }
        }

        // Track per-analyst votes and accumulate verdict history for marquee
        if (webEv.type === 'AnalystVotesPublished' && webEv.payload) {
          try {
            const p = typeof webEv.payload === 'string'
              ? JSON.parse(webEv.payload)
              : (webEv.payload as Record<string, unknown>);
            if (Array.isArray(p.votes)) {
              const ticker = p.ticker as string | undefined;
              const votes = p.votes as AnalystVote[];
              setAnalystVotes(votes);
              if (ticker) setCurrentTicker(ticker);
              const nowTs = Date.now();
              setNodeMetrics(prev => {
                const next = { ...prev };
                for (const vote of votes) {
                  if (vote.skipped) continue;
                  const nodeId = nodeIdByLabelRef.current.get(vote.model);
                  if (!nodeId) continue;
                  const existing = next[nodeId] ?? { eventCount: 0, lastEventAt: null };
                  next[nodeId] = {
                    eventCount: existing.eventCount + 1,
                    lastEventAt: nowTs,
                  };
                }
                return next;
              });

              // Accumulate per-analyst verdict history for the current cycle only.
              // A repeated ticker signals a new cycle — reset before appending so
              // the marquee stays fresh even when TickStarted is not emitted.
              if (ticker) {
                const isNewCycle = cycleTickersRef.current.has(ticker);
                if (isNewCycle) {
                  cycleTickersRef.current = new Set([ticker]);
                } else {
                  cycleTickersRef.current.add(ticker);
                }

                setVerdictHistory(prev => {
                  const base = isNewCycle ? {} : prev;
                  const next = { ...base };
                  for (const vote of votes) {
                    if (vote.skipped) continue;
                    const key = vote.model;
                    const existing = next[key] ?? [];
                    const updated = [...existing, { ticker, action: vote.action, confidence: vote.confidence }];
                    next[key] = updated.length > MAX_VERDICT_HISTORY ? updated.slice(-MAX_VERDICT_HISTORY) : updated;
                  }
                  return next;
                });

                // Build consensus-level verdict for this ticker
                const activeVotes = votes.filter(v => !v.skipped);
                if (activeVotes.length > 0) {
                  const buys = activeVotes.filter(v => v.action?.toUpperCase() === 'BUY').length;
                  const sells = activeVotes.filter(v => v.action?.toUpperCase() === 'SELL').length;
                  const action = buys > sells ? 'BUY' : sells > buys ? 'SELL' : 'HOLD';
                  const avgConf = activeVotes.reduce((s, v) => s + v.confidence, 0) / activeVotes.length;
                  setConsensusHistory(prev => {
                    const base = isNewCycle ? [] : prev;
                    const updated = [...base, { ticker, action, confidence: avgConf }];
                    return updated.length > MAX_VERDICT_HISTORY ? updated.slice(-MAX_VERDICT_HISTORY) : updated;
                  });
                }
              }
            }
          } catch { /* ignore */ }
        }

        // TickStarted — cycle boundary reset.
        // Zero out all node metrics except portfolio (which always shows latest state).
        // Also reset analyst consensus, votes, selected analysts, and risk checks
        // so cards return to their idle/standby states at the start of each tick.
        if (webEv.type === 'TickStarted') {
          setNodeMetrics(prev => {
            const portfolio = prev['portfolio'];
            const next: Record<string, NodeMetrics> = {};
            if (portfolio) next['portfolio'] = portfolio;
            return next;
          });
          setAnalystConsensus({});
          setAnalystVotes([]);
          setSelectedAnalysts([]);
          setLastRiskChecks([]);
          setCurrentTicker(null);
          setVerdictHistory({});
          setConsensusHistory([]);
          cycleTickersRef.current = new Set();
        }

        // Track risk check results
        if (webEv.type === 'RiskCheckCompleted' && webEv.payload) {
          try {
            const p = typeof webEv.payload === 'string'
              ? JSON.parse(webEv.payload)
              : (webEv.payload as Record<string, unknown>);
            if (Array.isArray(p.checks)) {
              setLastRiskChecks(p.checks as RiskCheckResult[]);
            }
          } catch { /* ignore */ }
        }

        // Track last trade
        if ((webEv.type === 'TradeExecuted' || webEv.type === 'TradeSimulated') && webEv.payload) {
          try {
            const p = typeof webEv.payload === 'string'
              ? JSON.parse(webEv.payload)
              : (webEv.payload as Record<string, unknown>);
            setLastTrade({
              direction: p.direction as string,
              entryPrice: p.entry_price as number,
              quantity: p.quantity as number,
              ticker: p.ticker as string,
            });
          } catch { /* ignore */ }
        }

        // Track portfolio snapshot
        if (webEv.type === 'PortfolioSnapshot' && webEv.payload) {
          try {
            const p = typeof webEv.payload === 'string'
              ? JSON.parse(webEv.payload)
              : (webEv.payload as Record<string, unknown>);
            setPortfolioState({
              totalValue: p.total_value as number,
              cash: p.cash as number,
              invested: p.invested as number,
              unrealizedPnL: p.unrealized_pnl as number,
              realizedPnlToday: (p.day_pnl != null ? p.day_pnl as number : null) || (p.realized_pnl_today as number) || 0,
              openPositions: p.open_positions as number,
              // PortfolioSnapshot WebSocket event doesn't carry the row count today.
              openTradeRows: (p.open_trade_rows != null ? p.open_trade_rows as number : null),
            });
          } catch { /* ignore */ }
        }
      } catch {
        // Malformed message — ignore.
      }
    };

    ws.onclose = () => {
      if (!shouldReconnectRef.current) return;
      setStatus('reconnecting');
      reconnectTimer.current = setTimeout(connect, reconnectDelay);
    };

    ws.onerror = () => {
      ws.close();
    };
  }, [url, reconnectDelay]);

  useEffect(() => {
    shouldReconnectRef.current = true;
    connect();
    return () => {
      shouldReconnectRef.current = false;
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current);
      wsRef.current?.close();
      setStatus('disconnected');
    };
  }, [connect]);

  return { status, nodes, events, eventsPerSecond, paused, setPaused, nodeMetrics, lastSignalAt, avgLatencyMs, analystConsensus, lastRiskChecks, lastTrade, portfolioState, analystVotes, currentTicker, selectedAnalysts, verdictHistory, consensusHistory };
}

function buildWsUrl(): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${window.location.host}/ws`;
}
