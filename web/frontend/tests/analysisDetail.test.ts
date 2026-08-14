import assert from 'node:assert/strict';
import test from 'node:test';

import {
  formatIndicator,
  shouldShowEntryChecks,
  titleCase,
} from '../src/components/analysis/analysisDetailFormat.ts';
import { INDICATOR_NAMES } from '../src/components/pipeline/indicatorCatalog.ts';
import { resetConfigPreservingStrategies } from '../src/components/settings/configReset.ts';

test('indicator catalog lists only technical indicator families', () => {
  assert.deepEqual(INDICATOR_NAMES, [
    'EMA', 'SMA', 'RSI', 'MACD', 'Bollinger Bands', 'ADX',
    'ATR', 'OBV', 'Stochastic', 'VWAP', 'Anchored VWAP', 'Volume SMA',
  ]);
  assert.equal(INDICATOR_NAMES.includes('Support' as never), false);
  assert.equal(INDICATOR_NAMES.includes('Consolidation' as never), false);
});

test('analysis detail formatting distinguishes unavailable indicator values', () => {
  assert.equal(formatIndicator(undefined), 'Unavailable');
  assert.equal(formatIndicator(0), 'Unavailable');
  assert.equal(formatIndicator(61.257), '61.26');
  assert.equal(formatIndicator(1234, 0), '1,234');
  assert.equal(titleCase('pullback_strength'), 'Pullback Strength');
});

test('reset keeps read-only strategy statuses from the loaded config', () => {
  const reset = resetConfigPreservingStrategies(
    {
      mode: 'paper',
      strategies: {
        trendFollowing: true,
        meanReversion: true,
        momentum: true,
        newsSentiment: false,
        pullbackStrength: false,
      },
    },
    {
      mode: 'auto',
      strategies: {
        trendFollowing: false,
        meanReversion: false,
        momentum: false,
        newsSentiment: true,
        pullbackStrength: true,
      },
    },
  );

  assert.deepEqual(reset.strategies, {
    trendFollowing: false,
    meanReversion: false,
    momentum: false,
    newsSentiment: true,
    pullbackStrength: true,
  });
  assert.equal(reset.mode, 'paper');
});

test('entry checks appear only for BUY analyses that have decisions', () => {
  assert.equal(shouldShowEntryChecks('BUY', 2), true);
  assert.equal(shouldShowEntryChecks('HOLD', 2), false);
  assert.equal(shouldShowEntryChecks('SELL', 2), false);
  assert.equal(shouldShowEntryChecks('BUY', 0), false);
});
