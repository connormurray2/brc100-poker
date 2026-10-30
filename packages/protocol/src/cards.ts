import { Hash, Point, Utils } from '@bsv/sdk';
import { PointError, parsePoint, toCompressed } from './points.js';

const DECK_SIZE = 52;

/**
 * Card i is the first `02 ‖ SHA-256("brc100-poker/card/v1/" ‖ i ‖ "/" ‖ t)`, t = 0, 1, ...,
 * that decodes to a canonical point. i and t are ASCII decimal.
 * Card index = (rank - 2) * 4 + suit.
 */
function derive(i: number): Point {
  for (let t = 0; ; t++) {
    const x = Hash.sha256(Utils.toArray(`brc100-poker/card/v1/${i}/${t}`, 'utf8'));
    try {
      return parsePoint(Uint8Array.from([0x02, ...x]));
    } catch (e) {
      if (!(e instanceof PointError)) throw e; // x >= p or no curve point: try the next t
    }
  }
}

export const CARD_POINTS: readonly Point[] = Object.freeze(Array.from({ length: DECK_SIZE }, (_, i) => derive(i)));

const INDEX_BY_POINT = new Map<string, number>(
  CARD_POINTS.map((p, i) => [Utils.toHex(Array.from(toCompressed(p))), i]),
);

/** The hash-to-curve point for card `index` (0..51). */
export function cardPoint(index: number): Point {
  if (!Number.isInteger(index) || index < 0 || index >= DECK_SIZE) {
    throw new RangeError(`card index must be an integer in 0..${DECK_SIZE - 1}, got ${index}`);
  }
  return CARD_POINTS[index]!;
}

/** The card a point encodes, or null if it is not one of the 52 card points. */
export function cardIndexOf(p: Point): number | null {
  if (p.isInfinity()) return null;
  return INDEX_BY_POINT.get(Utils.toHex(Array.from(toCompressed(p)))) ?? null;
}
