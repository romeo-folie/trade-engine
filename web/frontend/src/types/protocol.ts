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
  kind: 'source' | 'processing' | 'output' | 'analyst' | 'synthesis' | 'execution' | 'arbiter' | 'ticker' | 'arbiter_final' | 'enricher';
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
export const PARTICLE_COLORS: Record<string, string> = {
  // Market data & Indicators
  DataFetched:          '#0EA5E9', // sky blue
  IndicatorsComputed:   '#06B6D4', // cyan
  // News
  NewsEnriched:         '#F59E0B', // amber
  NewsIngested:         '#F59E0B', // amber
  // LLM / Analysis
  AnalysisCompleted:    '#8B5CF6', // violet
  AnalysisStarted:      '#6366F1', // indigo
  AnalystVotesPublished:'#A855F7', // light purple
  ArbiterInvoked:       '#EC4899', // pink — resolution
  // Signal flow
  SignalGenerated:      '#6366F1', // indigo
  RiskCheckCompleted:   '#3B82F6', // blue
  SignalQueued:       '#F59E0B', // amber
  // Execution
  TradeExecuted:        '#39FF14', // acid green
  TradeSimulated:       '#22C55E', // emerald
  TradeClosed:          '#EF4444', // red
  PositionUpdated:      '#3B82F6', // blue
  PortfolioSnapshot:    '#14B8A6', // teal
  // Blocked / expired
  SignalBlocked:        '#6B7280', // grey
  SignalExpired:        '#6B7280', // grey
  // Errors
  ErrorOccurred:        '#EF4444', // red
  // System
  TickStarted:          '#4B5563', // dim grey — cycle boundary, not a data event
};

export function particleColor(eventType: string): string {
  return PARTICLE_COLORS[eventType] ?? '#39FF14';
}

// ── Event type → human label ─────────────────────────────────────────────────

export const EVENT_LABELS: Record<string, string> = {
  DataFetched:        'Market Data',
  IndicatorsComputed: 'Indicators',
  NewsEnriched:       'News',
  AnalysisCompleted:    'Analysis',
  AnalysisStarted:      'Dispatch',
  AnalystVotesPublished:'Votes',
  ArbiterInvoked:       'Resolver',
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
  TickStarted:        'New Tick',
};
