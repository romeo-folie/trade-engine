import { useEffect, useRef, useState, useCallback } from 'react';
import type { WebEvent, SnapshotEvent, NodeState } from '../types/protocol';

export type ConnectionStatus = 'connecting' | 'connected' | 'reconnecting' | 'disconnected';

interface UseEngineSocketOptions {
  url?: string;
  reconnectDelayMs?: number;
}

export interface NodeMetrics {
  eventCount: number;
  lastEventAt: number | null; // epoch ms
}

interface EngineSocketState {
  status: ConnectionStatus;
  nodes: NodeState[];
  events: WebEvent[];
  eventsPerSecond: number;
  paused: boolean;
  setPaused: (p: boolean) => void;
  nodeMetrics: Record<string, NodeMetrics>;
}

const MAX_EVENTS = 200;
const RATE_WINDOW_MS = 5000;

export function useEngineSocket(opts: UseEngineSocketOptions = {}): EngineSocketState {
  const url = opts.url ?? buildWsUrl();
  const reconnectDelay = opts.reconnectDelayMs ?? 3000;

  const [status, setStatus] = useState<ConnectionStatus>('connecting');
  const [nodes, setNodes] = useState<NodeState[]>([]);
  const [events, setEvents] = useState<WebEvent[]>([]);
  const [eventsPerSecond, setEventsPerSecond] = useState(0);
  const [paused, setPaused] = useState(false);
  const [nodeMetrics, setNodeMetrics] = useState<Record<string, NodeMetrics>>({});

  const wsRef = useRef<WebSocket | null>(null);
  const pausedRef = useRef(paused);
  const recentTimestamps = useRef<number[]>([]);
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const shouldReconnectRef = useRef(true);

  useEffect(() => { pausedRef.current = paused; }, [paused]);

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
          setNodes((msg as SnapshotEvent).nodes);
          return;
        }

        const webEv = msg as WebEvent;
        const now = Date.now();
        recentTimestamps.current.push(now);

        setEvents(prev => [webEv, ...prev].slice(0, MAX_EVENTS));

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

  return { status, nodes, events, eventsPerSecond, paused, setPaused, nodeMetrics };
}

function buildWsUrl(): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${window.location.host}/ws`;
}
