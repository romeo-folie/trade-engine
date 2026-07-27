import useSWR from 'swr';
import { jsonFetcher } from './swr';

export interface WatchlistTicker {
  ticker: string;
  strategies: string[];
  timeframes: string[];
}

const EMPTY_WATCHLIST: WatchlistTicker[] = [];

export function useWatchlist(): WatchlistTicker[] {
  const { data } = useSWR<{ tickers: WatchlistTicker[] }>(
    '/api/node/watchlist',
    jsonFetcher,
    { refreshInterval: 30_000 },
  );
  return data?.tickers ?? EMPTY_WATCHLIST;
}
