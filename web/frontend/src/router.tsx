import { createBrowserRouter, Navigate } from 'react-router-dom';
import PipelineScreen   from './screens/Pipeline';
import PortfolioScreen  from './screens/Portfolio';
import SignalsScreen    from './screens/Signals';
import TradesScreen     from './screens/Trades';
import AnalysisScreen   from './screens/Analysis';
import BacktestsScreen  from './screens/Backtests';
import ConfigScreen     from './screens/Config';
import SystemScreen     from './screens/System';

export const router = createBrowserRouter([
  { path: '/',           element: <Navigate to="/pipeline" replace /> },
  { path: '/pipeline',   element: <PipelineScreen /> },
  { path: '/portfolio',  element: <PortfolioScreen /> },
  { path: '/signals',    element: <SignalsScreen /> },
  { path: '/trades',     element: <TradesScreen /> },
  { path: '/analysis',   element: <AnalysisScreen /> },
  { path: '/backtests',  element: <BacktestsScreen /> },
  { path: '/config',     element: <ConfigScreen /> },
  { path: '/system',     element: <SystemScreen /> },
  { path: '*',           element: <Navigate to="/pipeline" replace /> },
]);
