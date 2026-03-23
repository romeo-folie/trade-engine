import { useRef, useState } from 'react';
import { NavLink } from 'react-router-dom';
import { motion, AnimatePresence } from 'framer-motion';
import {
  Activity,
  TrendingUp,
  Zap,
  BarChart2,
  Brain,
  RefreshCw,
  Settings,
  Server,
} from 'lucide-react';
import type { LucideIcon } from 'lucide-react';
import { colors, typography } from '../../styles/tokens';

interface NavItem {
  label: string;
  icon: LucideIcon;
  path: string;
}

const NAV_ITEMS: NavItem[] = [
  { label: 'Pipeline',  icon: Activity,   path: '/pipeline' },
  { label: 'Portfolio', icon: TrendingUp, path: '/portfolio' },
  { label: 'Signals',   icon: Zap,        path: '/signals' },
  { label: 'Trades',    icon: BarChart2,   path: '/trades' },
  { label: 'Analysis',  icon: Brain,      path: '/analysis' },
  { label: 'Backtests', icon: RefreshCw,  path: '/backtests' },
  { label: 'Config',    icon: Settings,   path: '/config' },
  { label: 'System',    icon: Server,     path: '/system' },
];

const ICON_SIZE = 18;

export default function Sidebar() {
  const navRef = useRef<HTMLElement>(null);
  const [hoveredIndex, setHoveredIndex] = useState<number | null>(null);

  return (
    <nav 
      ref={navRef} 
      tabIndex={-1}
      style={{
        width: 48,
        background: colors.canvas,
        borderRight: '1px solid rgba(255, 255, 255, 0.15)',
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'center',
        padding: '20px 0',
        gap: 12,
        position: 'fixed',
        top: 40,
        bottom: 32,
        left: 0,
        zIndex: 900,
      }}
    >
      {NAV_ITEMS.map(({ label, icon: Icon, path }, i) => (
        <div 
          key={path} 
          style={{ 
            position: 'relative',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            width: '100%',
          }}
          onMouseEnter={() => setHoveredIndex(i)}
          onMouseLeave={() => setHoveredIndex(null)}
        >
          <NavLink
            to={path}
            aria-label={label}
            style={({ isActive }) => ({
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'center',
              width: 36,
              height: 36,
              borderRadius: 4,
              color: isActive ? colors.acidGreen : colors.textDim,
              background: isActive ? `${colors.acidGreen}11` : 'transparent',
              transition: 'all 0.2s ease',
            })}
          >
            <Icon size={ICON_SIZE} />
          </NavLink>

          <AnimatePresence>
            {hoveredIndex === i && (
              <motion.div
                initial={{ opacity: 0, x: -10 }}
                animate={{ opacity: 1, x: 0 }}
                exit={{ opacity: 0, x: -10 }}
                transition={{ duration: 0.15 }}
                style={{
                  position: 'absolute',
                  left: '100%',
                  marginLeft: 12,
                  background: colors.cardBgAlt,
                  border: `1px solid ${colors.border}`,
                  borderRadius: 4,
                  padding: '4px 8px',
                  color: colors.textPrimary,
                  fontSize: 11,
                  fontFamily: typography.mono,
                  whiteSpace: 'nowrap',
                  pointerEvents: 'none',
                  zIndex: 1000,
                  boxShadow: '0 4px 12px rgba(0,0,0,0.5)',
                }}
              >
                {label}
              </motion.div>
            )}
          </AnimatePresence>
        </div>
      ))}
    </nav>
  );
}
