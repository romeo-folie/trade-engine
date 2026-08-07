import assert from 'node:assert/strict';
import test from 'node:test';

import { buildBacktestEquityCurve } from '../src/components/backtests/backtestCurve.ts';
import { paddedEquityDomain } from '../src/components/portfolio/equityDomain.ts';

test('paddedEquityDomain fits a narrow portfolio range instead of anchoring at zero', () => {
  assert.deepEqual(paddedEquityDomain([99_000, 103_000]), [98_680, 103_320]);
});

test('paddedEquityDomain pads flat data and ignores non-finite values', () => {
  assert.deepEqual(paddedEquityDomain([Number.NaN, 100_000]), [99_000, 101_000]);
  assert.equal(paddedEquityDomain([]), undefined);
});

test('buildBacktestEquityCurve uses run start and realizes overlapping trades by exit', () => {
  const curve = buildBacktestEquityCurve([
    {
      EntryTS: '2026-01-10T14:30:00Z',
      ExitTS: '2026-03-10T15:00:00Z',
      PnL: 1_000,
    },
    {
      EntryTS: '2026-02-10T14:30:00Z',
      ExitTS: '2026-02-11T15:00:00Z',
      PnL: -500,
    },
    {
      EntryTS: '2026-02-12T14:30:00Z',
      ExitTS: null,
      PnL: null,
    },
  ], 100_000, '2026-01-01T00:00:00Z');

  assert.deepEqual(curve, [
    { date: '2026-01-01', value: 100_000 },
    { date: '2026-02-11', value: 99_500 },
    { date: '2026-02-12', value: 99_500 },
    { date: '2026-03-10', value: 100_500 },
  ]);
});
