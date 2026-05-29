import { useEffect, useRef } from 'react';

/**
 * Polls `callback` every `intervalMs` while the tab is visible. Pauses when the
 * tab is hidden (Page Visibility API) and re-fires the callback immediately on
 * visibility return so the user sees fresh data the moment they refocus.
 *
 * The callback is captured in a ref so consumers don't need to memoize it —
 * the effect's only dependency is `intervalMs`, which means filter / state
 * changes in the consumer don't tear the interval down and recreate it.
 *
 * Use alongside the consumer's existing initial-fetch useEffect:
 *
 *   useEffect(() => { load(); }, []);            // fire once on mount
 *   useVisiblePoll(load, 30_000);                // poll while visible
 */
export function useVisiblePoll(callback: () => void, intervalMs: number): void {
  const cbRef = useRef(callback);
  cbRef.current = callback;

  useEffect(() => {
    let timer: ReturnType<typeof setInterval> | null = null;

    function start() {
      if (timer === null) timer = setInterval(() => cbRef.current(), intervalMs);
    }
    function stop() {
      if (timer !== null) {
        clearInterval(timer);
        timer = null;
      }
    }
    function onVisibility() {
      if (document.hidden) {
        stop();
      } else {
        // Re-fire immediately on return so the visible UI is up to date, then
        // resume the regular cadence.
        cbRef.current();
        start();
      }
    }

    if (!document.hidden) start();
    document.addEventListener('visibilitychange', onVisibility);
    return () => {
      stop();
      document.removeEventListener('visibilitychange', onVisibility);
    };
  }, [intervalMs]);
}
