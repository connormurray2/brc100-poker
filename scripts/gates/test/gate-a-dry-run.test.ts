// The dry run is gate (a)'s main evidence until the funded run, so its in-process broadcaster must
// refuse what a real one would: a bad script, a lying EF, a double spend, a short fee, a non-final
// lock time. And the run file's wallet must never offer a coin a recorded step already spends.
import { describe, it, expect } from 'vitest';
import { LockingScript, Transaction, UnlockingScript } from '@bsv/sdk';
import { LocalSigner, encodePushData } from 'runar-sdk';
import { createBroadcaster } from '../src/broadcaster.js';
import { FakeBroadcaster } from '../src/gate-a/fake-broadcaster.js';
import { assertOutputs, efSizeOf, lookupIn } from '../src/gate-a/txs.js';
import { p2pkhScript, pkhOf, walletUtxos, type RunFile } from '../src/gate-a/store.js';

const RATE = { satoshis: 1, bytes: 1000 };
const LIMITS = { maxTxSize: 100_000_000, maxScriptSize: 100_000_000 };

async function setup() {
  const signer = new LocalSigner('5a'.repeat(32));
  const pub = await signer.getPublicKey();
  const script = LockingScript.fromHex(p2pkhScript(pkhOf(pub)));
  const funding = new Transaction();
  funding.addInput({ sourceTXID: 'ee'.repeat(32), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex('51'), sequence: 0xffffffff });
  funding.addOutput({ satoshis: 100_000, lockingScript: script });
  const fake = new FakeBroadcaster(1000, {}, RATE, LIMITS);
  fake.seed(funding);
  let t = 0;
  const b = createBroadcaster({ baseUrl: 'https://dry-run.invalid', fetch: fake.fetch, now: () => t, sleep: async (ms) => { t += ms; fake.mine(); } });

  /** A P2PKH spend of funding:0 to `outs` (fee = the rest), signed unless `unsigned`. */
  async function spend(o: { fee: number; lockTime?: number; sequence?: number; tamper?: boolean }) {
    const tx = new Transaction();
    tx.lockTime = o.lockTime ?? 0;
    tx.addInput({ sourceTransaction: funding, sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex(''), sequence: o.sequence ?? 0xffffffff });
    tx.addOutput({ satoshis: 100_000 - o.fee, lockingScript: script });
    let sig = await signer.sign(tx.toHex(), 0, script.toHex(), 100_000);
    if (o.tamper) sig = sig.slice(0, 20) + (sig[20] === '0' ? '1' : '0') + sig.slice(21);
    tx.inputs[0]!.unlockingScript = UnlockingScript.fromHex(encodePushData(sig) + encodePushData(pub));
    return tx;
  }
  return { fake, b, spend, funding, script };
}

describe('gate (a) dry run: the in-process broadcaster', () => {
  it('accepts a valid spend through Spend, reports SEEN, and MINED only after a block', async () => {
    const { fake, b, spend } = await setup();
    const tx = await spend({ fee: 1 });
    await expect(b.submit(tx.toHexEF())).resolves.toEqual({ txid: tx.id('hex'), status: 'RECEIVED' });
    expect(await b.getStatus(tx.id('hex'))).toMatchObject({ txStatus: 'SEEN_ON_NETWORK' });
    expect(fake.stats()).toEqual({ validated: 1, skipped: 0 });
    const st = await b.waitForStatus(tx.id('hex'), 'MINED', 60_000);
    expect(st).toMatchObject({ txStatus: 'MINED', blockHeight: 1001 });
  });

  it('refuses a bad signature (Spend), a short fee and a double spend', async () => {
    const { b, spend } = await setup();
    await expect(b.submit((await spend({ fee: 1, tamper: true })).toHexEF())).rejects.toThrow(/ARC 461.*input 0/);
    await expect(b.submit((await spend({ fee: 0 })).toHexEF())).rejects.toThrow(/ARC 465/);
    await b.submit((await spend({ fee: 1 })).toHexEF());
    await expect(b.submit((await spend({ fee: 2 })).toHexEF())).rejects.toThrow(/ARC 466/);
  });

  it('refuses an EF whose claimed source output is not the one on chain', async () => {
    const { b, spend, funding } = await setup();
    const tx = await spend({ fee: 1 });
    // Claim more than the coin holds: same outpoint, a doctored copy of its source.
    const lie = Transaction.fromHex(funding.toHex());
    lie.outputs[0]!.satoshis = 200_000;
    tx.inputs[0]!.sourceTXID = funding.id('hex');
    tx.inputs[0]!.sourceTransaction = lie;
    await expect(b.submit(tx.toHexEF())).rejects.toThrow(/ARC 461.*does not match/);
  });

  it('answers a non-final lock time with ARC 476, persists nothing, and accepts the same bytes at the height', async () => {
    const { fake, b, spend } = await setup();
    const tx = await spend({ fee: 1, lockTime: 1002, sequence: 0xfffffffe });
    const ef = tx.toHexEF();
    await expect(b.submit(ef)).rejects.toMatchObject({ notFinal: true, arcStatus: 476 });
    expect(await b.getStatus(tx.id('hex'))).toBeUndefined();
    fake.mine();
    fake.mine();
    expect(fake.tip).toBe(1002);
    await expect(b.submit(ef)).resolves.toMatchObject({ txid: tx.id('hex') });
  });
});

describe('gate (a) run file: the wallet', () => {
  it('offers funding and SEEN change, never a coin a recorded step spends, and never an unseen step\'s outputs', async () => {
    const { spend, funding, script } = await setup();
    const s = script.toHex();
    const run: RunFile = {
      version: 1, dryRun: true, network: 'testnet', broadcaster: '', address: '', createdAt: '', updatedAt: '', steps: {},
      funding: { txid: funding.id('hex'), rawHex: funding.toHex(), source: 'test' },
    };
    expect(walletUtxos(run, s).map((u) => `${u.txid}:${u.outputIndex}`)).toEqual([`${funding.id('hex')}:0`]);
    const tx = await spend({ fee: 1 });
    const rec = { txid: tx.id('hex'), rawHex: tx.toHex(), state: 'built' } as never;
    run.steps['referee-deploy'] = rec;
    expect(walletUtxos(run, s)).toEqual([]); // built: funding:0 is committed, its change is not yet spendable
    (run.steps['referee-deploy'] as { state: string }).state = 'seen';
    expect(walletUtxos(run, s)).toEqual([{ txid: tx.id('hex'), outputIndex: 0, satoshis: 99_999, script: s }]);
  });
});

describe('gate (a) sizes', () => {
  it('efSizeOf equals the length of the EF the broadcaster is sent', async () => {
    const { spend, funding } = await setup();
    const tx = await spend({ fee: 1 });
    const parsed = Transaction.fromHex(tx.toHex()); // as the harness holds it: inputs by txid, no source attached
    expect(efSizeOf(parsed, lookupIn(new Map([[funding.id('hex'), funding]])))).toBe(tx.toHexEF().length / 2);
  });
});

describe('gate (a) outputs', () => {
  it('assertOutputs refuses a transaction paying any script but the expected ones, or a different count', async () => {
    const { spend, script } = await setup();
    const tx = await spend({ fee: 1 });
    expect(() => assertOutputs('t', tx, [script.toHex()])).not.toThrow();
    expect(() => assertOutputs('t', tx, [p2pkhScript('11'.repeat(20))])).toThrow(/output 0 pays an unexpected script/);
    expect(() => assertOutputs('t', tx, [script.toHex(), script.toHex()])).toThrow(/1 outputs, expected 2/);
  });
});
