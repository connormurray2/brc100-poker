import { describe, expect, it } from 'vitest';
import { PrivateKey, P2PKH, Transaction, Script, type TransactionInput } from '@bsv/sdk';
import { CONTINUATION_SATS, checkBuyIn, inputOutpoint, isTerminal, networkFact, requireTestnet, sameSignedTx, type PinnedOutput } from '../buyin/checks.js';

const lock = (k: PrivateKey): Script => new P2PKH().lock(k.toPublicKey().toAddress('testnet'));
const keys = { cont: PrivateKey.fromRandom(), marker: PrivateKey.fromRandom(), change: PrivateKey.fromRandom() };
const FOREIGN = { txid: 'aa'.repeat(32), vout: 0 };
const OTHER_TXID = 'bb'.repeat(32);

const pinned: PinnedOutput[] = [
  { name: 'continuation', lockingScript: lock(keys.cont).toHex(), satoshis: CONTINUATION_SATS },
  { name: 'marker', lockingScript: lock(keys.marker).toHex(), satoshis: 1 },
];
const out = {
  cont: (satoshis = CONTINUATION_SATS) => ({ lockingScript: lock(keys.cont), satoshis }),
  marker: () => ({ lockingScript: lock(keys.marker), satoshis: 1 }),
  change: (satoshis = 190) => ({ lockingScript: lock(keys.change), satoshis }),
};
const inp = (txid: string, vout = 0): TransactionInput => ({ sourceTXID: txid, sourceOutputIndex: vout, sequence: 0xffffffff });

function build(inputs: TransactionInput[], outputs: { lockingScript: Script; satoshis: number }[]): Transaction {
  const tx = new Transaction();
  for (const i of inputs) tx.addInput(i);
  for (const o of outputs) tx.addOutput(o);
  return tx;
}
const run = (tx: Transaction) => checkBuyIn(tx, { foreign: FOREIGN, pinned });
const byFact = (r: ReturnType<typeof run>, fact: string) => {
  const f = r.find((x) => x.fact === fact);
  if (!f) throw new Error(`no fact "${fact}" in ${r.map((x) => x.fact).join(' | ')}`);
  return f;
};

describe('checkBuyIn', () => {
  it('passes every fact when the wallet keeps the foreign input and pinned outputs first', () => {
    const r = run(build([inp(FOREIGN.txid), inp(OTHER_TXID)], [out.cont(), out.marker(), out.change()]));
    expect(r.map((x) => x.fact)).toEqual([
      'foreign input is at index 0',
      'pinned outputs are first and in order',
      'extra outputs are all after the pinned prefix',
      `continuation carries ${CONTINUATION_SATS} sats`,
    ]);
    expect(r.every((x) => x.pass)).toBe(true);
    expect(byFact(r, 'extra outputs are all after the pinned prefix').detail).toMatch(/#2.*190 sat/);
  });

  it('passes with no extra outputs and says so', () => {
    const r = run(build([inp(FOREIGN.txid), inp(OTHER_TXID)], [out.cont(), out.marker()]));
    expect(r.every((x) => x.pass)).toBe(true);
    expect(byFact(r, 'extra outputs are all after the pinned prefix').detail).toMatch(/none/i);
  });

  it('fails the prefix fact when the wallet swaps the pinned outputs', () => {
    const r = run(build([inp(FOREIGN.txid), inp(OTHER_TXID)], [out.marker(), out.cont(), out.change()]));
    const f = byFact(r, 'pinned outputs are first and in order');
    expect(f.pass).toBe(false);
    expect(f.detail).toMatch(/output 0.*continuation/);
    expect(byFact(r, 'foreign input is at index 0').pass).toBe(true);
    expect(byFact(r, `continuation carries ${CONTINUATION_SATS} sats`).pass).toBe(true);
  });

  it('fails the foreign-input fact and reports where it went', () => {
    const r = run(build([inp(OTHER_TXID), inp(FOREIGN.txid)], [out.cont(), out.marker()]));
    const f = byFact(r, 'foreign input is at index 0');
    expect(f.pass).toBe(false);
    expect(f.detail).toMatch(/index 1/);
    expect(byFact(r, 'pinned outputs are first and in order').pass).toBe(true);
  });

  it('fails the foreign-input fact when the foreign outpoint is missing entirely', () => {
    const r = run(build([inp(OTHER_TXID)], [out.cont(), out.marker()]));
    const f = byFact(r, 'foreign input is at index 0');
    expect(f.pass).toBe(false);
    expect(f.detail).toMatch(/not (found|present)/i);
  });

  it('requires the vout to match, not just the txid', () => {
    const r = run(build([inp(FOREIGN.txid, 1), inp(OTHER_TXID)], [out.cont(), out.marker()]));
    expect(byFact(r, 'foreign input is at index 0').pass).toBe(false);
  });

  it('fails the extras fact when an extra output sits inside the pinned prefix', () => {
    const r = run(build([inp(FOREIGN.txid)], [out.cont(), out.change(), out.marker()]));
    const f = byFact(r, 'extra outputs are all after the pinned prefix');
    expect(f.pass).toBe(false);
    expect(f.detail).toMatch(/#1/);
    expect(byFact(r, 'pinned outputs are first and in order').pass).toBe(false);
  });

  it('fails the amount fact when the continuation is not 2,800 sats', () => {
    const r = run(build([inp(FOREIGN.txid)], [out.cont(2799), out.marker()]));
    const f = byFact(r, `continuation carries ${CONTINUATION_SATS} sats`);
    expect(f.pass).toBe(false);
    expect(f.detail).toMatch(/2799/);
    expect(byFact(r, 'pinned outputs are first and in order').pass).toBe(false);
  });

  it('fails the amount fact when the continuation output is absent', () => {
    const r = run(build([inp(FOREIGN.txid)], [out.marker(), out.change()]));
    const f = byFact(r, `continuation carries ${CONTINUATION_SATS} sats`);
    expect(f.pass).toBe(false);
    expect(f.detail).toMatch(/not (found|present)/i);
  });

  it('lists an output after the prefix as extra even when it repeats a pinned script', () => {
    const r = run(build([inp(FOREIGN.txid)], [out.cont(), out.marker(), out.marker()]));
    const f = byFact(r, 'extra outputs are all after the pinned prefix');
    expect(f.pass).toBe(true);
    expect(f.detail).toMatch(/1 after the prefix: #2/);
  });

  it('reads the input outpoint from sourceTransaction when sourceTXID is unset', () => {
    // A parent with a (blank) unlocking script so it can be serialised and hashed.
    const parent = build([{ ...inp(OTHER_TXID), unlockingScript: new Script() }], [out.cont(1000)]);
    const tx = new Transaction();
    tx.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, sequence: 0xffffffff });
    tx.addOutput(out.cont());
    tx.addOutput(out.marker());
    const r = checkBuyIn(tx, { foreign: { txid: parent.id('hex'), vout: 0 }, pinned });
    expect(byFact(r, 'foreign input is at index 0').pass).toBe(true);
  });
});

describe('inputOutpoint', () => {
  it('returns undefined past the last input', () => {
    expect(inputOutpoint(build([inp(FOREIGN.txid)], [out.cont()]), 1)).toBeUndefined();
  });
});

describe('sameSignedTx', () => {
  const mk = () => build([inp(FOREIGN.txid), inp(OTHER_TXID)], [out.cont(), out.marker(), out.change()]);

  it('passes for identical transactions, ignoring unlocking scripts', () => {
    const signed = mk();
    const final = mk();
    final.inputs[0]!.unlockingScript = new Script();
    expect(sameSignedTx(signed, final).pass).toBe(true);
  });

  it('fails when an output amount is altered', () => {
    const final = mk();
    final.outputs[2]!.satoshis = 100;
    const f = sameSignedTx(mk(), final);
    expect(f.pass).toBe(false);
    expect(f.detail).toMatch(/output 2 amount 190 -> 100/);
  });

  it('fails when an output script is altered', () => {
    const final = mk();
    final.outputs[1]!.lockingScript = lock(PrivateKey.fromRandom());
    expect(sameSignedTx(mk(), final).detail).toMatch(/output 1 script changed/);
  });

  it('fails when an input sequence is altered', () => {
    const final = mk();
    final.inputs[1]!.sequence = 0;
    const f = sameSignedTx(mk(), final);
    expect(f.pass).toBe(false);
    expect(f.detail).toMatch(/input 1 sequence 4294967295 -> 0/);
  });

  it('fails on an altered input outpoint, version, lock time or output count', () => {
    const a = mk(); a.inputs[0]!.sourceOutputIndex = 3;
    expect(sameSignedTx(mk(), a).detail).toMatch(/input 0 outpoint/);
    const b = mk(); b.version = 2;
    expect(sameSignedTx(mk(), b).detail).toMatch(/version 1 -> 2/);
    const c = mk(); c.lockTime = 500;
    expect(sameSignedTx(mk(), c).detail).toMatch(/lock time 0 -> 500/);
    const d = mk(); d.outputs.pop();
    expect(sameSignedTx(mk(), d).detail).toMatch(/3 outputs -> 2/);
  });

  it('fails when signAction returned no transaction', () => {
    expect(sameSignedTx(mk(), undefined).pass).toBe(false);
  });
});

describe('networkFact', () => {
  it('passes a clean SEEN_ON_NETWORK and a clean MINED', () => {
    expect(networkFact({ txStatus: 'SEEN_ON_NETWORK' }).pass).toBe(true);
    expect(networkFact({ txStatus: 'MINED', competingTxs: [] }).pass).toBe(true);
  });
  it.each(['SEEN_MULTIPLE_NODES', 'STUMP_PROCESSING', 'IMMUTABLE'])('passes a clean %s, the same set gate a accepts', (txStatus) => {
    expect(networkFact({ txStatus }).pass).toBe(true);
    expect(networkFact({ txStatus, competingTxs: [] }).pass).toBe(true);
  });
  it.each(['SEEN_MULTIPLE_NODES', 'STUMP_PROCESSING', 'IMMUTABLE'])('fails %s with a competing transaction', (txStatus) => {
    const f = networkFact({ txStatus, competingTxs: ['dd'.repeat(32)] });
    expect(f.pass).toBe(false);
    expect(f.detail).toContain('dd'.repeat(32));
  });
  it('fails SEEN_ON_NETWORK with competing transactions and names them', () => {
    const f = networkFact({ txStatus: 'SEEN_ON_NETWORK', competingTxs: ['cc'.repeat(32)] });
    expect(f.pass).toBe(false);
    expect(f.detail).toContain('cc'.repeat(32));
  });
  it.each(['REJECTED', 'DOUBLE_SPEND_ATTEMPTED', 'UNKNOWN', 'ANNOUNCED_TO_NETWORK'])('fails %s', (txStatus) => {
    expect(networkFact({ txStatus }).pass).toBe(false);
  });
  it('fails when there is no status at all', () => {
    expect(networkFact(undefined).pass).toBe(false);
    expect(networkFact({}).pass).toBe(false);
  });
  it('treats only final answers as terminal, so 404 and UNKNOWN keep polling', () => {
    expect(isTerminal(undefined)).toBe(false);
    expect(isTerminal({ txStatus: 'UNKNOWN' })).toBe(false);
    expect(isTerminal({ txStatus: 'ANNOUNCED_TO_NETWORK' })).toBe(false);
    for (const s of ['SEEN_ON_NETWORK', 'SEEN_MULTIPLE_NODES', 'STUMP_PROCESSING', 'MINED', 'IMMUTABLE', 'REJECTED', 'DOUBLE_SPEND_ATTEMPTED']) {
      expect(isTerminal({ txStatus: s })).toBe(true);
    }
  });
});

describe('requireTestnet', () => {
  it('accepts testnet', () => { expect(() => requireTestnet('testnet')).not.toThrow(); });
  it('refuses mainnet and anything else with a visible message', () => {
    expect(() => requireTestnet('mainnet')).toThrow(/testnet only/);
    expect(() => requireTestnet('')).toThrow(/Refusing/);
  });
});
