import { test } from 'node:test';
import assert from 'node:assert/strict';
import { verdictLabel, verdictColor } from '../src/utils/verdict.ts';

// #599: SELL exits a long and must never be shown as SHORT; SHORT and COVER
// are their own actions.
test('verdictLabel keeps SELL, SHORT and COVER distinct', () => {
  assert.equal(verdictLabel('BUY'), 'LONG');
  assert.equal(verdictLabel('LONG'), 'LONG');
  assert.equal(verdictLabel('SELL'), 'SELL');
  assert.equal(verdictLabel('SHORT'), 'SHORT');
  assert.equal(verdictLabel('COVER'), 'COVER');
  assert.equal(verdictLabel('cover'), 'COVER');
  assert.equal(verdictLabel(''), 'NEUTRAL');
});

test('verdictColor styles buys and sells by side', () => {
  assert.equal(verdictColor('SHORT'), 'var(--red)');
  assert.equal(verdictColor('SELL'), 'var(--red)');
  assert.equal(verdictColor('COVER'), 'var(--accent)');
  assert.equal(verdictColor('BUY'), 'var(--accent)');
});
