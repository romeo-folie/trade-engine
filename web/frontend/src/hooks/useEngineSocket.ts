import { useEffect, useRef, useState, useCallback } from 'react';
import type { WebEvent, SnapshotEvent, NodeState } from '../types/protocol';

export type ConnectionStatus = 'connecting' | 'connected' | 'reconnecting' | 'disconnected';

interface UseEngineSocketOptions {
  url?: string;
  reconnectDelayMs?: number;
}

interface EngineSocketState {
  status: ConnectionStatus;
  nodes: NodeState[];
  events: WebEvent[];
  eventsPerSecond: number;
  paused: boolean;
  setPaused: (p: boolean) => void;
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

  const wsRef = useRef<WebSocket | null>(null);
  const pausedRef = useRef(paused);
  const recentTimestamps = useRef<number[]>([]);
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Keep pausedRef in sync without recreating the socket effect.
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
        recentTimestamps.current.push(Date.now());
        setEvents(prev => [msg as WebEvent, ...prev].slice(0, MAX_EVENTS));
      } catch {
        // Malformed message — ignore.
      }
    };

    ws.onclose = () => {
      setStatus('reconnecting');
      reconnectTimer.current = setTimeout(connect, reconnectDelay);
    };

    ws.onerror = () => {
      ws.close();
    };
  }, [url, reconnectDelay]);

  useEffect(() => {
    connect();
    return () => {
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current);
      wsRef.current?.close();
      setStatus('disconnected');
    };
  }, [connect]);

  return { status, nodes, events, eventsPerSecond, paused, setPaused };
}

function buildWsUrl(): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${window.location.host}/ws`;
}
