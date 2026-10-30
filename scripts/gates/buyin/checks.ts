// Gate (b) ordering checks. Pure: no wallet, no network, no DOM.
//
// Spec 10.5: a buy-in spends the table escrow (a foreign input) next to the player's own coins, and the
// escrow's covenant pins the first outputs by position (a prefix compare). So the wallet must keep the
// foreign input at index 0 and the pinned outputs first and in order. These checks read the final
// transaction the wallet built and say, fact by fact, whether it did.
import type { Transaction } from '@bsv/sdk';
// One definition of "seen" and "rejected" for gate (a) and gate (b): src/broadcaster.ts owns the lists.
import { SEEN_OR_LATER, TERMINAL_FAILURES } from '../src/broadcaster.js';

/**
 * The continuation's amount. The page's buy-in spends the 1,000-sat escrow plus coins the wallet chooses,
 * pays this continuation and a 1-sat marker, and leaves the rest to fees and change.
 */
export const CONTINUATION_SATS = 2800;

export interface Outpoint { txid: string; vout: number }
export interface PinnedOutput { name: string; lockingScript: string; satoshis: number }
export interface Expected { foreign: Outpoint; pinned: PinnedOutput[] }
export interface Fact { fact: string; pass: boolean; detail: string }

export const FACT_FOREIGN_FIRST = 'foreign input is at index 0';
export const FACT_PINNED_PREFIX = 'pinned outputs are first and in order';
export const FACT_EXTRAS = 'extra outputs are all after the pinned prefix';
export const FACT_CONTINUATION = `continuation carries ${CONTINUATION_SATS} sats`;
export const FACT_SAME_TX = 'the transaction signAction returned is the one that was signed';
export const FACT_NETWORK = 'BSV Broadcaster has seen the transaction, with no competitor';

/** Refuse anything but testnet (G-02). Throws with a message fit to show the person. */
export function requireTestnet(network: string): void {
  if (network !== 'testnet') {
    throw new Error(`Refusing to run: the wallet reports network "${network}". This gate is testnet only. Switch BSV Desktop to testnet and reload.`);
  }
}

export function inputOutpoint(tx: Transaction, i: number): Outpoint | undefined {
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

  // 3. Extra outputs. The covenant ignores trailing outputs (wallet change, a commission), so every output
  // beyond the pinned prefix is listed as an extra, even one that repeats a pinned script. What fails is an
  // output with an unpinned script sitting inside the prefix: pinned outputs must come first.
  const extras = outs.filter((o) => o.index >= expected.pinned.length);
  const misplaced = outs.filter((o) => o.index < expected.pinned.length && nameOf(o.script) === undefined);
  const list = (os: typeof outs) => os.map((o) => `#${o.index} ${sats(o.satoshis)} to ${short(o.script)}`).join('; ');
  const extrasFact: Fact = {
    fact: FACT_EXTRAS,
    pass: misplaced.length === 0,
    detail:
      extras.length === 0 && misplaced.length === 0
        ? 'none (no change output)'
        : [
            extras.length ? `${extras.length} after the prefix: ${list(extras)}` : '',
            misplaced.length ? `inside the pinned prefix: ${list(misplaced)}` : '',
          ].filter(Boolean).join('; '),
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

/**
 * The transaction the wallet built and we signed over must be the one signAction returns: same version,
 * lock time, input outpoints and sequences, and output scripts and amounts. (Unlocking scripts are not
 * compared: ours is filled in by signAction.) A difference means our signature does not cover what was
 * broadcast.
 */
export function sameSignedTx(signed: Transaction, final: Transaction | undefined): Fact {
  if (!final) return { fact: FACT_SAME_TX, pass: false, detail: 'signAction returned no transaction, so it cannot be compared' };
  const diffs: string[] = [];
  if (signed.version !== final.version) diffs.push(`version ${signed.version} -> ${final.version}`);
  if (signed.lockTime !== final.lockTime) diffs.push(`lock time ${signed.lockTime} -> ${final.lockTime}`);
  if (signed.inputs.length !== final.inputs.length) diffs.push(`${signed.inputs.length} inputs -> ${final.inputs.length}`);
  else {
    signed.inputs.forEach((_, i) => {
      const a = inputOutpoint(signed, i);
      const b = inputOutpoint(final, i);
      if (!a || !b || a.txid !== b.txid || a.vout !== b.vout) diffs.push(`input ${i} outpoint ${a ? `${short(a.txid)}:${a.vout}` : '?'} -> ${b ? `${short(b.txid)}:${b.vout}` : '?'}`);
      if (signed.inputs[i]!.sequence !== final.inputs[i]!.sequence) diffs.push(`input ${i} sequence ${signed.inputs[i]!.sequence} -> ${final.inputs[i]!.sequence}`);
    });
  }
  if (signed.outputs.length !== final.outputs.length) diffs.push(`${signed.outputs.length} outputs -> ${final.outputs.length}`);
  else {
    signed.outputs.forEach((o, i) => {
      const f = final.outputs[i]!;
      if (o.lockingScript.toHex() !== f.lockingScript.toHex()) diffs.push(`output ${i} script changed`);
      if (o.satoshis !== f.satoshis) diffs.push(`output ${i} amount ${o.satoshis} -> ${f.satoshis}`);
    });
  }
  return {
    fact: FACT_SAME_TX,
    pass: diffs.length === 0,
    detail: diffs.length === 0 ? `${final.inputs.length} inputs and ${final.outputs.length} outputs are unchanged between the signed and returned transaction` : diffs.join('; '),
  };
}

/** What `GET /tx/{txid}` returns that matters here (BSV Broadcaster / Arcade). */
export interface BroadcasterStatus { txStatus?: string; competingTxs?: string[] }

/** Statuses after which polling can stop. Anything else (404, UNKNOWN, QUEUED, ...) means "not yet". */
export const TERMINAL_STATUSES = [...SEEN_OR_LATER, ...TERMINAL_FAILURES];
export const isTerminal = (status: BroadcasterStatus | undefined) => status?.txStatus !== undefined && TERMINAL_STATUSES.includes(status.txStatus);

/** Passes only at a seen-or-later status (SEEN_OR_LATER) with no competing transaction (G-09, G-10). */
export function networkFact(status: BroadcasterStatus | undefined): Fact {
  if (!status || status.txStatus === undefined) {
    return { fact: FACT_NETWORK, pass: false, detail: 'no status from BSV Broadcaster (the transaction was not found, or the request failed)' };
  }
  const competing = status.competingTxs ?? [];
  const seen = SEEN_OR_LATER.includes(status.txStatus);
  const parts = [`status ${status.txStatus}`];
  if (competing.length) parts.push(`competing transactions: ${competing.join(', ')}`);
  else if (seen) parts.push('no competing transactions');
  return { fact: FACT_NETWORK, pass: seen && competing.length === 0, detail: parts.join('; ') };
}
