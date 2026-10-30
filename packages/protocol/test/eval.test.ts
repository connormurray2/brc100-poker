import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { bestScore } from '../src/eval.js';

interface Vector { cards: number[]; category: number; score: number; }

// Written by `go run ./scripts/gates/evalvectors -n 2000 -seed 1` (see scripts/gates/evalvectors/main.go).
const vectors: Vector[] = JSON.parse(
  readFileSync(new URL('./fixtures/eval-vectors.json', import.meta.url), 'utf8'),
);

const sign = (n: number) => Math.sign(n);
const categoryOf = (score: number) => Math.floor(score / 16 ** 5);

describe('bestScore', () => {
  it('agrees with the Go evaluator', () => {
    expect(vectors.length).toBeGreaterThanOrEqual(2000);
    for (const v of vectors) {
      expect(categoryOf(bestScore(v.cards)), `category of ${v.cards}`).toBe(v.category);
    }
    // Consecutive pairs: the ordering of two hands must match the Go ordering (including ties).
    for (let i = 1; i < vectors.length; i++) {
      const a = vectors[i - 1], b = vectors[i];
      if (a === undefined || b === undefined) throw new Error(`no vector at ${i}`);
      expect(
        sign(bestScore(a.cards) - bestScore(b.cards)),
        `pair ${i - 1}/${i}: ${a.cards} vs ${b.cards}`,
      ).toBe(sign(a.score - b.score));
    }
  });

  it('orders every pair within a category the same way Go does', () => {
    const byCat = new Map<number, Vector[]>();
    for (const v of vectors) byCat.set(v.category, [...(byCat.get(v.category) ?? []), v]);
    let pairs = 0;
    for (const bucket of byCat.values()) {
      const mine = bucket.map(v => bestScore(v.cards));
      for (let i = 0; i < bucket.length; i++) {
        for (let j = i + 1; j < bucket.length; j++) {
          pairs++;
          const mi = mine[i], mj = mine[j], bi = bucket[i], bj = bucket[j];
          if (mi === undefined || mj === undefined || bi === undefined || bj === undefined) {
            throw new Error(`no vector at ${i}/${j}`);
          }
          if (sign(mi - mj) !== sign(bi.score - bj.score)) {
            throw new Error(`disagree on ${bi.cards} vs ${bj.cards}`);
          }
        }
      }
    }
    expect(pairs).toBeGreaterThan(100_000);
  });

  it('covers every category and the Review Focus 4 hands', () => {
    const cats = new Set(vectors.map(v => v.category));
    expect([...cats].sort()).toEqual([0, 1, 2, 3, 4, 5, 6, 7, 8]);
    const has = (cards: number[]) => vectors.some(v => v.cards.join() === cards.join());
    const C = (r: number, s: number) => r * 4 + s;
    expect(has([C(12, 0), C(0, 1), C(1, 2), C(2, 3), C(3, 0), C(7, 1), C(5, 2)])).toBe(true); // wheel A-2-3-4-5
  });
});
