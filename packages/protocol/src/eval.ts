/**
 * Reference 7-card evaluator in the on-chain contract's encoding (contracts/src/Eval7.runar.ts).
 *
 * Card index i: rank = floor(i / 4) (0 = deuce .. 12 = ace), suit = i % 4. This is `cards.Card.Index()` in the
 * Go module. A score is `category * 16^5 + five tiebreak values` in base 16, where a tiebreak value is
 * rank + 1 (0 = none); higher is better and equal scores are ties. Categories: 0 high card, 1 pair, 2 two pair,
 * 3 trips, 4 straight, 5 flush, 6 full house, 7 quads, 8 straight flush.
 *
 * It is deliberately the dumbest possible algorithm (the best of all 21 five-card subsets) so that it is
 * independent of the contract's straight-line code. The Go evaluator (`internal/game/eval`) is the authority;
 * `test/eval.test.ts` holds this to it.
 */

/** Score exactly five cards. */
export function score5(cs: readonly number[]): number {
  if (cs.length !== 5) throw new RangeError(`score5 needs 5 cards, got ${cs.length}`);
  for (const c of cs) {
    if (!Number.isInteger(c) || c < 0 || c > 51) throw new RangeError(`card index out of range: ${c}`);
  }
  const ranks = cs.map(c => Math.floor(c / 4)).sort((a, b) => b - a);
  const suits = cs.map(c => c % 4);
  const flush = suits.every(s => s === suits[0]);
  const uniq = [...new Set(ranks)];
  let straightHigh = -1;
  if (uniq.length === 5 && ranks[0] - ranks[4] === 4) straightHigh = ranks[0];
  if (uniq.length === 5 && ranks[0] === 12 && ranks[1] === 3) straightHigh = 3; // the wheel, A-2-3-4-5, is five-high
  // Rank groups by count descending, then rank descending: exactly the kicker precedence.
  const groups = uniq.map(r => [r, ranks.filter(x => x === r).length]).sort((a, b) => b[1] - a[1] || b[0] - a[0]);
  let cat: number;
  let tb: number[];
  if (flush && straightHigh >= 0) { cat = 8; tb = [straightHigh]; }
  else if (groups[0][1] === 4) { cat = 7; tb = [groups[0][0], groups[1][0]]; }
  else if (groups[0][1] === 3 && groups[1][1] === 2) { cat = 6; tb = [groups[0][0], groups[1][0]]; }
  else if (flush) { cat = 5; tb = ranks; }
  else if (straightHigh >= 0) { cat = 4; tb = [straightHigh]; }
  else if (groups[0][1] === 3) { cat = 3; tb = groups.map(g => g[0]); }
  else if (groups[0][1] === 2 && groups[1][1] === 2) { cat = 2; tb = groups.map(g => g[0]); }
  else if (groups[0][1] === 2) { cat = 1; tb = groups.map(g => g[0]); }
  else { cat = 0; tb = ranks; }
  tb = tb.map(r => r + 1);
  while (tb.length < 5) tb.push(0);
  return cat * 16 ** 5 + tb[0] * 16 ** 4 + tb[1] * 16 ** 3 + tb[2] * 256 + tb[3] * 16 + tb[4];
}

/** The best five-card score among seven distinct cards (or any five or more). */
export function bestScore(cards: readonly number[]): number {
  const n = cards.length;
  if (n < 5) throw new RangeError(`bestScore needs at least 5 cards, got ${n}`);
  if (new Set(cards).size !== n) throw new RangeError('duplicate card');
  let best = -1;
  const pick: number[] = [];
  const rec = (start: number): void => {
    if (pick.length === 5) {
      best = Math.max(best, score5(pick.map(i => cards[i])));
      return;
    }
    for (let i = start; i < n; i++) {
      pick.push(i);
      rec(i + 1);
      pick.pop();
    }
  };
  rec(0);
  return best;
}
