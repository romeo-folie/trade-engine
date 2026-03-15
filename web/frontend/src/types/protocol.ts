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

// ── Particle colors by event type (PRD §14.6) ──────────────────────────────

export const PARTICLE_COLORS: Record<string, string> = {
  // Market data
  DataFetched:          '#00b4ff',
  IndicatorsComputed:   '#00b4ff',
  // News
  NewsIngested:         '#f59e0b',
  // LLM / analysis
  AnalysisCompleted:    '#a855f7',
  // Signal flow
  SignalGenerated:      '#a855f7',
  RiskCheckCompleted:   '#a855f7',
  SignalQueued:         '#f59e0b',
  // Execution
  TradeExecuted:        '#22c55e',
  TradeSimulated:       '#22c55e',
  TradeClosed:          '#ef4444',
  PositionUpdated:      '#22c55e',
  PortfolioSnapshot:    '#22c55e',
  // Blocked / expired
  SignalBlocked:        '#6b7280',
  SignalExpired:        '#6b7280',
  // Errors
  ErrorOccurred:        '#ef4444',
};

export function particleColor(eventType: string): string {
  return PARTICLE_COLORS[eventType] ?? '#00f5ff';
}

// ── Event type → human label ─────────────────────────────────────────────────

export const EVENT_LABELS: Record<string, string> = {
  DataFetched:        'Market Data',
  IndicatorsComputed: 'Indicators',
  AnalysisCompleted:  'Analysis',
  SignalGenerated:    'Signal',
  SignalBlocked:      'Blocked',
  SignalExpired:      'Expired',
  SignalQueued:       'Queued',
  RiskCheckCompleted: 'Risk OK',
  TradeExecuted:      'Trade',
  TradeSimulated:     'Simulated',
  TradeClosed:        'Closed',
  PositionUpdated:    'Position',
  PortfolioSnapshot:  'Portfolio',
  ErrorOccurred:      'Error',
};
