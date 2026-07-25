/**
 * HubGraph — hub-and-spoke pipeline visualisation.
 *
 * ENGINE sits at centre. Nodes radiate outward by kind:
 *   output (closest) → processing (mid) → source (outermost)
 *
 * Animated particles travel along edges powered by framer-motion.
 * Clicking a node opens the DrillDownPanel.
 */
import { useCallback, useMemo, useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { LayoutGrid, Lock, LockOpen, Crosshair } from 'lucide-react';
import {
  ReactFlow,
  PanOnScrollMode,
  Background,
  MiniMap,
  useNodesState,
  useEdgesState,
  type Node,
  type Edge,
  type NodeMouseHandler,
  BackgroundVariant,
} from '@xyflow/react';
import '@xyflow/react/dist/style.css';
import { useAnimationFrame } from 'framer-motion';
import type { NodeState } from '../../types/protocol';
import type { NodeMetrics } from '../../hooks/useEngineSocket';
import { useEngineSocketContext } from '../../contexts/EngineSocketContext';
import { useWatchlist } from '../../hooks/useWatchlist';
import { useModelEnsemble } from '../../hooks/useModelEnsemble';
import { useSystemStatus } from '../../hooks/useSystemStatus';
import PipelineNodeCard, { type PipelineNodeData } from './PipelineNodeCard';
import { particleColor } from '../../types/protocol';
import { colors } from '../../styles/tokens';
import styles from './HubGraph.module.css';
import OrganicEdge from './OrganicEdge';

// ── Constants ────────────────────────────────────────────────────────────────

const NODE_TYPES = { pipelineNode: PipelineNodeCard };
const EDGE_TYPES = { organic: OrganicEdge };

// Column X-offsets — 400px gap (NODE_W=280 + 120 spacer)
const COL = {
  source: 60,
  enricher: 460,
  indicators: 860,
  analysts: 1360,     // Extra spacer for analyst stack
  consensus: 1860,
  arbiter: 2360,       // Center point between consensus and risk
  risk: 2860,
  execution: 3360,
  portfolio: 3800,
};

const CY = 400;

// Node dimensions
const NODE_W = 280;
const NODE_H = 140;

const EDGE_ACTIVE = colors.acidGreen;

// ── Helpers ──────────────────────────────────────────────────────────────────

function getColumnX(nodeId: string, kind: string): number {
  // Priority 1: Specific node IDs for key pipeline stages
  if (nodeId === 'consensus') return COL.consensus;
  if (nodeId === 'arbiter_node') return COL.arbiter;
  if (nodeId === 'risk_manager') return COL.risk;
  if (nodeId === 'indicator_engine') return COL.indicators;
  if (nodeId === 'portfolio') return COL.portfolio;
  if (nodeId === 'alpaca') return COL.enricher; // Move Alpaca to process col to avoid crossing
  if (nodeId === 'news_api') return COL.source;

  // Priority 2: General kinds
  switch (kind) {
    case 'source': return COL.source;
    case 'enricher': return COL.enricher;
    case 'analyst': return COL.analysts;
    case 'execution': return COL.execution;
    default: return COL.source;
  }
}

function modelAccentColor(provider: string): string {
  switch (provider.toLowerCase()) {
    case 'anthropic': return colors.analystTechnical;
    case 'openai': return colors.analystMomentum;
    case 'google': return colors.analystSentiment;
    default: return colors.analystMacro;
  }
}

// ── Types ────────────────────────────────────────────────────────────────────

interface HubGraphProps {
  nodes: NodeState[];
  nodeMetrics: Record<string, NodeMetrics>;
  recentEventSignatures: Record<string, { type: string; seqId: number }>;
  onNodeClick: (nodeId: string) => void;
}

interface Particle {
  id: string;
  fromX: number; fromY: number;
  toX: number; toY: number;
  color: string;
  progress: number;
  speed: number;
  bezierIntensity: number; // For non-linear paths
}

// ── CanvasIconButton ──────────────────────────────────────────────────────────

interface CanvasIconButtonProps {
  icon: ReactNode;
  onClick: () => void;
  title?: string;
  active?: boolean;
}

function CanvasIconButton({ icon, onClick, title, active }: CanvasIconButtonProps) {
  return (
    <button
      onClick={onClick}
      title={title}
      style={{
        width: 28,
        height: 28,
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        background: active ? 'rgba(57, 255, 20, 0.1)' : 'rgba(13, 13, 17, 0.85)',
        border: `1px solid ${active ? 'rgba(57, 255, 20, 0.4)' : 'rgba(26, 26, 34, 0.8)'}`,
        borderRadius: 4,
        color: active ? '#39FF14' : '#6B7280',
        cursor: 'pointer',
        backdropFilter: 'blur(4px)',
        transition: 'all 0.15s ease',
        padding: 0,
      }}
      onMouseEnter={e => {
        if (!active) (e.currentTarget as HTMLButtonElement).style.color = '#F0F0F0';
      }}
      onMouseLeave={e => {
        if (!active) (e.currentTarget as HTMLButtonElement).style.color = '#6B7280';
      }}
    >
      {icon}
    </button>
  );
}

// ── Component ────────────────────────────────────────────────────────────────

export default function HubGraph({ nodes, nodeMetrics, recentEventSignatures, onNodeClick }: HubGraphProps) {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const wrapperRef = useRef<HTMLDivElement>(null);
  const particlesRef = useRef<Particle[]>([]);
  const lastFrameRef = useRef<number>(0);
  const setViewportRef = useRef<((vp: { x: number; y: number; zoom: number }) => void) | null>(null);
  const fitViewRef = useRef<((opts?: { padding?: number; duration?: number }) => void) | null>(null);
  const [layoutLocked, setLayoutLocked] = useState(false);

  const watchlistTickers = useWatchlist();
  const modelEnsemble = useModelEnsemble();
  const systemStatus = useSystemStatus();
  const isOrbMode = systemStatus?.mode?.toLowerCase() === 'orb';
  const { analystConsensus, analystVotes, currentTicker, lastRiskChecks, lastTrade, portfolioState, selectedAnalysts, verdictHistory } = useEngineSocketContext();

  // ── Build React Flow nodes ──────────────────────────────────────────────

  const rfNodes = useMemo<Node<PipelineNodeData>[]>(() => {
    // Config-seeded analyst nodes: prefer "analyst-*" prefixed nodes (config-seeded).
    // Fall back to "llm_analysis" only when no analyst-* nodes exist (legacy single-model config).
    const prefixedAnalysts = nodes.filter(n => n.id.startsWith('analyst-'));
    const snapshotAnalystNodes = prefixedAnalysts.length > 0
      ? prefixedAnalysts
      : nodes.filter(n => n.id === 'llm_analysis');

    // Build base analyst list from snapshot nodes (config-seeded, always complete).
    // modelEnsemble is only used to ENRICH snapshot nodes with performance data (accuracy,
    // totalSignals) — it never replaces the list, because /api/node/models filters to models
    // with ≥5 historical trades and would drop analysts that haven't fired enough yet.
    const baseAnalysts: NodeState[] = snapshotAnalystNodes.length > 0
      ? snapshotAnalystNodes.map(n => ({
        id: n.id,
        label: n.label,
        kind: 'analyst' as const,
        event_count: 0,
        last_event_at: undefined,
        meta: { provider: (n.meta?.provider as string) ?? '' },
      }))
      : [{ id: 'llm_analysis', label: 'LLM Analysis', kind: 'analyst' as const, event_count: 0, last_event_at: undefined }];

    // Overlay performance data from modelEnsemble where available (matched by model label).
    const analystNodes: NodeState[] = baseAnalysts.map(base => {
      const perf = modelEnsemble.find(m => m.modelName === base.label);
      if (!perf) return base;
      return {
        ...base,
        event_count: perf.totalSignals,
        meta: { ...(base.meta ?? {}), provider: perf.provider, accuracy: perf.accuracy },
      };
    });

    // Pipeline nodes with real backend IDs mapped to visual stages.
    const PIPELINE_NODES: NodeState[] = [
      // Sources (col 0)
      { id: 'alpaca', label: 'Alpaca Feed', kind: 'source', event_count: 0, last_event_at: undefined },
      { id: 'news_api', label: nodes.find(n => n.id === 'news_api')?.label ?? 'News Feed', kind: 'source', event_count: 0, last_event_at: undefined },
      // Enricher (col 0.5)
      { id: 'news_enricher', label: 'News Enricher', kind: 'enricher', event_count: 0, last_event_at: undefined },
      // Arbiter (col 1)
      { id: 'indicator_engine', label: 'Indicator Engine', kind: 'arbiter', event_count: 0, last_event_at: undefined },
      // Analyst (col 2) — dynamic
      ...analystNodes,
      // Consensus (col 3)
      { id: 'consensus', label: 'Consensus', kind: 'synthesis', event_count: 0, last_event_at: undefined },
      // Arbiter Final (col 4) — handles dissent
      {
        id: 'arbiter_node',
        label: nodes.find(n => n.id === 'arbiter_node')?.label ?? 'Ensemble Arbiter',
        kind: 'arbiter_final',
        event_count: 0,
        last_event_at: undefined
      },
      // Risk Manager (col 5)
      { id: 'risk_manager', label: 'Risk Manager', kind: 'synthesis', event_count: 0, last_event_at: undefined },
      // Execution (col 6)
      { id: 'executor', label: 'Executor', kind: 'execution', event_count: 0, last_event_at: undefined },
      { id: 'portfolio', label: 'Portfolio', kind: 'execution', event_count: 0, last_event_at: undefined },
    ];

    // Merge live metrics and metadata from backend into pipeline nodes.
    const mergedPipeline = PIPELINE_NODES.map(pn => {
      const live = nodes.find(n => n.id === pn.id);
      if (!live) return pn;
      return {
        ...pn,
        label: live.label || pn.label,
        event_count: live.event_count,
        last_event_at: live.last_event_at,
        meta: { ...(pn.meta ?? {}), ...(live.meta ?? {}) }
      };
    });

    const allNodes = [...mergedPipeline];

    // Stack nodes vertically within each kind group
    const kindGroups: Record<string, typeof allNodes> = {};
    for (const n of allNodes) {
      if (!kindGroups[n.kind]) kindGroups[n.kind] = [];
      kindGroups[n.kind].push(n);
    }

    // Calculate global allRiskPassed state
    const allRiskPassed = lastRiskChecks.length === 0 || lastRiskChecks.every(c => c.passed);

    return allNodes.map((n) => {
      // Grouping logic for vertical stacking. 
      // Group Alpaca with Enricher for vertical stacking to clear the path.
      const groupKey = (n.id === 'consensus' || n.id === 'risk_manager' || n.id === 'executor' || n.id === 'portfolio') 
        ? n.id 
        : (n.id === 'alpaca' || n.kind === 'enricher') ? 'entry_processing' : n.kind;
      const group = allNodes.filter(an => {
        const ak = (an.id === 'consensus' || an.id === 'risk_manager' || an.id === 'executor' || an.id === 'portfolio') 
          ? an.id 
          : (an.id === 'alpaca' || an.kind === 'enricher') ? 'entry_processing' : an.kind;
        return ak === groupKey;
      });

      // Layout coordinates (center-to-center symmetry)
      const groupGap = (n.kind === 'analyst' || n.kind === 'enricher') ? 200 : 260;
      const groupIdx = group.findIndex(gn => gn.id === n.id);

      const x = getColumnX(n.id, n.kind);

      // Variable height estimation for centering (Data-backed from CSS + Content)
      let estimatedH = NODE_H;
      if (n.id === 'consensus') estimatedH = 180;
      if (n.id === 'risk_manager') estimatedH = 180; // Refined height estimate
      if (n.id === 'portfolio') estimatedH = 240;
      if (n.id === 'executor') estimatedH = 160;
      if (n.kind === 'execution' || n.kind === 'enricher' || n.kind === 'analyst') estimatedH = 180;

      // Center the whole group vertically around CY
      let y = CY - ((group.length - 1) * groupGap) / 2 + (groupIdx * groupGap) - (estimatedH / 2);

      // Arbiter offset to clarify bypass path
      if (n.id === 'arbiter_node') {
        y -= 245;
      }

      // Pin risk_manager Y to match consensus's midpoint so the connecting edge
      // stays horizontally level regardless of risk_manager card height.
      // consensus estimatedH = 180, so its midpoint = y_consensus + 90.
      // We want risk_manager's midpoint to equal consensus's midpoint:
      //   y_risk + estimatedH_risk/2 = y_consensus + 90
      // Since risk_manager is a single-node group, y_consensus === CY - (180/2) = CY - 90.
      // Therefore y_risk = CY - 90 - estimatedH_risk/2 ... but estimatedH varies.
      // Simplest approach: pin risk_manager top to CY - 90 so its top aligns with
      // consensus's top, and the edge mid connects at the same horizontal band.
      if (n.id === 'risk_manager') {
        const consensusEstH = 180;
        y = CY - consensusEstH / 2 - 20;
      }

      // Execution phase stagger as per desired layout: 
      // Risk (Center) -> Executor (Down) -> Portfolio (Up)
      if (n.id === 'executor') {
        y += 100;
      }
      if (n.id === 'portfolio') {
        y -= 150;
      }

      const metrics = nodeMetrics[n.id] ?? null;
      const liveLastEventAt = metrics?.lastEventAt
        ? new Date(metrics.lastEventAt).toISOString()
        : (n.last_event_at ?? null);

      // Pass extra model data through for analyst cards
      const provider = n.meta?.provider as string | undefined;
      const accuracy = n.meta?.accuracy as number | undefined;

      // Match analyst vote by node label (analyst name) only — provider is not
      // a stable key and causes mis-assignment when multiple analysts share a provider.
      const vote = analystVotes.find(v => v.model === n.label);

      return {
        id: n.id,
        type: 'pipelineNode',
        position: { x, y },
        data: {
          nodeId: n.id,
          label: n.label,
          kind: n.kind,
          eventCount: (metrics?.eventCount ?? 0) + (n.event_count ?? 0),
          lastEventAt: liveLastEventAt,
          metrics,
          isHub: false,
          lastEventType: recentEventSignatures[n.id]?.type ?? null,
          onNodeClick,
          provider,
          accuracy,
          accentColor: provider ? modelAccentColor(provider) : undefined,
          // Per-analyst live vote
          ...(vote ? { verdict: vote.action, voteConfidence: vote.confidence, skipped: vote.skipped } : {}),
          isChosen: selectedAnalysts.includes(n.id),
          verdictEntries: n.label ? (verdictHistory[n.label] ?? []) : [],
          // Alpaca Feed: pass watchlist for marquee display
          ...(n.id === 'alpaca' ? { watchlistTickers, isOrbMode } : {}),
          // Live pipeline data
          ...(n.id === 'consensus' ? { analystConsensus, analystVotes, currentTicker } : {}),
          ...(n.id === 'risk_manager' ? { riskChecks: lastRiskChecks } : {}),
          ...(n.id === 'executor' ? { lastTrade, allRiskPassed } : {}),
          ...(n.id === 'portfolio' ? { portfolioState, allRiskPassed } : {}),
        },
        draggable: true,
        selectable: true,
      };
    });
  }, [nodes, nodeMetrics, recentEventSignatures, onNodeClick, watchlistTickers, modelEnsemble, isOrbMode, analystConsensus, analystVotes, currentTicker, lastRiskChecks, lastTrade, portfolioState, selectedAnalysts, verdictHistory]);

  // ── Build React Flow edges ──────────────────────────────────────────────

  const rfEdges = useMemo<Edge[]>(() => {
    const isNodeRecentlyActive = (nodeId: string): boolean => {
      const lastEventAt = nodeMetrics[nodeId]?.lastEventAt;
      if (!lastEventAt) return false;
      // Execution cards (executor, portfolio): treat as active if data is present.
      // Trades/positions are infrequent — a recency gate would show IDLE immediately
      // after the 5s window even though the last trade data is still displayed.
      const node = rfNodes.find(n => n.id === nodeId);
      if (node?.data.kind === 'execution') {
        const allPassed = node.data.allRiskPassed !== false;
        if (node.id === 'executor') return !!(node.data.lastTrade) && allPassed;
        if (node.id === 'portfolio') return !!(node.data.portfolioState) && allPassed;
      }
      return Date.now() - lastEventAt < 10_000;
    };

    const edges: Edge[] = [];
    const sourceIds = rfNodes.filter(n => n.data.kind === 'source').map(n => n.id);
    const arbiterId = rfNodes.find(n => n.data.kind === 'arbiter')?.id;
    const analystIds = rfNodes.filter(n => n.data.kind === 'analyst').map(n => n.id);
    const arbiterFinalId = rfNodes.find(n => n.data.kind === 'arbiter_final')?.id;

    const makeEdge = (src: string, tgt: string, opts?: { forceInactive?: boolean; activeOverride?: boolean }) => {
      const active = opts?.activeOverride !== undefined
        ? opts.activeOverride
        : !opts?.forceInactive && isNodeRecentlyActive(src);
      return {
        id: `e-${src}-${tgt}`,
        source: src,
        target: tgt,
        style: active
          ? {
            stroke: EDGE_ACTIVE,
            strokeWidth: 1,
            strokeDasharray: '5 5',
            animation: 'edge-dash 0.5s linear infinite',
            opacity: 1,
          }
          : {
            stroke: colors.textFaint,
            strokeWidth: 1,
            strokeDasharray: '',
            animation: '',
            opacity: 0.55,
          },
        data: {
          active,
        },
        type: 'organic',
      };
    };

    if (arbiterId) {
      sourceIds.forEach(sid => {
        // News source goes to enricher, others to indicator_engine
        if (sid === 'news_api') {
          edges.push(makeEdge(sid, 'news_enricher'));
          edges.push(makeEdge('news_enricher', arbiterId));
        } else {
          edges.push(makeEdge(sid, arbiterId));
        }
      });

      analystIds.forEach(aid => {
        // Selective pulsing based on selectedAnalysts set (from AnalysisStarted)
        // Edge lights up only while the analyst is actively selected for this dispatch.
        // selectedAnalysts is cleared on TickStarted so the edge goes dark at cycle reset.
        // wasInvoked (from analystVotes) was removed: it persisted the entire tick duration,
        // keeping edges lit long after analysis completed.
        const isSelected = selectedAnalysts.includes(aid) && isNodeRecentlyActive(aid);
        edges.push(makeEdge(arbiterId, aid, { activeOverride: isSelected }));
      });
    }

    // All analysts → consensus
    analystIds.forEach(aid => edges.push(makeEdge(aid, 'consensus')));

    // The two post-consensus paths are mutually exclusive:
    //   Fast path:    consensus → risk_manager        (when arbiter was NOT invoked)
    //   Arbiter path: consensus → arbiter_node → risk_manager (when ArbiterInvoked fired)
    //
    // arbiter_node nodeMetrics are only ever updated by ArbiterInvoked events
    // (protocol.go maps ArbiterInvoked → source=consensus, target=arbiter_node).
    // So isNodeRecentlyActive('arbiter_node') is a reliable "was arbiter used?" flag
    // with no new state required.
    const arbiterActive = arbiterFinalId ? isNodeRecentlyActive(arbiterFinalId) : false;
    edges.push(makeEdge('consensus', 'risk_manager', {
      activeOverride: isNodeRecentlyActive('consensus') && !arbiterActive,
    }));
    if (arbiterFinalId) {
      edges.push(makeEdge('consensus', arbiterFinalId, { activeOverride: arbiterActive }));
      edges.push(makeEdge(arbiterFinalId, 'risk_manager'));
    }

    // Causal flow: Risk → Executor → Portfolio
    const allRiskPassed = lastRiskChecks.length === 0 || lastRiskChecks.every(c => c.passed);
    const riskActive = isNodeRecentlyActive('risk_manager');

    // Risk → Executor (Lights only on PASS)
    edges.push(makeEdge('risk_manager', 'executor', {
      activeOverride: riskActive && allRiskPassed
    }));

    // Executor → Portfolio (Pulse on trade activity)
    edges.push(makeEdge('executor', 'portfolio'));

    return edges;
  }, [rfNodes, nodeMetrics, lastRiskChecks, selectedAnalysts]);

  const [rfN, setRfNodes, onNodesChange] = useNodesState(rfNodes);
  const [rfE, setRfEdges, onEdgesChange] = useEdgesState(rfEdges);

  // Reconcile nodes and edges before paint. Live engine events update card data
  // many times per second; replacing nodes after paint discarded React Flow's
  // measured dimensions/handle internals and briefly left edges without valid
  // endpoints. Preserve the complete existing node shell while overlaying fresh
  // application data, and batch both topology updates in one layout effect.
  useLayoutEffect(() => {
    setRfNodes((nds) => {
      const currentMap = new Map(nds.map(n => [n.id, n]));

      return rfNodes.map(n => {
        const existing = currentMap.get(n.id);
        if (!existing) {
          return { ...n, draggable: !layoutLocked };
        }
        return {
          ...existing,
          ...n,
          position: existing.position,
          dragging: existing.dragging,
          selected: existing.selected,
          draggable: !layoutLocked,
        };
      });
    });
    setRfEdges((edges) => {
      const currentMap = new Map(edges.map(edge => [edge.id, edge]));
      return rfEdges.map(edge => {
        const existing = currentMap.get(edge.id);
        return existing ? { ...existing, ...edge, selected: existing.selected } : edge;
      });
    });
  }, [rfNodes, rfEdges, layoutLocked, setRfNodes, setRfEdges]);

  const handleNodeClick: NodeMouseHandler = useCallback((_evt, node) => {
    onNodeClick(node.id);
  }, [onNodeClick]);

  const viewportRef = useRef({ x: 0, y: 0, zoom: 1 });
  const handleViewportChange = useCallback((vp: { x: number; y: number; zoom: number }) => {
    viewportRef.current = vp;
  }, []);

  // ── Particle Spawning ──────────────────────────────────────────────────
  // Map nodes from the actual React Flow state (rfN) so that particles
  // originate from current/dragged positions rather than calculated defaults.
  const rfNodeMap = useMemo(() => {
    const m = new Map<string, typeof rfN[number]>();
    for (const n of rfN) m.set(n.id, n);
    return m;
  }, [rfN]);

  const prevSignatures = useRef<Record<string, { type: string; seqId: number }>>({});
  useEffect(() => {
    for (const [nodeId, sig] of Object.entries(recentEventSignatures)) {
      const prev = prevSignatures.current[nodeId];
      if (prev && prev.type === sig.type && prev.seqId === sig.seqId) continue;
      prevSignatures.current[nodeId] = sig;

      // Find the edge(s) this node is a source of.
      // For the consensus node, only spawn particles on the path actually taken:
      //   AnalysisCompleted → fast path (consensus → risk_manager), suppress arbiter edge
      //   ArbiterInvoked    → arbiter path (consensus → arbiter_node), suppress direct edge
      const outgoingEdges = rfE.filter(e => {
        if (e.source !== 'consensus') return e.source === nodeId;
        if (sig.type === 'ArbiterInvoked') return e.target === 'arbiter_node';
        return e.target === 'risk_manager';
      });
      outgoingEdges.forEach(edge => {
        const src = rfNodeMap.get(edge.source);
        const tgt = rfNodeMap.get(edge.target);
        if (!src || !tgt) return;

        const getEstimatedH = (id: string, kind: string) => {
          if (id === 'consensus') return 180;
          if (id === 'risk_manager') return 180;
          if (id === 'portfolio') return 240;
          if (id === 'executor') return 160;
          if (kind === 'execution' || kind === 'enricher' || kind === 'analyst') return 180;
          return NODE_H;
        };

        const srcH = getEstimatedH(src.id, src.data.kind);
        const tgtH = getEstimatedH(tgt.id, tgt.data.kind);

        // Recalculate positions with the same offsets used in the node mapping
        let fromY = src.position.y + srcH / 2;
        if (src.id === 'executor') {
          fromY += 100;
        }
        if (src.id === 'portfolio') {
          fromY -= 150;
        }

        let toY = tgt.position.y + tgtH / 2;
        if (tgt.id === 'executor') {
          toY += 100;
        }
        if (tgt.id === 'portfolio') {
          toY -= 150;
        }

        particlesRef.current.push({
          id: `${nodeId}-${edge.target}-${Date.now()}-${Math.random()}`,
          fromX: src.position.x + NODE_W,
          fromY,
          toX: tgt.position.x,
          toY,
          color: particleColor(sig.type),
          progress: 0,
          speed: 0.0008 + Math.random() * 0.0004,
          bezierIntensity: 0.8, // Match OrganicEdge
        });
      });
    }
  }, [recentEventSignatures, rfNodeMap, rfE]);

  // Canvas loop... (same as before but using rfN for dynamic node positions)
  useAnimationFrame((time) => {
    const canvas = canvasRef.current;
    if (!canvas) return;

    const ctx = canvas.getContext('2d');
    if (!ctx) return;

    const dt = lastFrameRef.current ? time - lastFrameRef.current : 16;
    lastFrameRef.current = time;

    const dpr = window.devicePixelRatio || 1;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, canvas.width / dpr, canvas.height / dpr);

    const vp = viewportRef.current;

    particlesRef.current = particlesRef.current.filter(p => {
      p.progress = Math.min(1, p.progress + p.speed * dt);

      const t = p.progress;
      const xStart = p.fromX;
      const yStart = p.fromY;
      const xEnd = p.toX;
      const yEnd = p.toY;

      // Standard cubic bezier for horizontal flow (control points moved out by intensity)
      const cp1X = xStart + (xEnd - xStart) * p.bezierIntensity;
      const cp1Y = yStart;
      const cp2X = xEnd - (xEnd - xStart) * p.bezierIntensity;
      const cp2Y = yEnd;

      // Calculate path (Cubic Bezier)
      const xRaw = Math.pow(1 - t, 3) * xStart + 3 * Math.pow(1 - t, 2) * t * cp1X + 3 * (1 - t) * Math.pow(t, 2) * cp2X + Math.pow(t, 3) * xEnd;
      const yRaw = Math.pow(1 - t, 3) * yStart + 3 * Math.pow(1 - t, 2) * t * cp1Y + 3 * (1 - t) * Math.pow(t, 2) * cp2Y + Math.pow(t, 3) * yEnd;

      const x = xRaw * vp.zoom + vp.x;
      const y = yRaw * vp.zoom + vp.y;

      const radius = 6 * vp.zoom;
      ctx.beginPath();
      const grad = ctx.createRadialGradient(x, y, 0, x, y, radius);
      grad.addColorStop(0, p.color + 'ff');
      grad.addColorStop(1, p.color + '00');
      ctx.fillStyle = grad;
      ctx.arc(x, y, radius, 0, Math.PI * 2);
      ctx.fill();

      ctx.beginPath();
      ctx.fillStyle = p.color;
      ctx.arc(x, y, 2.5 * vp.zoom, 0, Math.PI * 2);
      ctx.fill();

      return p.progress < 1;
    });
  });

  const resetLayout = useCallback(() => {
    setRfNodes((currentNodes) => {
      const groups: Record<string, typeof currentNodes> = {};
      for (const n of currentNodes) {
        if (!groups[n.data.kind]) groups[n.data.kind] = [];
        groups[n.data.kind].push(n);
      }
      return currentNodes.map(n => {
        const groupKey = (n.id === 'consensus' || n.id === 'risk_manager' || n.id === 'executor' || n.id === 'portfolio') 
          ? n.id 
          : (n.id === 'alpaca' || n.data.kind === 'enricher') ? 'entry_processing' : n.data.kind;
        const group = currentNodes.filter(an => {
          const ak = (an.id === 'consensus' || an.id === 'risk_manager' || an.id === 'executor' || an.id === 'portfolio') 
            ? an.id 
            : (an.id === 'alpaca' || an.data.kind === 'enricher') ? 'entry_processing' : an.data.kind;
          return ak === groupKey;
        });

        // Variable height estimation for centering (Data-backed from CSS + Content)
        let estimatedH = NODE_H;
        if (n.id === 'consensus') estimatedH = 180;
        if (n.id === 'risk_manager') estimatedH = 180; // Refined height estimate
        if (n.id === 'portfolio') estimatedH = 240;
        if (n.id === 'executor') estimatedH = 160;
        if (n.data.kind === 'execution' || n.data.kind === 'enricher' || n.data.kind === 'analyst') estimatedH = 180;

        const groupGap = (n.data.kind === 'analyst' || n.data.kind === 'enricher') ? 200 : 260;
        const groupIdx = group.findIndex(gn => gn.id === n.id);
        const x = getColumnX(n.id, n.data.kind);

        // Center vertically around CY
        let y = CY - ((group.length - 1) * groupGap) / 2 + (groupIdx * groupGap) - (estimatedH / 2);

        // Arbiter offset to clarify bypass path
        if (n.id === 'arbiter_node') {
          y -= 245;
        }

        // Minor vertical correction for Risk Manager to align with Consensus spline
        if (n.id === 'risk_manager') {
          y -= 20;
        }

        // Execution phase stagger: Risk (Center) -> Executor (Down) -> Portfolio (Up)
        if (n.id === 'executor') {
          y += 100;
        }
        if (n.id === 'portfolio') {
          y -= 150;
        }

        return { ...n, position: { x, y } };
      });
    });
    // Give React a tick to apply new positions before fitting
    setTimeout(() => {
      fitViewRef.current?.({ padding: 0.15, duration: 400 });
    }, 50);
  }, [setRfNodes]);

  return (
    <div ref={wrapperRef} className={styles.wrapper}>
      <ReactFlow
        nodes={rfN}
        edges={rfE}
        onNodesChange={onNodesChange}
        onEdgesChange={onEdgesChange}
        onNodeClick={handleNodeClick}
        onViewportChange={handleViewportChange}
        onInit={(instance) => {
          setViewportRef.current = instance.setViewport;
          fitViewRef.current = instance.fitView;
        }}
        nodeTypes={NODE_TYPES}
        edgeTypes={EDGE_TYPES}
        minZoom={0.2}
        maxZoom={1.5}
        fitView
        fitViewOptions={{ padding: 0.2 }}
        panOnScroll
        panOnScrollMode={PanOnScrollMode.Free}
        zoomOnScroll={false}
        proOptions={{ hideAttribution: true }}
      >
        <Background variant={BackgroundVariant.Dots} gap={20} size={1} color="#1A1A22" style={{ opacity: 0.2 }} />
        <MiniMap
          nodeColor={() => colors.border}
          maskColor="rgba(5, 5, 7, 0.85)"
          style={{ background: colors.cardBg, border: `1px solid ${colors.border}` }}
          pannable
          zoomable
        />
      </ReactFlow>
      <canvas ref={canvasRef} className={styles.particleCanvas} style={{ pointerEvents: 'none' }} />

      {/* Floating Controls — vertical icon stack, bottom left */}
      <div style={{
        position: 'absolute',
        bottom: 20,
        left: 20,
        zIndex: 10,
        display: 'flex',
        flexDirection: 'column',
        gap: 8,
      }}>
        <CanvasIconButton
          icon={<Crosshair size={14} />}
          onClick={() => fitViewRef.current?.({ padding: 0.15, duration: 400 })}
          title="Fit view"
          active={false}
        />
        <CanvasIconButton
          icon={<LayoutGrid size={14} />}
          onClick={resetLayout}
          title="Reset layout"
          active={false}
        />
        <CanvasIconButton
          icon={layoutLocked ? <Lock size={14} /> : <LockOpen size={14} />}
          onClick={() => setLayoutLocked(l => !l)}
          title={layoutLocked ? 'Unlock layout' : 'Lock layout'}
          active={layoutLocked}
        />
      </div>
    </div>
  );
}
