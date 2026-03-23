import { memo, useMemo } from 'react';
import { Handle, Position, type NodeProps } from '@xyflow/react';
import { motion } from 'framer-motion';
import { colors, typography } from '../styles/tokens';
import { CornerBrackets } from './common/CornerBrackets';
import { ChamferedBox } from './common/ChamferedBox';
import type { NodeState } from '../types/protocol';
import type { NodeMetrics } from '../hooks/useEngineSocket';

interface PipelineNodeData {
  nodeState: NodeState;
  isActive: boolean;
  metrics?: NodeMetrics;
  [key: string]: unknown;
}

/**
 * Maps node logic to visual roles as per DESIGN_SPEC.md
 */
type NodeRole = 'SIGNAL_SOURCE' | 'ARBITER' | 'ANALYST' | 'SYNTHESIS' | 'EXECUTION' | 'DEFAULT';

function getNodeRole(node: NodeState): NodeRole {
  if (node.kind === 'source') return 'SIGNAL_SOURCE';
  if (node.id === 'data_enrichment' || node.id === 'indicator_engine' || node.id === 'prompt_assembly') return 'ARBITER';
  if (node.id.includes('analysis') || node.id.includes('analyst')) return 'ANALYST';
  if (node.id === 'consensus' || node.id === 'risk_manager') return 'SYNTHESIS';
  if (node.id === 'executor' || node.id === 'portfolio') return 'EXECUTION';
  return 'DEFAULT';
}

export const PipelineNode = memo(function PipelineNode({ data }: NodeProps) {
  const { nodeState, isActive } = data as PipelineNodeData;
  const role = useMemo(() => getNodeRole(nodeState), [nodeState]);
  
  // Extract meta data with defaults for "polishing"
  const meta = nodeState.meta || {};
  const label = nodeState.label || 'UNKNOWN';
  const subLabel = (meta.subLabel as string) || '';
  const value = (meta.value as string) || '';
  const trend = (meta.trend as string) || ''; // e.g. "+2.4%"
  const status = (meta.status as string) || 'ACTIVE'; // e.g. "LONG", "NEUTRAL"
  
  // Analyst specific
  const analystType = (meta.analystType as keyof typeof colors) || 'analystTechnical';
  const analystColor = colors[analystType] || colors.indigo;

  const renderContent = () => {
    switch (role) {
      case 'SIGNAL_SOURCE':
        return (
          <div style={{ padding: '12px 16px' }}>
            <div style={{ color: colors.textDim, fontSize: 10, fontFamily: typography.mono, textTransform: 'uppercase', marginBottom: 4 }}>SIGNAL SOURCE</div>
            <div style={{ color: colors.textMuted, fontSize: 11, fontFamily: typography.mono, marginBottom: 8 }}>{subLabel || 'BTC / USDT'}</div>
            <div style={{ color: colors.textPrimary, fontSize: 20, fontFamily: typography.sans, fontWeight: 700 }}>{label || 'BTC'}</div>
            <div style={{ display: 'flex', alignItems: 'baseline', gap: 8, marginTop: 4 }}>
              <div style={{ color: colors.textPrimary, fontSize: 14, fontFamily: typography.mono }}>{value || '$84,192.30'}</div>
              <div style={{ color: colors.emerald, fontSize: 10, fontFamily: typography.mono }}>{trend || '+2.4%'}</div>
            </div>
          </div>
        );

      case 'ARBITER':
        return (
          <div style={{ padding: '12px 16px' }}>
             <div style={{ color: colors.acidGreen, fontSize: 10, fontFamily: typography.mono, textTransform: 'uppercase', marginBottom: 4 }}>ARBITER</div>
             <div style={{ color: colors.textPrimary, fontSize: 14, fontFamily: typography.sans, fontWeight: 600, marginBottom: 8 }}>{label}</div>
             <div style={{ color: colors.textDim, fontSize: 9, fontFamily: typography.mono, textTransform: 'uppercase', marginBottom: 4 }}>ROUTING TO:</div>
             <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap' }}>
                {['Technical', 'Momentum', 'Sentiment'].map(a => (
                  <div key={a} style={{ fontSize: 9, padding: '2px 6px', border: `1px solid ${colors.border}`, borderRadius: 2, color: colors.textMuted }}>{a}</div>
                ))}
             </div>
          </div>
        );

      case 'ANALYST':
        return (
          <CornerBrackets color={analystColor} active={isActive}>
            <div style={{ padding: '8px 4px', minWidth: 160 }}>
              <div style={{ color: analystColor, fontSize: 10, fontFamily: typography.mono, textTransform: 'uppercase', marginBottom: 2 }}>ANALYST</div>
              <div style={{ color: colors.textPrimary, fontSize: 13, fontFamily: typography.sans, fontWeight: 500, marginBottom: 8 }}>{label.replace('Analyst', '').trim()}</div>
              <div style={{ color: colors.emerald, fontSize: 16, fontFamily: typography.mono, fontWeight: 700, letterSpacing: '0.05em' }}>{status || 'LONG'}</div>
              <div style={{ display: 'flex', justifyContent: 'space-between', marginTop: 4 }}>
                <span style={{ color: colors.textDim, fontSize: 9 }}>CONFIDENCE</span>
                <span style={{ color: colors.textMuted, fontSize: 9 }}>87%</span>
              </div>
            </div>
          </CornerBrackets>
        );

      case 'SYNTHESIS':
        return (
          <div style={{ padding: '12px 16px' }}>
            <div style={{ color: colors.amber, fontSize: 10, fontFamily: typography.mono, textTransform: 'uppercase', marginBottom: 4 }}>SYNTHESIS</div>
            <div style={{ color: colors.textPrimary, fontSize: 13, fontFamily: typography.sans, fontWeight: 500, marginBottom: 8 }}>Ensemble</div>
            <div style={{ width: '100%', height: 40, display: 'flex', flexDirection: 'column', gap: 4, justifyContent: 'center' }}>
               <div style={{ width: '80%', height: 4, background: colors.indigo, borderRadius: 2 }} />
               <div style={{ width: '60%', height: 4, background: colors.amber, borderRadius: 2 }} />
               <div style={{ width: '45%', height: 4, background: colors.violet, borderRadius: 2 }} />
            </div>
            <div style={{ color: colors.emerald, fontSize: 16, fontFamily: typography.mono, fontWeight: 700, marginTop: 4 }}>LONG 0.81</div>
          </div>
        );

      case 'EXECUTION':
        return (
          <div style={{ padding: '12px 16px' }}>
            <div style={{ color: colors.emerald, fontSize: 10, fontFamily: typography.mono, textTransform: 'uppercase', marginBottom: 4 }}>EXECUTION</div>
            <div style={{ color: colors.textPrimary, fontSize: 13, fontFamily: typography.sans, fontWeight: 500, marginBottom: 8 }}>Live Router</div>
            <div style={{ background: `${colors.emerald}22`, color: colors.emerald, fontSize: 10, padding: '2px 8px', borderRadius: 4, display: 'inline-block', marginBottom: 8 }}>LONG</div>
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 4, fontSize: 9, color: colors.textDim }}>
               <span>SIZE</span><span style={{ color: colors.textPrimary }}>$12,500</span>
               <span>ENTRY</span><span style={{ color: colors.textPrimary }}>$83,261.50</span>
            </div>
          </div>
        );

      default:
        return (
          <div style={{ padding: '12px 16px' }}>
            <div style={{ color: colors.textDim, fontSize: 10, fontFamily: typography.mono, textTransform: 'uppercase' }}>{nodeState.kind}</div>
            <div style={{ color: colors.textPrimary, fontSize: 14, fontFamily: typography.sans, fontWeight: 600 }}>{label}</div>
          </div>
        );
    }
  };

  const showLeftStrip = role === 'SIGNAL_SOURCE' || role === 'ARBITER' || role === 'SYNTHESIS';
  const stripColor = role === 'SIGNAL_SOURCE' ? colors.slate : role === 'ARBITER' ? colors.acidGreen : colors.amber;

  return (
    <motion.div
      whileHover={{ scale: 1.02 }}
      transition={{ duration: 0.2 }}
      style={{ cursor: 'pointer', position: 'relative' }}
    >
      <Handle 
        type="target" 
        position={Position.Left} 
        style={{ 
          background: colors.border, 
          width: 8, height: 8, 
          border: `2px solid ${colors.canvas}`,
          zIndex: 10 
        }} 
      />

      <ChamferedBox 
        isActive={isActive}
        backgroundColor={colors.cardBg}
        borderColor={isActive ? colors.acidGreen : colors.border}
        style={{ minWidth: role === 'ARBITER' ? 280 : 220 }}
      >
        <div style={{ display: 'flex', position: 'relative' }}>
          {showLeftStrip && (
            <div style={{ width: 4, backgroundColor: stripColor, flexShrink: 0 }} />
          )}
          <div style={{ flexGrow: 1 }}>
            {renderContent()}
          </div>
        </div>
      </ChamferedBox>

      <Handle 
        type="source" 
        position={Position.Right} 
        style={{ 
          background: colors.border, 
          width: 8, height: 8, 
          border: `2px solid ${colors.canvas}`,
          zIndex: 10 
        }} 
      />
      
      {/* Active pulse */}
      {isActive && (
        <motion.div
          animate={{ opacity: [1, 0.4, 1] }}
          transition={{ duration: 1.5, repeat: Infinity }}
          style={{
            position: 'absolute', top: 4, right: 4,
            width: 4, height: 4, borderRadius: '50%',
            background: colors.acidGreen,
            boxShadow: `0 0 6px ${colors.acidGreen}`,
            zIndex: 20
          }}
        />
      )}
    </motion.div>
  );
});
