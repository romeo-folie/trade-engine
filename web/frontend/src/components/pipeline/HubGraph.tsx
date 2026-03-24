/**
 * HubGraph — hub-and-spoke pipeline visualisation.
 *
 * ENGINE sits at centre. Nodes radiate outward by kind:
 *   output (closest) → processing (mid) → source (outermost)
 *
 * Animated particles travel along edges powered by framer-motion.
 * Clicking a node opens the DrillDownPanel.
 */
import { useCallback, useMemo, useEffect, useRef } from 'react';
import {
  ReactFlow,
  Background,
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
import PipelineNodeCard, { type PipelineNodeData } from './PipelineNodeCard';
import { particleColor } from '../../types/protocol';
import { ChamferedBox } from '../common/ChamferedBox';
import { colors } from '../../styles/tokens';
import styles from './HubGraph.module.css';

// ── Constants ────────────────────────────────────────────────────────────────

const NODE_TYPES = { pipelineNode: PipelineNodeCard };

// Column X-offsets based on design spec
const COL = {
  source: 0,
  arbiter: 350,
  analyst: 700,
  synthesis: 1050,
  execution: 1400
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
  if (kind.includes('analyst')) return COL.analyst;
  if (kind === 'synthesis') return COL.synthesis;
  if (kind === 'execution') return COL.execution;
  return COL.source;
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

// ── Component ────────────────────────────────────────────────────────────────

export default function HubGraph({ nodes, nodeMetrics, recentEventSignatures, onNodeClick }: HubGraphProps) {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const wrapperRef = useRef<HTMLDivElement>(null);
  const particlesRef = useRef<Particle[]>([]);
  const lastFrameRef = useRef<number>(0);

  // ── Build React Flow nodes ──────────────────────────────────────────────

  const rfNodes = useMemo<Node<PipelineNodeData>[]>(() => {
    // Pipeline nodes with real backend IDs mapped to visual kinds.
    // Only nodes that have a meaningful card template are included.
    const PIPELINE_NODES: NodeState[] = [
      // Sources (col 0)
      { id: 'alpaca',           label: 'Alpaca Feed',       kind: 'source',    event_count: 0, last_event_at: undefined },
      { id: 'news_api',         label: 'News Feed',         kind: 'source',    event_count: 0, last_event_at: undefined },
      // Arbiter (col 1)
      { id: 'indicator_engine', label: 'Indicator Engine',  kind: 'arbiter',   event_count: 0, last_event_at: undefined },
      // Analyst (col 2)
      { id: 'llm_analysis',     label: 'LLM Analysis',      kind: 'analyst',   event_count: 0, last_event_at: undefined },
      // Synthesis (col 3)
      { id: 'consensus',        label: 'Consensus',         kind: 'synthesis', event_count: 0, last_event_at: undefined },
      { id: 'risk_manager',     label: 'Risk Manager',      kind: 'synthesis', event_count: 0, last_event_at: undefined },
      // Execution (col 4)
      { id: 'executor',         label: 'Executor',          kind: 'execution', event_count: 0, last_event_at: undefined },
      { id: 'portfolio',        label: 'Portfolio',         kind: 'execution', event_count: 0, last_event_at: undefined },
    ];

    // Merge live metrics from backend into pipeline nodes.
    // Only nodes in PIPELINE_NODES are rendered — unknown backend nodes are dropped.
    const allNodes = PIPELINE_NODES.map(pn => {
      const live = nodes.find(n => n.id === pn.id);
      return live
        ? { ...pn, event_count: live.event_count, last_event_at: live.last_event_at }
        : pn;
    });

    // Stack nodes vertically within each kind group
    const kindGroups: Record<string, typeof allNodes> = {};
    for (const n of allNodes) {
      if (!kindGroups[n.kind]) kindGroups[n.kind] = [];
      kindGroups[n.kind].push(n);
    }

    return allNodes.map((n) => {
      const group = kindGroups[n.kind] || [n];
      const groupGap = 200;
      const groupStartY = CY - ((group.length - 1) * groupGap) / 2;
      const groupIdx = group.findIndex(gn => gn.id === n.id);

      const x = getColumnX(n.kind);
      const y = group.length > 1 ? groupStartY + groupIdx * groupGap : CY - NODE_H / 2;

      const metrics = nodeMetrics[n.id] ?? null;
      const liveLastEventAt = metrics?.lastEventAt
        ? new Date(metrics.lastEventAt).toISOString()
        : (n.last_event_at ?? null);

      return {
        id: n.id,
        type: 'pipelineNode',
        position: { x, y },
        data: {
          nodeId: n.id,
          label: n.label,
          kind: n.kind,
          eventCount: n.event_count,
          lastEventAt: liveLastEventAt,
          metrics,
          isHub: false,
          lastEventType: recentEventSignatures[n.id]?.type ?? 'buy_signal',
          onNodeClick,
        },
        draggable: true,
        selectable: true,
      };
    });
  }, [nodes, nodeMetrics, recentEventSignatures, onNodeClick]);

  // ── Build React Flow edges ──────────────────────────────────────────────

  const rfEdges = useMemo<Edge[]>(() => {
    const edges: Edge[] = [];
    const sourceIds = rfNodes.filter(n => n.data.kind === 'source').map(n => n.id);
    const arbiterId = rfNodes.find(n => n.data.kind === 'arbiter')?.id;
    const analystIds = rfNodes.filter(n => n.data.kind.includes('analyst')).map(n => n.id);
    const synthesisIds = rfNodes.filter(n => n.data.kind === 'synthesis').map(n => n.id);
    const executionIds = rfNodes.filter(n => n.data.kind === 'execution').map(n => n.id);

    const makeEdge = (src: string, tgt: string) => ({
      id: `e-${src}-${tgt}`,
      source: src,
      target: tgt,
      style: {
        stroke: EDGE_ACTIVE,
        strokeWidth: 1,
        strokeDasharray: '5 5',
        animation: 'edge-dash 0.5s linear infinite',
      },
      type: 'smoothstep',
    });

    if (arbiterId) {
      sourceIds.forEach(sid => edges.push(makeEdge(sid, arbiterId)));
      analystIds.forEach(aid => edges.push(makeEdge(arbiterId, aid)));
    }
    // All analysts → all synthesis nodes
    synthesisIds.forEach(sid => {
      analystIds.forEach(aid => edges.push(makeEdge(aid, sid)));
    });
    // All synthesis → all execution nodes
    executionIds.forEach(eid => {
      synthesisIds.forEach(sid => edges.push(makeEdge(sid, eid)));
    });

    return edges;
  }, [rfNodes]);

  const [rfN, setRfNodes, onNodesChange] = useNodesState(rfNodes);
  const [rfE, setRfEdges, onEdgesChange] = useEdgesState(rfEdges);

  useEffect(() => { setRfNodes(rfNodes); }, [rfNodes, setRfNodes]);
  useEffect(() => { setRfEdges(rfEdges); }, [rfEdges, setRfEdges]);

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
        const k = n.data.kind;
        if (!groups[k]) groups[k] = [];
        groups[k].push(n);
      }
      return currentNodes.map(n => {
        const group = groups[n.data.kind] || [n];
        const groupGap = 200;
        const groupStartY = CY - ((group.length - 1) * groupGap) / 2;
        const groupIdx = group.findIndex(gn => gn.id === n.id);
        const x = getColumnX(n.data.kind);
        const y = group.length > 1 ? groupStartY + groupIdx * groupGap : CY - NODE_H / 2;
        return { ...n, position: { x, y } };
      });
    });
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
        nodeTypes={NODE_TYPES}
        fitView
        fitViewOptions={{ padding: 0.1 }}
        proOptions={{ hideAttribution: true }}
      >
        <Background variant={BackgroundVariant.Dots} gap={20} size={1} color="#1A1A22" style={{ opacity: 0.2 }} />
      </ReactFlow>
      <canvas ref={canvasRef} className={styles.particleCanvas} style={{ pointerEvents: 'none' }} />
      
      {/* Floating Controls */}
      <div style={{
        position: 'absolute',
        top: 20,
        right: 20,
        zIndex: 10,
        display: 'flex',
        gap: 12
      }}>
        <ChamferedBox
          chamfer={6}
          backgroundColor="rgba(13, 13, 17, 0.8)"
          borderColor={colors.borderMuted}
          onClick={resetLayout}
          style={{ 
            padding: '6px 12px',
            backdropFilter: 'blur(4px)',
          }}
        >
          <span style={{ 
            fontSize: '10px',
            fontWeight: 700,
            color: colors.textDim,
            letterSpacing: '0.05em'
          }}>
            RESET LAYOUT
          </span>
        </ChamferedBox>
      </div>
    </div>
  );
}
