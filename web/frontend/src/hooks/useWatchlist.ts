import { useState, useEffect, useCallback } from 'react';
import { useVisiblePoll } from './useVisiblePoll';

export interface WatchlistTicker {
  ticker: string;
  strategies: string[];
  timeframes: string[];
}

export function useWatchlist(): WatchlistTicker[] {
  const [tickers, setTickers] = useState<WatchlistTicker[]>([]);

  const fetchWatchlist = useCallback(() => {
    fetch('/api/node/watchlist')
      .then(r => { if (!r.ok) throw new Error(r.statusText); return r.json(); })
      .then(d => setTickers(d.tickers || []))
      .catch(() => {});
  }, []);

  useEffect(() => { fetchWatchlist(); }, [fetchWatchlist]);
  useVisiblePoll(fetchWatchlist, 30_000);

  return tickers;
}
