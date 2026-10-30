import { describe, expect, it } from 'vitest';
import { Curve, Point, Utils } from '@bsv/sdk';
import { CARD_POINTS, cardIndexOf, cardPoint } from '../src/cards.js';
import { toCompressed } from '../src/points.js';

const G = new Curve().g as Point;
const hex = (b: Uint8Array) => Utils.toHex(Array.from(b));

describe('cardPoint', () => {
  it('reproduces the pinned values (computed 2026-10-30)', () => {
    expect(hex(toCompressed(cardPoint(0)))).toBe('021738215e26042202a099ecc26fe754243449ad80eb1b4fc83831fc77a5ebcc4c'); // t = 5
    expect(hex(toCompressed(cardPoint(1)))).toBe('02ac72bf790d18e083964db15028f69b4016f4e12773dcce0e0a7515eb8944cbbe'); // t = 0
    expect(hex(toCompressed(cardPoint(51)))).toBe('02439e62c5b93bdbf44c8306df4bb3e5745fe46eb7c8bb7f49f2693ff8662a001c'); // t = 0
  });

  it('throws outside 0..51', () => {
    expect(() => cardPoint(-1)).toThrow();
    expect(() => cardPoint(52)).toThrow();
    expect(() => cardPoint(1.5)).toThrow();
    expect(() => cardPoint(NaN)).toThrow();
  });
});

describe('CARD_POINTS', () => {
  it('has 52 pairwise-distinct points', () => {
    expect(CARD_POINTS.length).toBe(52);
    expect(new Set(CARD_POINTS.map((p) => hex(toCompressed(p)))).size).toBe(52);
  });

  it('matches cardPoint', () => {
    CARD_POINTS.forEach((p, i) => expect(p.eq(cardPoint(i))).toBe(true));
  });
});

describe('cardIndexOf', () => {
  it('inverts cardPoint for every card', () => {
    for (let i = 0; i < 52; i++) expect(cardIndexOf(cardPoint(i))).toBe(i);
  });

  it('is null for G and for a masked card', () => {
    expect(cardIndexOf(G)).toBeNull();
    expect(cardIndexOf(cardPoint(3).dbl())).toBeNull();
  });
});
