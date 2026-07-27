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
import type { CSSProperties, ReactNode } from 'react';
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
import type { NodeState, PipelineTopology } from '../../types/protocol';
import type { AnalystVote, NodeMetrics } from '../../hooks/useEngineSocket';
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

const CY = 400;
const STAGE_X = 460;
const STAGE_ORIGIN_X = 60;

// Node dimensions
const NODE_W = 280;
const NODE_H = 140;

const EDGE_ACTIVE = colors.acidGreen;

// ── Helpers ──────────────────────────────────────────────────────────────────

function getStageX(stage: number): number {
  return STAGE_ORIGIN_X + stage * STAGE_X;
}

/**
 * Approximate rendered card height, used to centre a node on the spine and to
 * anchor particle endpoints.
 *
 * These are calibrated against measured heights: a node whose estimate is too
 * small is placed too low, because placement subtracts half the estimate but
 * the browser lays out half the real height. risk_manager was the worst
 * offender at 180 against a measured 222 — enough to drop Risk Controls ~20px
 * below the queue and executor it is supposed to line up with.
 */
function estimatedNodeHeight(nodeId: string, kind: string): number {
  // Renders one row per configured risk check, so it is the tallest card here.
  if (nodeId === 'risk_manager') return 220;
  if (nodeId === 'consensus') return 180;
  if (nodeId === 'portfolio') return 205;
  if (nodeId === 'executor') return 150;
  if (kind === 'execution' || kind === 'enricher' || kind === 'analyst' || kind === 'risk_specialist') return 180;
  return NODE_H;
}

function modelAccentColor(provider: string): string {
  switch (provider.toLowerCase()) {
    case 'anthropic': return colors.analystTechnical;
    case 'openai': return colors.analystMomentum;
    case 'google': return colors.analystSentiment;
    default: return colors.analystMacro;
  }
}

interface StagedNode {
  id: string;
  stage: number;
  kind: string;
}

/**
 * Nodes lifted off the main spine by a fixed offset instead of being centred
 * within their stage.
 *
 * `blocked_signals` is a terminal branch, not a step in the flow. Centring it
 * alongside the pending queue pushed the queue below the spine, so the run of
 * Risk Controls → queue → Executor no longer read as one horizontal line.
 * Excluding it from the centring keeps the queue on the spine and hangs Blocked
 * above it, matching how signals actually move.
 */
const OFF_SPINE_OFFSET: Record<string, number> = {
  blocked_signals: -240,
};

/**
 * Lays nodes out in one vertical column per stage and returns an id → position
 * lookup.
 *
 * Bucketing by stage up front replaces the previous per-node rescans of the
 * whole node list (filter + findIndex inside a map, i.e. O(n²) — and O(n³) in
 * resetLayout, which nested a `find` inside that filter). It also removes the
 * duplicated layout arithmetic that had to be kept in sync by hand between the
 * initial build and the reset handler.
 */
function computeStagePositions(items: StagedNode[]): Map<string, { x: number; y: number }> {
  const byStage = new Map<number, StagedNode[]>();
  for (const item of items) {
    const bucket = byStage.get(item.stage);
    if (bucket) bucket.push(item);
    else byStage.set(item.stage, [item]);
  }

  const positions = new Map<string, { x: number; y: number }>();
  for (const [stage, members] of byStage) {
    const x = getStageX(stage);

    // Off-spine nodes are positioned relative to the centre line and take no
    // part in centring the rest of the column.
    const group = members.filter(item => OFF_SPINE_OFFSET[item.id] === undefined);
    for (const item of members) {
      const offset = OFF_SPINE_OFFSET[item.id];
      if (offset === undefined) continue;
      positions.set(item.id, {
        x,
        y: CY + offset - estimatedNodeHeight(item.id, item.kind) / 2,
      });
    }

    const groupGap = group.length > 2 ? 200 : 240;
    group.forEach((item, groupIdx) => {
      const estimatedH = estimatedNodeHeight(item.id, item.kind);
      let y = CY - ((group.length - 1) * groupGap) / 2 + (groupIdx * groupGap) - (estimatedH / 2);
      if (item.id === 'arbiter_node') y -= 245;
      positions.set(item.id, { x, y });
    });
  }
  return positions;
}

// ── Types ────────────────────────────────────────────────────────────────────

interface HubGraphProps {
  nodes: NodeState[];
  topology: PipelineTopology | null;
  nodeMetrics: Record<string, NodeMetrics>;
  recentEventSignatures: Record<string, { type: string; seqId: number; target: string }>;
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

function shallowEqualObject(
  a: Record<string, unknown> | undefined,
  b: Record<string, unknown> | undefined,
): boolean {
  if (a === b) return true;
  if (!a || !b) return a === b;
  const aKeys = Object.keys(a);
  const bKeys = Object.keys(b);
  if (aKeys.length !== bKeys.length) return false;
  return aKeys.every(key => Object.is(a[key], b[key]));
}

// ── CanvasIconButton ──────────────────────────────────────────────────────────

interface CanvasIconButtonProps {
  icon: ReactNode;
  onClick: () => void;
  title?: string;
  active?: boolean;
}

// Hoisted out of the component: these two objects are otherwise reallocated on
// every render of a component that re-renders with each engine event.
const CANVAS_BUTTON_BASE: CSSProperties = {
  width: 28,
  height: 28,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  borderRadius: 4,
  cursor: 'pointer',
  backdropFilter: 'blur(4px)',
  transition: 'all 0.15s ease',
  padding: 0,
};

const CANVAS_BUTTON_ACTIVE: CSSProperties = {
  ...CANVAS_BUTTON_BASE,
  background: 'rgba(57, 255, 20, 0.1)',
  border: '1px solid rgba(57, 255, 20, 0.4)',
  color: colors.acidGreen,
};

const CANVAS_BUTTON_IDLE: CSSProperties = {
  ...CANVAS_BUTTON_BASE,
  background: 'rgba(13, 13, 17, 0.85)',
  border: '1px solid rgba(26, 26, 34, 0.8)',
  color: colors.textDim,
};

function CanvasIconButton({ icon, onClick, title, active }: CanvasIconButtonProps) {
  return (
    <button
      type="button"
      onClick={onClick}
      title={title}
      aria-label={title}
      aria-pressed={active}
      // Hover is a CSS concern; the previous inline mouseenter/mouseleave
      // handlers wrote to element.style on every pointer crossing.
      className={active ? styles.canvasButtonActive : styles.canvasButton}
      style={active ? CANVAS_BUTTON_ACTIVE : CANVAS_BUTTON_IDLE}
    >
      {icon}
    </button>
  );
}

// ── Component ────────────────────────────────────────────────────────────────

export default function HubGraph({ nodes, topology, nodeMetrics, recentEventSignatures, onNodeClick }: HubGraphProps) {
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
    // Index the lookups this pass performs once per node, so the map below stays
    // linear instead of rescanning these arrays for every node.
    const perfByModel = new Map(modelEnsemble.map(m => [m.modelName, m]));
    const selectedAnalystSet = new Set(selectedAnalysts);
    const voteByRole = new Map<string, AnalystVote>();
    const voteByModel = new Map<string, AnalystVote>();
    for (const vote of analystVotes) {
      if (vote.role != null && !voteByRole.has(vote.role)) voteByRole.set(vote.role, vote);
      if (!voteByModel.has(vote.model)) voteByModel.set(vote.model, vote);
    }

    // The snapshot is the configured graph. Performance data enriches analyst
    // cards but must never add models that are absent from this topology.
    const configuredNodes = nodes.map(node => {
      if (node.kind !== 'analyst') return node;
      const model = (node.meta?.model as string | undefined) ?? node.label;
      const perf = perfByModel.get(model);
      if (!perf) return node;
      return {
        ...node,
        event_count: perf.totalSignals,
        meta: { ...(node.meta ?? {}), provider: perf.provider, accuracy: perf.accuracy },
      };
    });

    const allRiskPassed = lastRiskChecks.length === 0 || lastRiskChecks.every(c => c.passed);
    const positions = computeStagePositions(configuredNodes);

    return configuredNodes.map((n) => {
      const { x, y } = positions.get(n.id) ?? { x: getStageX(n.stage), y: CY };

      const metrics = nodeMetrics[n.id] ?? null;
      const liveLastEventAt = metrics?.lastEventAt
        ? new Date(metrics.lastEventAt).toISOString()
        : (n.last_event_at ?? null);

      // Pass extra model data through for analyst cards
      const provider = n.meta?.provider as string | undefined;
      const accuracy = n.meta?.accuracy as number | undefined;
      const role = n.meta?.role as string | undefined;
      const model = n.meta?.model as string | undefined;

      // Specialist roles are stable even when two roles share a model.
      const vote = role ? voteByRole.get(role) : voteByModel.get(model ?? n.label);

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
          meta: n.meta,
          accuracy,
          accentColor: provider ? modelAccentColor(provider) : undefined,
          // Per-analyst live vote
          ...(vote ? { verdict: vote.action, voteConfidence: vote.confidence, skipped: vote.skipped } : {}),
          isChosen: selectedAnalystSet.has(n.id),
          verdictEntries: role
            ? (verdictHistory[`role:${role}`] ?? [])
            : model ? (verdictHistory[model] ?? []) : (verdictHistory[n.label] ?? []),
          // Alpaca Feed: pass watchlist for marquee display
          ...(n.id === 'alpaca' ? { watchlistTickers, isOrbMode } : {}),
          // Live pipeline data
          ...(n.id === 'consensus' ? { analystConsensus, analystVotes, currentTicker } : {}),
          ...(n.id === 'specialist-risk-manager' ? { analystVotes, currentTicker } : {}),
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
    // One index for the three per-edge lookups below (source exists, target
    // exists, source kind) — previously three linear scans on every edge.
    const nodeById = new Map(rfNodes.map(n => [n.id, n]));
    const selectedAnalystSet = new Set(selectedAnalysts);

    const isNodeRecentlyActive = (nodeId: string): boolean => {
      const lastEventAt = nodeMetrics[nodeId]?.lastEventAt;
      if (!lastEventAt) return false;
      // Execution cards (executor, portfolio): treat as active if data is present.
      // Trades/positions are infrequent — a recency gate would show IDLE immediately
      // after the 5s window even though the last trade data is still displayed.
      const node = nodeById.get(nodeId);
      if (node?.data.kind === 'execution') {
        const allPassed = node.data.allRiskPassed !== false;
        if (node.id === 'executor') return !!(node.data.lastTrade) && allPassed;
        if (node.id === 'portfolio') return !!(node.data.portfolioState) && allPassed;
      }
      return Date.now() - lastEventAt < 10_000;
    };

    if (!topology) return [];
    const arbiterActive = isNodeRecentlyActive('arbiter_node');

    const makeEdge = (src: string, tgt: string, edgeKind: string) => {
      const exactEventActive = recentEventSignatures[src]?.target === tgt && isNodeRecentlyActive(src);
      let active = exactEventActive;
      if (src === 'indicator_engine' && nodeById.get(tgt)?.data.kind === 'analyst') {
        active = selectedAnalystSet.has(tgt) && isNodeRecentlyActive(tgt);
      }
      if (edgeKind === 'bypass') {
        active = exactEventActive;
      }
      if (edgeKind === 'conditional') {
        const isArbiterPath = src === 'arbiter_node' || tgt === 'arbiter_node';
        active = isArbiterPath ? exactEventActive && arbiterActive : exactEventActive;
      }
      if (src === 'risk_manager' && tgt === 'executor') {
        const allRiskPassed = lastRiskChecks.length === 0 || lastRiskChecks.every(c => c.passed);
        active = exactEventActive && allRiskPassed;
      }
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

    return topology.edges
      .filter(edge => nodeById.has(edge.source) && nodeById.has(edge.target))
      .map(edge => makeEdge(edge.source, edge.target, edge.kind));
  }, [rfNodes, topology, nodeMetrics, recentEventSignatures, lastRiskChecks, selectedAnalysts]);

  const [rfN, setRfNodes, onNodesChange] = useNodesState(rfNodes);
  const [rfE, setRfEdges, onEdgesChange] = useEdgesState(rfEdges);
  const previousTopologyRevision = useRef(topology?.revision ?? '');

  // Reconcile nodes and edges before paint. Live engine events update card data
  // many times per second; replacing nodes after paint discarded React Flow's
  // measured dimensions/handle internals and briefly left edges without valid
  // endpoints. Preserve the complete existing node shell while overlaying fresh
  // application data, and batch both topology updates in one layout effect.
  useLayoutEffect(() => {
    const nextRevision = topology?.revision ?? '';
    const topologyChanged = previousTopologyRevision.current !== nextRevision;
    setRfNodes((nds) => {
      const currentMap = new Map(nds.map(n => [n.id, n]));
      let changed = nds.length !== rfNodes.length;
      const nextNodes = rfNodes.map(n => {
        const existing = currentMap.get(n.id);
        if (!existing) {
          changed = true;
          return { ...n, draggable: !layoutLocked };
        }
        const position = topologyChanged ? n.position : existing.position;
        const nodeChanged = topologyChanged
          ? position.x !== existing.position.x || position.y !== existing.position.y
          : false;
        const dataChanged = !shallowEqualObject(
          existing.data as unknown as Record<string, unknown>,
          n.data as unknown as Record<string, unknown>,
        );
        const shellChanged = existing.draggable !== !layoutLocked;
        if (nodeChanged || dataChanged || shellChanged) changed = true;
        if (!nodeChanged && !dataChanged && !shellChanged) return existing;
        return {
          ...existing,
          ...n,
          position,
          dragging: existing.dragging,
          selected: existing.selected,
          draggable: !layoutLocked,
        };
      });
      return changed ? nextNodes : nds;
    });
    setRfEdges((edges) => {
      const currentMap = new Map(edges.map(edge => [edge.id, edge]));
      let changed = edges.length !== rfEdges.length;
      const nextEdges = rfEdges.map(edge => {
        const existing = currentMap.get(edge.id);
        if (!existing) {
          changed = true;
          return edge;
        }
        const edgeChanged = existing.source !== edge.source ||
          existing.target !== edge.target ||
          existing.type !== edge.type ||
          !shallowEqualObject(existing.style as Record<string, unknown> | undefined, edge.style as Record<string, unknown> | undefined) ||
          !shallowEqualObject(existing.data as Record<string, unknown> | undefined, edge.data as Record<string, unknown> | undefined);
        if (edgeChanged) changed = true;
        return edgeChanged ? { ...existing, ...edge, selected: existing.selected } : existing;
      });
      return changed ? nextEdges : edges;
    });
    previousTopologyRevision.current = nextRevision;
    if (topologyChanged) {
      window.setTimeout(() => fitViewRef.current?.({ padding: 0.15, duration: 400 }), 50);
    }
  }, [rfNodes, rfEdges, topology?.revision, layoutLocked, setRfNodes, setRfEdges]);

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

  const prevSignatures = useRef<Record<string, { type: string; seqId: number; target: string }>>({});
  useEffect(() => {
    for (const [nodeId, sig] of Object.entries(recentEventSignatures)) {
      const prev = prevSignatures.current[nodeId];
      if (prev && prev.type === sig.type && prev.seqId === sig.seqId) continue;
      prevSignatures.current[nodeId] = sig;

      // AnalysisCompleted updates the verdict-producing node itself. The
      // subsequent SignalGenerated event is what traverses into risk controls.
      if (sig.type === 'AnalysisCompleted') continue;

      // Route only across the exact edge named by the event contract. This
      // prevents a source with several outcomes from lighting every branch.
      const outgoingEdges = rfE.filter(e => e.source === nodeId && e.target === sig.target);
      outgoingEdges.forEach(edge => {
        const src = rfNodeMap.get(edge.source);
        const tgt = rfNodeMap.get(edge.target);
        if (!src || !tgt) return;

        const srcH = estimatedNodeHeight(src.id, src.data.kind);
        const tgtH = estimatedNodeHeight(tgt.id, tgt.data.kind);
        const fromY = src.position.y + srcH / 2;
        const toY = tgt.position.y + tgtH / 2;

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
      const stageById = new Map(nodes.map(configured => [configured.id, configured.stage]));
      const positions = computeStagePositions(
        currentNodes.map(n => ({ id: n.id, stage: stageById.get(n.id) ?? 0, kind: n.data.kind })),
      );
      return currentNodes.map(n => {
        const position = positions.get(n.id);
        return position ? { ...n, position } : n;
      });
    });
    // Give React a tick to apply new positions before fitting
    setTimeout(() => {
      fitViewRef.current?.({ padding: 0.15, duration: 400 });
    }, 50);
  }, [nodes, setRfNodes]);

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
      <div className={styles.canvasControls}>
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
