import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import AppRoot from './AppRoot';
import './styles/tokens.css';
import './styles/global.css';
import './styles/chamfer.css';

const root = document.getElementById('root');
if (!root) throw new Error('Missing #root element');

createRoot(root).render(
  <StrictMode>
    <AppRoot />
  </StrictMode>
);
