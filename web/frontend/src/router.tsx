import { lazy, Suspense } from 'react';
import type { ReactNode } from 'react';
import { createBrowserRouter, Navigate } from 'react-router-dom';
import Shell from './components/shell/Shell';

// Route-level code splitting: each screen is its own chunk and is fetched only
// when the user navigates to it. Before this change, the main bundle eagerly
// included every screen — including heavy deps like @xyflow/react (Pipeline)
// and recharts (Portfolio) — even though the user only sees one screen at a time.
const PipelineScreen  = lazy(() => import('./screens/Pipeline'));
const PortfolioScreen = lazy(() => import('./screens/Portfolio'));
const SignalsScreen   = lazy(() => import('./screens/Signals'));
const TradesScreen    = lazy(() => import('./screens/Trades'));
const AnalysisScreen  = lazy(() => import('./screens/Analysis'));
const BacktestsScreen = lazy(() => import('./screens/Backtests'));
const ConfigScreen    = lazy(() => import('./screens/Config'));
const SystemScreen    = lazy(() => import('./screens/System'));

// Fallback shown while a route chunk is downloading. Intentionally minimal —
// route chunks are small and load fast over the local web server.
function RouteFallback() {
  return <div style={{ padding: 'var(--space-6)', color: 'var(--text-dim)' }}>Loading…</div>;
}

function lazyRoute(element: ReactNode): ReactNode {
  return <Suspense fallback={<RouteFallback />}>{element}</Suspense>;
}

export const router = createBrowserRouter([
  {
    path: '/',
    element: <Shell />,
    children: [
      { index: true, element: <Navigate to="/pipeline" replace /> },
      { path: 'pipeline',  element: lazyRoute(<PipelineScreen />) },
      { path: 'portfolio', element: lazyRoute(<PortfolioScreen />) },
      { path: 'signals',   element: lazyRoute(<SignalsScreen />) },
      { path: 'trades',    element: lazyRoute(<TradesScreen />) },
      { path: 'analysis',  element: lazyRoute(<AnalysisScreen />) },
      { path: 'backtests', element: lazyRoute(<BacktestsScreen />) },
      { path: 'config',    element: lazyRoute(<ConfigScreen />) },
      { path: 'system',    element: lazyRoute(<SystemScreen />) },
      { path: '*',         element: <Navigate to="/pipeline" replace /> },
    ],
  },
]);
