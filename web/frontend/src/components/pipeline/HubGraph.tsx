/**
 * HubGraph — hub-and-spoke pipeline visualisation.
 *
 * ENGINE sits at centre. Nodes radiate outward by kind:
 *   output (closest) → processing (mid) → source (outermost)
 *
 * Animated particles travel along edges powered by framer-motion.
 * Clicking a node opens the DrillDownPanel.
 */
import { useCallback, useMemo, useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { LayoutGrid, Lock, LockOpen, Crosshair } from 'lucide-react';
import {
  ReactFlow,
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

// ── Constants ────────────────────────────────────────────────────────────────

const NODE_TYPES = { pipelineNode: PipelineNodeCard };

// Column X-offsets — 40px gap between card edges (NODE_W=280)
const COL = {
  source:        60,
  arbiter:       440,   // 60 + 280 + 100
  analyst:       820,   // 440 + 280 + 100
  synthesis:     1200,  // 820 + 280 + 100
  arbiter_final: 1580,  // 1200 + 280 + 100
  execution:     1960,  // 1580 + 280 + 100
};

const CY = 400;

// Node dimensions
const NODE_W = 280;
const NODE_H = 140;

const EDGE_ACTIVE = colors.acidGreen;

// ── Helpers ──────────────────────────────────────────────────────────────────

function getColumnX(kind: string): number {
  if (kind === 'source') return COL.source;
  if (kind === 'arbiter') return COL.arbiter;
  if (kind === 'ticker') return COL.source;
  if (kind.includes('analyst')) return COL.analyst;
  if (kind === 'synthesis') return COL.synthesis;
  if (kind === 'arbiter_final') return COL.arbiter_final;
  if (kind === 'execution') return COL.execution;
  return COL.source;
}

function modelAccentColor(provider: string): string {
  switch (provider.toLowerCase()) {
    case 'anthropic': return colors.analystTechnical;
    case 'openai':    return colors.analystMomentum;
    case 'google':    return colors.analystSentiment;
    default:          return colors.analystMacro;
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
  toX: number;   toY: number;
  color: string;
  progress: number;
  speed: number;
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
  const { analystConsensus, analystVotes, lastRiskChecks, lastTrade, portfolioState } = useEngineSocketContext();

  // ── Build React Flow nodes ──────────────────────────────────────────────

  const rfNodes = useMemo<Node<PipelineNodeData>[]>(() => {
    // Config-seeded analyst nodes from WS snapshot (id prefix "analyst-") or legacy "llm_analysis"
    const snapshotAnalystNodes = nodes.filter(n => n.id.startsWith('analyst-') || n.id === 'llm_analysis');

    // modelEnsemble provides richer data (accuracy, totalSignals) once available.
    // Prefer modelEnsemble when populated, else fall back to snapshot nodes.
    const analystNodes: NodeState[] = modelEnsemble.length > 0
      ? modelEnsemble.map(m => ({
          id: m.id,
          label: m.modelName,
          kind: 'analyst' as const,
          event_count: m.totalSignals,
          last_event_at: undefined,
          meta: { provider: m.provider, accuracy: m.accuracy },
        }))
      : snapshotAnalystNodes.length > 0
        ? snapshotAnalystNodes.map(n => ({
            id: n.id,
            label: n.label,
            kind: 'analyst' as const,
            event_count: 0,
            last_event_at: undefined,
            meta: { provider: (n.meta?.provider as string) ?? '' },
          }))
        : [{ id: 'llm_analysis', label: 'LLM Analysis', kind: 'analyst' as const, event_count: 0, last_event_at: undefined }];

    // Pipeline nodes with real backend IDs mapped to visual kinds.
    const PIPELINE_NODES: NodeState[] = [
      // Sources (col 0)
      { id: 'alpaca',           label: 'Alpaca Feed',       kind: 'source',        event_count: 0, last_event_at: undefined },
      { id: 'news_api',         label: nodes.find(n => n.id === 'news_api')?.label ?? 'News Feed', kind: 'source', event_count: 0, last_event_at: undefined },
      // Arbiter (col 1)
      { id: 'indicator_engine', label: 'Indicator Engine',  kind: 'arbiter',       event_count: 0, last_event_at: undefined },
      // Analyst (col 2) — dynamic
      ...analystNodes,
      // Synthesis (col 3)
      { id: 'consensus',        label: 'Consensus',         kind: 'synthesis',     event_count: 0, last_event_at: undefined },
      // Arbiter Final (col 3.5) — new
      { id: 'arbiter_node',     label: 'Arbiter',           kind: 'arbiter_final', event_count: 0, last_event_at: undefined },
      // Synthesis (col 3 continued)
      { id: 'risk_manager',     label: 'Risk Manager',      kind: 'synthesis',     event_count: 0, last_event_at: undefined },
      // Execution (col 4)
      { id: 'executor',         label: 'Executor',          kind: 'execution',     event_count: 0, last_event_at: undefined },
      { id: 'portfolio',        label: 'Portfolio',         kind: 'execution',     event_count: 0, last_event_at: undefined },
    ];

    // Merge live metrics from backend into pipeline nodes.
    const mergedPipeline = PIPELINE_NODES.map(pn => {
      const live = nodes.find(n => n.id === pn.id);
      return live
        ? { ...pn, event_count: live.event_count, last_event_at: live.last_event_at }
        : pn;
    });

    const allNodes = [...mergedPipeline];

    // Stack nodes vertically within each kind group
    const kindGroups: Record<string, typeof allNodes> = {};
    for (const n of allNodes) {
      if (!kindGroups[n.kind]) kindGroups[n.kind] = [];
      kindGroups[n.kind].push(n);
    }

    return allNodes.map((n) => {
      const group = kindGroups[n.kind] || [n];
      // synthesis needs extra vertical gap so arbiter_final can sit between consensus and risk_manager
      const groupGap = n.kind === 'synthesis' ? 320 : 200;
      const groupStartY = CY - ((group.length - 1) * groupGap) / 2;
      const groupIdx = group.findIndex(gn => gn.id === n.id);
      const x = getColumnX(n.kind);
      const y = group.length > 1 ? groupStartY + groupIdx * groupGap : CY - NODE_H / 2;

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
          // Alpaca Feed: pass watchlist for marquee display
          ...(n.id === 'alpaca' ? { watchlistTickers, isOrbMode } : {}),
          // Live pipeline data
          ...(n.id === 'consensus' ? { analystConsensus } : {}),
          ...(n.id === 'risk_manager' ? { riskChecks: lastRiskChecks } : {}),
          ...(n.id === 'executor' ? { lastTrade } : {}),
          ...(n.id === 'portfolio' ? { portfolioState } : {}),
        },
        draggable: true,
        selectable: true,
      };
    });
  }, [nodes, nodeMetrics, recentEventSignatures, onNodeClick, watchlistTickers, modelEnsemble, isOrbMode, analystConsensus, analystVotes, lastRiskChecks, lastTrade, portfolioState]);

  // ── Build React Flow edges ──────────────────────────────────────────────

  const rfEdges = useMemo<Edge[]>(() => {
    const isNodeRecentlyActive = (nodeId: string): boolean => {
      const lastEventAt = nodeMetrics[nodeId]?.lastEventAt;
      if (!lastEventAt) return false;
      return Date.now() - lastEventAt < 10_000;
    };

    const edges: Edge[] = [];
    const sourceIds = rfNodes.filter(n => n.data.kind === 'source').map(n => n.id);
    const arbiterId = rfNodes.find(n => n.data.kind === 'arbiter')?.id;
    const analystIds = rfNodes.filter(n => n.data.kind === 'analyst').map(n => n.id);
    const arbiterFinalId = rfNodes.find(n => n.data.kind === 'arbiter_final')?.id;
    const executionIds = rfNodes.filter(n => n.data.kind === 'execution').map(n => n.id);

    const makeEdge = (src: string, tgt: string, opts?: { forceInactive?: boolean }) => {
      const active = !opts?.forceInactive && isNodeRecentlyActive(src);
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
        type: 'smoothstep',
      };
    };

    if (arbiterId) {
      sourceIds.forEach(sid => edges.push(makeEdge(sid, arbiterId)));
      analystIds.forEach(aid => {
        // Only light the edge to an analyst if their vote was NOT skipped
        const analystNode = rfNodes.find(n => n.id === aid);
        const modelName = (analystNode?.data as PipelineNodeData)?.label ?? '';
        // Match by analyst name (node label) only — no provider fallback.
        const vote = analystVotes.find(v => v.model === modelName);
        const wasInvoked = vote ? !vote.skipped : false;
        edges.push(makeEdge(arbiterId, aid, { forceInactive: analystVotes.length > 0 && !wasInvoked }));
      });
    }

    // All analysts → consensus
    analystIds.forEach(aid => edges.push(makeEdge(aid, 'consensus')));

    // Always draw both paths; edge activity is driven by isNodeRecentlyActive.
    // Fast path: consensus → risk_manager (lights up via SignalGenerated)
    // Arbiter path: consensus → arbiter_node → risk_manager (lights up via ArbiterInvoked)
    edges.push(makeEdge('consensus', 'risk_manager'));
    if (arbiterFinalId) {
      edges.push(makeEdge('consensus', arbiterFinalId));
      edges.push(makeEdge(arbiterFinalId, 'risk_manager'));
    }

    // risk_manager → all execution nodes
    executionIds.forEach(eid => edges.push(makeEdge('risk_manager', eid)));

    return edges;
  }, [rfNodes, nodeMetrics, analystVotes]);

  const [rfN, setRfNodes, onNodesChange] = useNodesState(rfNodes);
  const [rfE, setRfEdges, onEdgesChange] = useEdgesState(rfEdges);

  useEffect(() => { setRfNodes(rfNodes); }, [rfNodes, setRfNodes]);
  useEffect(() => { setRfEdges(rfEdges); }, [rfEdges, setRfEdges]);

  // Center view on initial render
  useEffect(() => {
    const timer = setTimeout(() => {
      fitViewRef.current?.({ padding: 0.15, duration: 300 });
    }, 100);
    return () => clearTimeout(timer);
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  // Patch draggable on all nodes when lock state changes
  useEffect(() => {
    setRfNodes(nds => nds.map(n => ({ ...n, draggable: !layoutLocked })));
  }, [layoutLocked, setRfNodes]);

  const handleNodeClick: NodeMouseHandler = useCallback((_evt, node) => {
    onNodeClick(node.id);
  }, [onNodeClick]);

  const viewportRef = useRef({ x: 0, y: 0, zoom: 1 });
  const handleViewportChange = useCallback((vp: { x: number; y: number; zoom: number }) => {
    viewportRef.current = vp;
  }, []);

  // ── Particle Spawning ──────────────────────────────────────────────────
  const rfNodeMap = useMemo(() => {
    const m = new Map<string, typeof rfNodes[number]>();
    for (const n of rfNodes) m.set(n.id, n);
    return m;
  }, [rfNodes]);

  const prevSignatures = useRef<Record<string, { type: string; seqId: number }>>({});
  useEffect(() => {
    for (const [nodeId, sig] of Object.entries(recentEventSignatures)) {
      const prev = prevSignatures.current[nodeId];
      if (prev && prev.type === sig.type && prev.seqId === sig.seqId) continue;
      prevSignatures.current[nodeId] = sig;

      // Find the edge(s) this node is a source of
      const outgoingEdges = rfE.filter(e => e.source === nodeId);
      outgoingEdges.forEach(edge => {
        const src = rfNodeMap.get(edge.source);
        const tgt = rfNodeMap.get(edge.target);
        if (!src || !tgt) return;

        particlesRef.current.push({
          id: `${nodeId}-${edge.target}-${Date.now()}-${Math.random()}`,
          fromX: src.position.x + NODE_W / 2,
          fromY: src.position.y + NODE_H / 2,
          toX: tgt.position.x + NODE_W / 2,
          toY: tgt.position.y + NODE_H / 2,
          color: particleColor(sig.type),
          progress: 0,
          speed: 0.001 + Math.random() * 0.0005,
        });
      });
    }
  }, [recentEventSignatures, rfNodes, rfE]);

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
      const x = (p.fromX + (p.toX - p.fromX) * p.progress) * vp.zoom + vp.x;
      const y = (p.fromY + (p.toY - p.fromY) * p.progress) * vp.zoom + vp.y;

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
        const group = groups[n.data.kind] || [n];
        // synthesis needs extra vertical gap so arbiter_final can sit between consensus and risk_manager
        const groupGap = n.data.kind === 'synthesis' ? 320 : 200;
        const groupStartY = CY - ((group.length - 1) * groupGap) / 2;
        const groupIdx = group.findIndex(gn => gn.id === n.id);
        const x = getColumnX(n.data.kind);
        const y = group.length > 1 ? groupStartY + groupIdx * groupGap : CY - NODE_H / 2;
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
        minZoom={0.2}
        maxZoom={1.5}
        defaultViewport={{ x: 20, y: 60, zoom: 0.6 }}
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
