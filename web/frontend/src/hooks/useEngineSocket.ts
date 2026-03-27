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
}

const MAX_EVENTS = 200;
const RATE_WINDOW_MS = 5000;

export function useEngineSocket(opts: UseEngineSocketOptions = {}): EngineSocketState {
  const url = opts.url ?? buildWsUrl();
  const reconnectDelay = opts.reconnectDelayMs ?? 3000;

  const [status, setStatus] = useState<ConnectionStatus>('connecting');
  const [nodes, setNodes] = useState<NodeState[]>([]);
  const nodesRef = useRef<NodeState[]>([]);
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

  const wsRef = useRef<WebSocket | null>(null);
  const pausedRef = useRef(paused);
  const recentTimestamps = useRef<number[]>([]);
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const shouldReconnectRef = useRef(true);

  useEffect(() => { pausedRef.current = paused; }, [paused]);
  useEffect(() => { nodesRef.current = nodes; }, [nodes]);

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
                realizedPnlToday: (m.realized_pnl_today as number) ?? 0,
                openPositions: m.open_positions as number,
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

        // Track per-analyst votes
        if (webEv.type === 'AnalystVotesPublished' && webEv.payload) {
          try {
            const p = typeof webEv.payload === 'string'
              ? JSON.parse(webEv.payload)
              : (webEv.payload as Record<string, unknown>);
            if (Array.isArray(p.votes)) {
              const votes = p.votes as AnalystVote[];
              setAnalystVotes(votes);
              const nowTs = Date.now();
              setNodeMetrics(prev => {
                const next = { ...prev };
                for (const vote of votes) {
                  if (vote.skipped) continue;
                  const node = nodesRef.current.find(n => n.label === vote.model);
                  if (!node) continue;
                  const existing = next[node.id] ?? { eventCount: 0, lastEventAt: null };
                  next[node.id] = {
                    eventCount: existing.eventCount + 1,
                    lastEventAt: nowTs,
                  };
                }
                return next;
              });
            }
          } catch { /* ignore */ }
        }

        // Reset analyst consensus on new tick
        if (webEv.type === 'DataFetched') {
          setAnalystConsensus({});
          setAnalystVotes([]);
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
              realizedPnlToday: (p.realized_pnl_today as number) ?? 0,
              openPositions: p.open_positions as number,
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

  return { status, nodes, events, eventsPerSecond, paused, setPaused, nodeMetrics, lastSignalAt, avgLatencyMs, analystConsensus, lastRiskChecks, lastTrade, portfolioState, analystVotes };
}

function buildWsUrl(): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${window.location.host}/ws`;
}
