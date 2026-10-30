import { describe, expect, it } from 'vitest';
import { median, percentile, reconcile, summarise } from '../src/latency-stats.js';

describe('reconcile', () => {
  it('accepts an exact match in any order', () => {
    expect(reconcile([0, 1, 2], [2, 0, 1])).toEqual({ lost: [], duplicated: [], unexpected: [], ok: true });
  });
  it('reports lost, duplicated and unexpected numbers', () => {
    const r = reconcile([0, 1, 2, 3, 4], [0, 2, 2, 2, 4, 9]);
    expect(r.lost).toEqual([1, 3]);
    expect(r.duplicated).toEqual([2]);
    expect(r.unexpected).toEqual([9]);
    expect(r.ok).toBe(false);
  });
  it('treats nothing received as everything lost', () => {
    expect(reconcile([5, 6], [])).toMatchObject({ lost: [5, 6], ok: false });
  });
});

describe('percentiles', () => {
  it('median of odd and even counts', () => {
    expect(median([3, 1, 2])).toBe(2);
    expect(median([4, 1, 3, 2])).toBe(2.5);
  });
  it('nearest-rank p95 of 1..100 is 95; of 50 values is the 48th', () => {
    const hundred = Array.from({ length: 100 }, (_, i) => i + 1);
    expect(percentile(hundred, 95)).toBe(95);
    const fifty = Array.from({ length: 50 }, (_, i) => (i + 1) * 10);
    expect(percentile(fifty, 95)).toBe(480);
    expect(percentile(fifty, 100)).toBe(500);
    expect(percentile(fifty, 0)).toBe(10);
  });
  it('does not mutate its input', () => {
    const v = [3, 1, 2];
    summarise(v);
    expect(v).toEqual([3, 1, 2]);
  });
  it('summarises', () => {
    expect(summarise([10, 20, 30, 40])).toEqual({ n: 4, min: 10, median: 25, p95: 40, max: 40, mean: 25 });
  });
  it('rejects empty input', () => {
    expect(() => median([])).toThrow();
    expect(() => summarise([])).toThrow();
  });
});
