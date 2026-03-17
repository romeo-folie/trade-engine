/**
 * EngineSocketContext — provides single shared WebSocket state to all consumers.
 *
 * Shell calls useEngineSocket() once and publishes state here.
 * Screens consume via useEngineSocketContext() — no duplicate connections.
 */
import { createContext, useContext } from 'react';
import type { ConnectionStatus, NodeMetrics } from '../hooks/useEngineSocket';
import type { NodeState, WebEvent } from '../types/protocol';

export interface EngineSocketContextValue {
  status: ConnectionStatus;
  nodes: NodeState[];
  events: WebEvent[];
  eventsPerSecond: number;
  paused: boolean;
  setPaused: (p: boolean) => void;
  nodeMetrics: Record<string, NodeMetrics>;
}

export const EngineSocketContext = createContext<EngineSocketContextValue | null>(null);

export function useEngineSocketContext(): EngineSocketContextValue {
  const ctx = useContext(EngineSocketContext);
  if (!ctx) throw new Error('useEngineSocketContext must be used within EngineSocketContext.Provider');
  return ctx;
}
