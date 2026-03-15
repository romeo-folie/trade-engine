// Wire types matching internal/web/protocol.go

export interface WebEvent {
  type: string;
  timestamp: string;
  source: string;
  target: string;
  payload?: unknown;
}

export interface NodeState {
  id: string;
  label: string;
  kind: 'source' | 'processing' | 'output';
  event_count: number;
  last_event_at?: string;
  avg_latency_ms?: number;
  meta?: Record<string, unknown>;
}

export interface SnapshotEvent {
  type: 'snapshot';
  timestamp: string;
  nodes: NodeState[];
}

// Particle color by event type (matches PRD §14.6)
export const PARTICLE_COLORS: Record<string, string> = {
  DataFetched: '#00b4ff',       // blue — market data
  NewsIngested: '#f59e0b',      // amber — news
  SignalGenerated: '#a855f7',   // purple — LLM / signal
  TradeExecuted: '#22c55e',     // green — buy
  TradeClosed: '#ef4444',       // red — sell/close
  SignalBlocked: '#6b7280',     // gray — blocked
  default: '#00f5ff',           // cyan — fallback
};

export function particleColor(eventType: string): string {
  return PARTICLE_COLORS[eventType] ?? PARTICLE_COLORS.default;
}
