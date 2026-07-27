/**
 * RouteErrorBoundary — errorElement for every route.
 *
 * Without an errorElement, React Router falls back to its built-in boundary,
 * which renders a bare "Unexpected Application Error!" plus a raw stack trace
 * and offers no way forward. The most common cause here is a chunk load failure
 * after the engine restarted with a fresh build (see utils/lazyWithReload), so
 * the recovery action that matters is a reload.
 */
import type { CSSProperties } from 'react';
import { isRouteErrorResponse, useRouteError } from 'react-router-dom';
import { colors } from '../styles/tokens';

/** Vite/browser wording for a route chunk that is no longer on the server. */
const CHUNK_ERROR_RE = /dynamically imported module|Importing a module script failed|ChunkLoadError/i;

function errorMessage(error: unknown): string {
  if (isRouteErrorResponse(error)) return `${error.status} ${error.statusText}`;
  if (error instanceof Error) return error.message;
  if (typeof error === 'string') return error;
  return 'Unknown error';
}

function reload() {
  window.location.reload();
}

export default function RouteErrorBoundary() {
  const error = useRouteError();
  const message = errorMessage(error);
  const isStaleChunk = CHUNK_ERROR_RE.test(message);

  return (
    <div style={styles.wrap} role="alert">
      <div style={styles.title}>{isStaleChunk ? 'This tab is running an old build' : 'Screen failed to load'}</div>
      <p style={styles.body}>
        {isStaleChunk
          ? 'The engine restarted with a newer frontend, so this page’s assets are no longer on the server. Reloading picks up the current build.'
          : 'Something went wrong while rendering this screen.'}
      </p>
      <pre style={styles.detail}>{message}</pre>
      <button type="button" onClick={reload} style={styles.button}>
        Reload
      </button>
    </div>
  );
}

// Hoisted so the style objects are not rebuilt on every render.
const styles: Record<string, CSSProperties> = {
  wrap: {
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'flex-start',
    gap: 'var(--space-3)',
    padding: 'var(--space-6)',
    fontFamily: '"JetBrains Mono", monospace',
    color: colors.textMuted,
    maxWidth: 720,
  },
  title: {
    color: colors.red,
    fontSize: 13,
    fontWeight: 600,
    letterSpacing: '0.04em',
    textTransform: 'uppercase',
  },
  body: {
    fontSize: 12,
    lineHeight: 1.6,
    color: colors.textMuted,
  },
  detail: {
    fontSize: 11,
    lineHeight: 1.5,
    color: colors.textDim,
    background: colors.cardBg,
    border: `1px solid ${colors.borderMuted}`,
    borderRadius: 4,
    padding: '0.5rem 0.75rem',
    whiteSpace: 'pre-wrap',
    wordBreak: 'break-word',
    maxWidth: '100%',
    overflowX: 'auto',
  },
  button: {
    fontFamily: 'inherit',
    fontSize: 11,
    letterSpacing: '0.04em',
    textTransform: 'uppercase',
    color: colors.acidGreen,
    background: 'transparent',
    border: `1px solid ${colors.acidGreen}`,
    borderRadius: 4,
    padding: '0.4rem 0.9rem',
    cursor: 'pointer',
  },
};
