/**
 * EngineSocketContext — provides single shared WebSocket state to all consumers.
 *
 * AppRoot calls useEngineSocket() once and publishes state here.
 * Screens consume via useEngineSocketContext() — no duplicate connections.
 */
import { createContext, useContext } from 'react';
import type { ConnectionStatus, NodeMetrics, IndexedWebEvent, AnalystConsensusEntry, AnalystVote, RiskCheckResult, LastTrade, PortfolioState } from '../hooks/useEngineSocket';
import type { NodeState } from '../types/protocol';

export interface EngineSocketContextValue {
  status: ConnectionStatus;
  nodes: NodeState[];
  events: IndexedWebEvent[];
  eventsPerSecond: number;
  paused: boolean;
  setPaused: (p: boolean) => void;
  nodeMetrics: Record<string, NodeMetrics>;
  lastSignalAt: number | null;
  avgLatencyMs: number | null;
  analystConsensus: Record<string, AnalystConsensusEntry>;
  lastRiskChecks: RiskCheckResult[];
  lastTrade: LastTrade | null;
  portfolioState: PortfolioState | null;
  analystVotes: AnalystVote[];
  selectedAnalysts: string[];
}

export const EngineSocketContext = createContext<EngineSocketContextValue | null>(null);

export function useEngineSocketContext(): EngineSocketContextValue {
  const ctx = useContext(EngineSocketContext);
  if (!ctx) throw new Error('useEngineSocketContext must be used within EngineSocketContext.Provider');
  return ctx;
}
