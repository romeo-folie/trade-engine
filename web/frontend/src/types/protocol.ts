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
  kind: 'source' | 'processing' | 'output' | 'analyst' | 'synthesis' | 'execution' | 'arbiter';
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

// Particle colours sourced from design tokens (tokens.css / tokens.ts).
// Update here when the token palette changes to keep particles in sync.
// Particle colours sourced from design tokens (tokens.css / tokens.ts).
// Update here when the token palette changes to keep particles in sync.

export const PARTICLE_COLORS: Record<string, string> = {
  // Market data
  DataFetched:          '#22C55E', // emerald
  IndicatorsComputed:   '#39FF14', // acidGreen
  // News
  NewsIngested:         '#F59E0B', // amber
  // LLM / analysis
  AnalysisCompleted:    '#8B5CF6', // violet
  // Signal flow
  SignalGenerated:      '#8B5CF6', // violet
  RiskCheckCompleted:   '#39FF14', // acidGreen
  SignalQueued:         '#F59E0B', // amber
  // Execution
  TradeExecuted:        '#22C55E', // emerald
  TradeSimulated:       '#22C55E', // emerald
  TradeClosed:          '#EF4444', // red
  PositionUpdated:      '#22C55E', // emerald
  PortfolioSnapshot:    '#22C55E', // emerald
  // Blocked / expired
  SignalBlocked:        '#6B7280', // textDim
  SignalExpired:        '#6B7280', // textDim
  // Errors
  ErrorOccurred:        '#EF4444', // red
};

export function particleColor(eventType: string): string {
  return PARTICLE_COLORS[eventType] ?? '#39FF14';
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
