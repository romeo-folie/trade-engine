import { NavLink } from 'react-router-dom';
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
import styles from './Sidebar.module.css';

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

const ICON_SIZE = 16;

export default function Sidebar() {
  return (
    <nav className={styles.sidebar}>
      {NAV_ITEMS.map(({ label, icon: Icon, path }) => (
        <NavLink
          key={path}
          to={path}
          className={({ isActive }) =>
            `${styles.navItem}${isActive ? ` ${styles.active}` : ''}`
          }
        >
          <span className={styles.icon}>
            <Icon size={ICON_SIZE} />
          </span>
          <span className={styles.label}>{label}</span>
        </NavLink>
      ))}
    </nav>
  );
}
