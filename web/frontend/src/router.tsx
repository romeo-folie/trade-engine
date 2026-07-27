import { Suspense } from 'react';
import type { CSSProperties, ReactNode } from 'react';
import { createBrowserRouter, Navigate } from 'react-router-dom';
import Shell from './components/shell/Shell';
import RouteErrorBoundary from './components/RouteErrorBoundary';
import { lazyWithReload } from './utils/lazyWithReload';

// Route-level code splitting: each screen is its own chunk and is fetched only
// when the user navigates to it. Before this change, the main bundle eagerly
// included every screen — including heavy deps like @xyflow/react (Pipeline)
// and recharts (Portfolio) — even though the user only sees one screen at a time.
//
// lazyWithReload rather than React.lazy: the Go binary embeds these chunks, so a
// tab open across an engine restart asks for content hashes the new build no
// longer serves. React.lazy surfaced that as an unrecoverable route error.
const PipelineScreen    = lazyWithReload('Pipeline',    () => import('./screens/Pipeline'));
const PortfolioScreen   = lazyWithReload('Portfolio',   () => import('./screens/Portfolio'));
const SignalsScreen     = lazyWithReload('Signals',     () => import('./screens/Signals'));
const TradesScreen      = lazyWithReload('Trades',      () => import('./screens/Trades'));
const AnalysisScreen    = lazyWithReload('Analysis',    () => import('./screens/Analysis'));
const PerformanceScreen = lazyWithReload('Performance', () => import('./screens/Performance'));
const BacktestsScreen   = lazyWithReload('Backtests',   () => import('./screens/Backtests'));
const ConfigScreen      = lazyWithReload('Config',      () => import('./screens/Config'));
const SystemScreen      = lazyWithReload('System',      () => import('./screens/System'));

// Hoisted: rebuilding this object on every render allocates for no benefit.
const fallbackStyle: CSSProperties = { padding: 'var(--space-6)', color: 'var(--text-dim)' };

// Fallback shown while a route chunk is downloading. Intentionally minimal —
// route chunks are small and load fast over the local web server.
function RouteFallback() {
  return <div style={fallbackStyle}>Loading…</div>;
}

function lazyRoute(element: ReactNode): ReactNode {
  return <Suspense fallback={<RouteFallback />}>{element}</Suspense>;
}

// Declared per child route (not only on the parent) so a failed screen keeps the
// Shell — sidebar, top bar, connection status — mounted around the error.
const routeError = <RouteErrorBoundary />;

export const router = createBrowserRouter([
  {
    path: '/',
    element: <Shell />,
    errorElement: routeError,
    children: [
      { index: true, element: <Navigate to="/pipeline" replace /> },
      { path: 'pipeline',    element: lazyRoute(<PipelineScreen />),    errorElement: routeError },
      { path: 'portfolio',   element: lazyRoute(<PortfolioScreen />),   errorElement: routeError },
      { path: 'signals',     element: lazyRoute(<SignalsScreen />),     errorElement: routeError },
      { path: 'trades',      element: lazyRoute(<TradesScreen />),      errorElement: routeError },
      { path: 'analysis',    element: lazyRoute(<AnalysisScreen />),    errorElement: routeError },
      { path: 'performance', element: lazyRoute(<PerformanceScreen />), errorElement: routeError },
      { path: 'backtests',   element: lazyRoute(<BacktestsScreen />),   errorElement: routeError },
      { path: 'config',      element: lazyRoute(<ConfigScreen />),      errorElement: routeError },
      { path: 'system',      element: lazyRoute(<SystemScreen />),      errorElement: routeError },
      { path: '*',           element: <Navigate to="/pipeline" replace /> },
    ],
  },
]);
