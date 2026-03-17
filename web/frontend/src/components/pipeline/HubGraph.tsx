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
import PipelineNodeCard from './PipelineNodeCard';
import { particleColor } from '../../types/protocol';
import styles from './HubGraph.module.css';

// ── Constants ────────────────────────────────────────────────────────────────

const NODE_TYPES = { pipelineNode: PipelineNodeCard };

// Radial ring radii (px, from centre)
const RADII = { output: 180, processing: 320, source: 460 };

// Canvas centre (React Flow uses a virtual coordinate system)
const CX = 600;
const CY = 400;

// Node dimensions per spec
const NODE_W = 220;
const NODE_H = 120;
const HUB_W  = 160;
const HUB_H  = 80;

// Edge styling
const EDGE_ACTIVE   = '#39FF14';
const EDGE_INACTIVE = '#1A1A22';

// ── Helpers ──────────────────────────────────────────────────────────────────

function kindOrder(kind: NodeState['kind']): 'output' | 'processing' | 'source' {
  if (kind === 'output')     return 'output';
  if (kind === 'processing') return 'processing';
  return 'source';
}

function radialPosition(index: number, total: number, radius: number): { x: number; y: number } {
  const angle = (2 * Math.PI * index) / total - Math.PI / 2;
  return {
    x: CX + radius * Math.cos(angle) - NODE_W / 2,
    y: CY + radius * Math.sin(angle) - NODE_H / 2,
  };
}

// ── Types ────────────────────────────────────────────────────────────────────

interface HubGraphProps {
  nodes: NodeState[];
  nodeMetrics: Record<string, NodeMetrics>;
  recentEventSignatures: Record<string, { type: string; ts: number }>; // nodeId → event signature
  onNodeClick: (nodeId: string) => void;
}

// ── Particle layer ───────────────────────────────────────────────────────────

interface Particle {
  id: string;
  fromX: number; fromY: number;
  toX: number;   toY: number;
  color: string;
  progress: number; // 0→1
  speed: number;    // progress units/ms
}

// Particle canvas is rendered as a sibling overlay in CSS.
// Uses imperative canvas API for performance.

// ── Component ────────────────────────────────────────────────────────────────

export default function HubGraph({ nodes, nodeMetrics, recentEventSignatures, onNodeClick }: HubGraphProps) {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const wrapperRef = useRef<HTMLDivElement>(null);
  const particlesRef = useRef<Particle[]>([]);
  const lastFrameRef = useRef<number>(0);

  // ── Build React Flow nodes ──────────────────────────────────────────────

  const rfNodes = useMemo<Node[]>(() => {
    // Group by kind
    const groups: Record<'output' | 'processing' | 'source', NodeState[]> = {
      output: [], processing: [], source: [],
    };
    for (const n of nodes) groups[kindOrder(n.kind)].push(n);

    const result: Node[] = [
      {
        id: '__hub__',
        type: 'pipelineNode',
        position: { x: CX - HUB_W / 2, y: CY - HUB_H / 2 },
        data: {
          nodeId: '__hub__',
          label: 'ENGINE',
          kind: 'hub',
          eventCount: 0,
          lastEventAt: null,
          metrics: null,
          isHub: true,
          lastEventType: null,
          onNodeClick,
        },
        draggable: false,
        selectable: true,
      },
    ];

    for (const [kindKey, group] of Object.entries(groups) as [keyof typeof groups, NodeState[]][]) {
      const radius = RADII[kindKey];
      group.forEach((n, i) => {
        const pos = radialPosition(i, group.length, radius);
        const metrics = nodeMetrics[n.id] ?? null;
        // Snapshot event_count is the authoritative total; metrics is used only
        // for live activity indicators (lastEventAt / status dot).
        const liveLastEventAt = metrics?.lastEventAt
          ? new Date(metrics.lastEventAt).toISOString()
          : (n.last_event_at ?? null);
        result.push({
          id: n.id,
          type: 'pipelineNode',
          position: pos,
          data: {
            nodeId: n.id,
            label: n.label,
            kind: n.kind,
            eventCount: n.event_count,
            lastEventAt: liveLastEventAt,
            metrics,
            isHub: false,
            lastEventType: recentEventSignatures[n.id]?.type ?? null,
            onNodeClick,
          },
          draggable: false,
          selectable: true,
        });
      });
    }
    return result;
  }, [nodes, nodeMetrics, recentEventSignatures, onNodeClick]);

  // ── Build React Flow edges ──────────────────────────────────────────────

  const rfEdges = useMemo<Edge[]>(() => {
    return nodes.map(n => {
      const isActive = (nodeMetrics[n.id]?.lastEventAt ?? 0) > Date.now() - 5000;
      return {
        id: `hub-${n.id}`,
        source: '__hub__',
        target: n.id,
        animated: false, // particles handled by canvas overlay
        style: {
          stroke: isActive ? EDGE_ACTIVE : EDGE_INACTIVE,
          strokeWidth: 1,
          strokeDasharray: isActive ? '4 4' : undefined,
        },
        type: 'straight',
      };
    });
  }, [nodes, nodeMetrics]);

  const [rfN, setRfNodes, onNodesChange] = useNodesState(rfNodes);
  const [rfE, setRfEdges, onEdgesChange] = useEdgesState(rfEdges);

  // Sync external state changes into React Flow
  useEffect(() => { setRfNodes(rfNodes); }, [rfNodes]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => { setRfEdges(rfEdges); }, [rfEdges]); // eslint-disable-line react-hooks/exhaustive-deps

  // ── Node click handler ──────────────────────────────────────────────────

  const handleNodeClick: NodeMouseHandler = useCallback((_evt, node) => {
    if (node.id !== '__hub__') onNodeClick(node.id);
  }, [onNodeClick]);

  // ── Particle spawning ───────────────────────────────────────────────────

  // Each node in recentEventSignatures spawns a particle toward/from hub.
  // Track signature (type + timestamp) so back-to-back same-type events still spawn.
  const prevSignatures = useRef<Record<string, { type: string; ts: number }>>({});
  useEffect(() => {
    const hubX = CX;
    const hubY = CY;

    for (const [nodeId, sig] of Object.entries(recentEventSignatures)) {
      const prev = prevSignatures.current[nodeId];
      if (prev && prev.type === sig.type && prev.ts === sig.ts) continue; // no new event
      prevSignatures.current[nodeId] = sig;

      const node = rfNodes.find(n => n.id === nodeId);
      if (!node) continue;

      const nX = node.position.x + NODE_W / 2;
      const nY = node.position.y + NODE_H / 2;

      // source nodes: particle flows inward (source → hub)
      // output nodes: particle flows outward (hub → output)
      const [fromX, fromY, toX, toY] = node.data.kind === 'source'
        ? [nX, nY, hubX, hubY]
        : [hubX, hubY, nX, nY];

      particlesRef.current.push({
        id: `${nodeId}-${Date.now()}-${Math.random()}`,
        fromX, fromY, toX, toY,
        color: particleColor(sig.type),
        progress: 0,
        speed: 0.0008 + Math.random() * 0.0004, // ~1.2–2s per edge
      });

      // Cap to avoid memory blowup
      if (particlesRef.current.length > 80) {
        particlesRef.current = particlesRef.current.slice(-60);
      }
    }
  }, [recentEventSignatures, rfNodes]);

  // ── Viewport tracking for particle canvas ────────────────────────────────

  const viewportRef = useRef({ x: 0, y: 0, zoom: 1 });
  const handleViewportChange = useCallback((vp: { x: number; y: number; zoom: number }) => {
    viewportRef.current = vp;
  }, []);

  // ── HiDPI canvas sizing ─────────────────────────────────────────────────

  useEffect(() => {
    const canvas = canvasRef.current;
    const wrapper = wrapperRef.current;
    if (!canvas || !wrapper) return;

    const resizeCanvas = () => {
      const rect = wrapper.getBoundingClientRect();
      const dpr = window.devicePixelRatio || 1;
      canvas.width = rect.width * dpr;
      canvas.height = rect.height * dpr;
      canvas.style.width = `${rect.width}px`;
      canvas.style.height = `${rect.height}px`;
      const ctx = canvas.getContext('2d');
      if (ctx) ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    };

    resizeCanvas();
    const observer = new ResizeObserver(resizeCanvas);
    observer.observe(wrapper);
    return () => observer.disconnect();
  }, []);

  // ── Canvas particle render loop ─────────────────────────────────────────

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

      // Map flow-space coordinates to screen-space using viewport transform
      const flowX = p.fromX + (p.toX - p.fromX) * p.progress;
      const flowY = p.fromY + (p.toY - p.fromY) * p.progress;
      const x = flowX * vp.zoom + vp.x;
      const y = flowY * vp.zoom + vp.y;

      const radius = 6 * vp.zoom;
      const dotRadius = 2.5 * vp.zoom;

      // Glow
      ctx.beginPath();
      const grad = ctx.createRadialGradient(x, y, 0, x, y, radius);
      grad.addColorStop(0, p.color + 'ff');
      grad.addColorStop(1, p.color + '00');
      ctx.fillStyle = grad;
      ctx.arc(x, y, radius, 0, Math.PI * 2);
      ctx.fill();

      // Core dot
      ctx.beginPath();
      ctx.fillStyle = p.color;
      ctx.arc(x, y, dotRadius, 0, Math.PI * 2);
      ctx.fill();

      return p.progress < 1;
    });
  });

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
        fitViewOptions={{ padding: 0.15, maxZoom: 1.2 }}
        minZoom={0.4}
        maxZoom={2}
        proOptions={{ hideAttribution: true }}
        panOnDrag
        zoomOnScroll
      >
        <Background
          variant={BackgroundVariant.Dots}
          gap={20}
          size={1}
          color="#1A1A22"
          style={{ opacity: 0.2 }}
        />
      </ReactFlow>
      <canvas
        ref={canvasRef}
        className={styles.particleCanvas}
        style={{ pointerEvents: 'none' }}
      />
    </div>
  );
}
