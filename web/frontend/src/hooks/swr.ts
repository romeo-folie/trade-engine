/**
 * SWR fetcher helpers used across the app.
 *
 * SWR replaces the older useVisiblePoll-based pattern at every site that
 * consumes a JSON REST endpoint. The wins versus a raw setInterval/useEffect:
 *
 *   - Cross-screen cache: navigating to another route and back doesn't
 *     refetch within the deduping window (default 2s). Old approach forced a
 *     fresh fetch on every mount.
 *   - Dedup: multiple components asking for the same URL only fire one
 *     request.
 *   - Built-in visibility-aware revalidation: pauses when document.hidden,
 *     fires on focus return.
 *   - AbortController is managed for us — no need for the manual
 *     fetchControllerRef pattern.
 *
 * The fetcher throws on non-2xx so SWR's `error` state populates correctly.
 */

export class FetchError extends Error {
  constructor(public status: number, public statusText: string, message?: string) {
    super(message ?? `${status} ${statusText}`);
    this.name = 'FetchError';
  }
}

export async function jsonFetcher<T = unknown>(url: string): Promise<T> {
  const res = await fetch(url);
  if (!res.ok) {
    throw new FetchError(res.status, res.statusText);
  }
  return res.json() as Promise<T>;
}
