// Gate (b) ordering checks. Pure: no wallet, no network, no DOM.
//
// Spec 10.5: a buy-in spends the table escrow (a foreign input) next to the player's own coins, and the
// escrow's covenant pins the first outputs by position (a prefix compare). So the wallet must keep the
// foreign input at index 0 and the pinned outputs first and in order. These checks read the final
// transaction the wallet built and say, fact by fact, whether it did.
import type { Transaction } from '@bsv/sdk';

/** The continuation output carries the escrow value plus the buy-in: 1,000 + 2,000 - 200 fee allowance. */
export const CONTINUATION_SATS = 2800;

export interface Outpoint { txid: string; vout: number }
export interface PinnedOutput { name: string; lockingScript: string; satoshis: number }
export interface Expected { foreign: Outpoint; pinned: PinnedOutput[] }
export interface Fact { fact: string; pass: boolean; detail: string }

export const FACT_FOREIGN_FIRST = 'foreign input is at index 0';
export const FACT_PINNED_PREFIX = 'pinned outputs are first and in order';
export const FACT_EXTRAS = 'extra outputs are all after the pinned prefix';
export const FACT_CONTINUATION = `continuation carries ${CONTINUATION_SATS} sats`;

function inputOutpoint(tx: Transaction, i: number): Outpoint | undefined {
  const input = tx.inputs[i];
  if (!input) return undefined;
  const txid = input.sourceTXID ?? input.sourceTransaction?.id('hex');
  return txid === undefined ? undefined : { txid, vout: input.sourceOutputIndex };
}

const sameOutpoint = (a: Outpoint | undefined, b: Outpoint) => a !== undefined && a.txid === b.txid && a.vout === b.vout;
const short = (hex: string) => (hex.length > 24 ? `${hex.slice(0, 12)}..${hex.slice(-8)}` : hex);
const sats = (n: number) => `${n} sat${n === 1 ? '' : 's'}`;

export function checkBuyIn(tx: Transaction, expected: Expected): Fact[] {
  const outs = tx.outputs.map((o, index) => ({ index, script: o.lockingScript.toHex(), satoshis: o.satoshis ?? 0 }));
  const nameOf = (script: string) => expected.pinned.find((p) => p.lockingScript === script)?.name;
  const describe = (o: { script: string; satoshis: number }) => `${nameOf(o.script) ?? `an unpinned script ${short(o.script)}`} (${sats(o.satoshis)})`;

  // 1. Foreign input at index 0.
  const foreignAt = tx.inputs.findIndex((_, i) => sameOutpoint(inputOutpoint(tx, i), expected.foreign));
  const foreignFact: Fact = {
    fact: FACT_FOREIGN_FIRST,
    pass: foreignAt === 0,
    detail:
      foreignAt === 0
        ? `${expected.foreign.txid}:${expected.foreign.vout} is input 0 of ${tx.inputs.length}`
        : foreignAt < 0
          ? `foreign outpoint ${expected.foreign.txid}:${expected.foreign.vout} not found among the ${tx.inputs.length} inputs`
          : `foreign outpoint is at index ${foreignAt} of ${tx.inputs.length}, not 0`,
  };

  // 2. Pinned outputs first and in order (script and amount, by position).
  const mismatches: string[] = [];
  expected.pinned.forEach((p, i) => {
    const o = outs[i];
    if (!o) mismatches.push(`output ${i}: expected ${p.name}, but the transaction has only ${outs.length} outputs`);
    else if (o.script !== p.lockingScript) mismatches.push(`output ${i}: expected ${p.name}, found ${describe(o)}`);
    else if (o.satoshis !== p.satoshis) mismatches.push(`output ${i}: expected ${p.name} at ${sats(p.satoshis)}, found ${sats(o.satoshis)}`);
  });
  const prefixFact: Fact = {
    fact: FACT_PINNED_PREFIX,
    pass: mismatches.length === 0,
    detail: mismatches.length === 0 ? `outputs 0..${expected.pinned.length - 1} are ${expected.pinned.map((p) => p.name).join(', ')}` : mismatches.join('; '),
  };

  // 3. Extra outputs: anything that is not one of the pinned scripts. The covenant ignores trailing
  // extras (wallet change, a commission) but a pinned prefix needs them after, never between.
  const extras = outs.filter((o) => nameOf(o.script) === undefined);
  const misplaced = extras.filter((o) => o.index < expected.pinned.length);
  const listing = extras.map((o) => `#${o.index} ${sats(o.satoshis)} to ${short(o.script)}`).join('; ');
  const extrasFact: Fact = {
    fact: FACT_EXTRAS,
    pass: misplaced.length === 0,
    detail:
      extras.length === 0
        ? 'none (no change output)'
        : `${extras.length} extra: ${listing}${misplaced.length ? `; inside the pinned prefix: ${misplaced.map((o) => `#${o.index}`).join(', ')}` : ''}`,
  };

  // 4. The continuation's amount, wherever the wallet put it.
  const cont = expected.pinned.find((p) => p.name === 'continuation');
  const contOut = cont && outs.find((o) => o.script === cont.lockingScript);
  const contFact: Fact = {
    fact: FACT_CONTINUATION,
    pass: contOut !== undefined && contOut.satoshis === CONTINUATION_SATS,
    detail: !contOut
      ? 'continuation output not found in the final transaction'
      : contOut.satoshis === CONTINUATION_SATS
        ? `output ${contOut.index} carries ${sats(contOut.satoshis)}`
        : `output ${contOut.index} carries ${sats(contOut.satoshis)}, expected ${CONTINUATION_SATS}`,
  };

  return [foreignFact, prefixFact, extrasFact, contFact];
}
