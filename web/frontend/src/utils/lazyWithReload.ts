/**
 * lazyWithReload — React.lazy that survives a backend redeploy.
 *
 * The Go binary embeds the built frontend, so restarting the engine (a rebuild,
 * or a te-switch.sh session change) swaps every content-hashed asset in one go.
 * A tab that was open across that restart still holds the previous index.html,
 * so navigating to a route it had not yet visited requests a chunk the new
 * server does not have:
 *
 *   GET /assets/Config-Tp3DUHQ_.js  →  404
 *   TypeError: Failed to fetch dynamically imported module
 *
 * The server is right to 404 (spaFallback deliberately refuses to answer
 * extension-bearing paths with index.html), and the chunk is genuinely gone —
 * the only real recovery is to reload and pick up the current index.html.
 *
 * Rather than reload on any import failure, confirm the deployment actually
 * changed by comparing the served index.html's entry script against the one
 * that booted this document. A transient network error then still surfaces as
 * a real error instead of an unexplained page refresh.
 *
 * A sessionStorage flag bounds this to one reload per chunk per tab session, so
 * a server that is broken in some other way cannot induce a reload loop.
 */
import { lazy } from 'react';
import type { ComponentType, LazyExoticComponent } from 'react';

const RELOAD_FLAG_PREFIX = 'te:chunk-reload:';

/** Matches the module entry script Vite injects into index.html. */
const ENTRY_SCRIPT_RE = /<script[^>]+type="module"[^>]+src="([^"]+)"/i;

function readReloadFlag(key: string): boolean {
  try {
    return sessionStorage.getItem(RELOAD_FLAG_PREFIX + key) !== null;
  } catch {
    // Storage unavailable (private mode / blocked cookies). Treat as "already
    // reloaded" so we never risk an unbounded reload loop.
    return true;
  }
}

function writeReloadFlag(key: string): void {
  try {
    sessionStorage.setItem(RELOAD_FLAG_PREFIX + key, '1');
  } catch {
    /* best effort */
  }
}

function clearReloadFlag(key: string): void {
  try {
    sessionStorage.removeItem(RELOAD_FLAG_PREFIX + key);
  } catch {
    /* best effort */
  }
}

/**
 * Returns true when the server is serving a different build than the one this
 * document booted from. Any uncertainty resolves to false so that unrelated
 * failures keep their original error.
 */
export async function isStaleDeployment(): Promise<boolean> {
  const loadedEntry = document
    .querySelector<HTMLScriptElement>('script[type="module"][src]')
    ?.getAttribute('src');
  if (!loadedEntry) return false;

  try {
    const res = await fetch('/index.html', { cache: 'no-store' });
    if (!res.ok) return false;
    const servedEntry = ENTRY_SCRIPT_RE.exec(await res.text())?.[1];
    return servedEntry != null && servedEntry !== loadedEntry;
  } catch {
    return false;
  }
}

/**
 * Drop-in replacement for React.lazy for route-level code splitting.
 *
 * The `ComponentType<any>` constraint mirrors React.lazy's own signature —
 * narrowing it breaks prop inference for the wrapped component at the call site.
 *
 * @param chunkName Stable identifier used for the one-reload-per-session guard.
 * @param factory   The dynamic import, exactly as passed to React.lazy.
 */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function lazyWithReload<T extends ComponentType<any>>(
  chunkName: string,
  factory: () => Promise<{ default: T }>,
): LazyExoticComponent<T> {
  return lazy(async () => {
    try {
      const mod = await factory();
      // Loading succeeded — re-arm the guard for a future deployment.
      clearReloadFlag(chunkName);
      return mod;
    } catch (err) {
      if ((await isStaleDeployment()) && !readReloadFlag(chunkName)) {
        writeReloadFlag(chunkName);
        window.location.reload();
        // The document is being replaced; never settle so React does not paint
        // an error state during teardown.
        return new Promise<never>(() => {});
      }
      throw err;
    }
  });
}
