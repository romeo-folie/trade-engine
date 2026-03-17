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

// Particle colours sourced from design tokens (tokens.css / tokens.ts).
// Update here when the token palette changes to keep particles in sync.
import { TOKENS } from '../styles/tokens';

export const PARTICLE_COLORS: Record<string, string> = {
  // Market data
  DataFetched:          TOKENS.blue,
  IndicatorsComputed:   TOKENS.blue,
  // News
  NewsIngested:         TOKENS.amber,
  // LLM / analysis
  AnalysisCompleted:    TOKENS.violet,
  // Signal flow
  SignalGenerated:      TOKENS.violet,
  RiskCheckCompleted:   TOKENS.violet,
  SignalQueued:         TOKENS.amber,
  // Execution
  TradeExecuted:        TOKENS.emerald,
  TradeSimulated:       TOKENS.emerald,
  TradeClosed:          TOKENS.red,
  PositionUpdated:      TOKENS.emerald,
  PortfolioSnapshot:    TOKENS.emerald,
  // Blocked / expired
  SignalBlocked:        TOKENS.textDim,
  SignalExpired:        TOKENS.textDim,
  // Errors
  ErrorOccurred:        TOKENS.red,
};

export function particleColor(eventType: string): string {
  return PARTICLE_COLORS[eventType] ?? TOKENS.blue; // fallback: blue for unknown event types
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
