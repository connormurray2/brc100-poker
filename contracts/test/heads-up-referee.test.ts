// The spike's HeadsUpReferee, compiled with the vendored toolchain, spent through @bsv/sdk Spend
// (MockProvider validates every input). Gate (a) mines these shapes on testnet; this pins that the
// copy compiles and that one post-and-forfeit path is accepted, and a premature forfeit is not.
import { describe, it, expect, beforeAll } from 'vitest';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { Hash, PrivateKey, ProtoWallet, Transaction, Utils } from '@bsv/sdk';
import { RunarContract, MockProvider, LocalSigner, buildP2PKHScript } from 'runar-sdk';
import type { RunarArtifact } from 'runar-ir-schema';
import { contractsRoot, loadArtifact } from './helpers';

const PROTOCOL: [1, string] = [1, 'brc100 poker escrow'];
const KEY_ID = 'gate-a-test';
const ANCHOR = 1000;
const MAX_SEQ = 12;
const WINDOW = 2;
const BIG_BLIND = 100;
const BAL = 10_000;
const SEQ = 3;
const rung = (seq: number) => ANCHOR + (MAX_SEQ - seq) * WINDOW;
const p2pkh = (pub: string) => `76a914${Utils.toHex(Hash.hash160(Utils.toArray(pub, 'hex')))}88ac`;

/** A seat signs the way a BRC-100 wallet does: createSignature over a digest the caller built. */
class Seat {
  readonly wallet: ProtoWallet;
  constructor(hex: string) { this.wallet = new ProtoWallet(PrivateKey.fromHex(hex)); }
  async key() { return (await this.wallet.getPublicKey({ protocolID: PROTOCOL, keyID: KEY_ID, counterparty: 'self' })).publicKey; }
  async sign(sighash: string, flag: number) {
    const { signature } = await this.wallet.createSignature({ hashToDirectlySign: Utils.toArray(sighash, 'hex'), protocolID: PROTOCOL, keyID: KEY_ID, counterparty: 'self' });
    return Utils.toHex(signature) + flag.toString(16).padStart(2, '0');
  }
}

let art: RunarArtifact;
let coinSeq = 0;
async function coin(provider: MockProvider, satoshis: number) {
  const signer = new LocalSigner((++coinSeq).toString(16).padStart(64, '4'));
  const utxo = { txid: coinSeq.toString(16).padStart(64, 'c'), outputIndex: 0, satoshis, script: buildP2PKHScript(await signer.getPublicKey()) };
  provider.addUtxo(await signer.getAddress(), utxo);
  return { signer, utxo };
}
const last = (p: MockProvider) => Transaction.fromHex(p.getBroadcastedTxs().at(-1)!);
const follow = (p: MockProvider) => {
  const tx = last(p);
  return RunarContract.fromUtxo(art, { txid: tx.id('hex'), outputIndex: 0, satoshis: tx.outputs[0]!.satoshis!, script: tx.outputs[0]!.lockingScript.toHex() });
};

describe('HeadsUpReferee (spike copy)', () => {
  beforeAll(() => {
    execFileSync('node', [join(contractsRoot, 'compile.mjs'), 'HeadsUpReferee'], { stdio: 'pipe' });
    art = loadArtifact('HeadsUpReferee');
  }, 120_000);

  it('compiles with the vendored toolchain and names nothing t<digits> (icellan/runar#175)', () => {
    const src = readFileSync(join(contractsRoot, 'src', 'HeadsUpReferee.runar.ts'), 'utf8');
    expect(src.match(/\bt\d+\b/g)).toBeNull();
    expect(art.abi.methods.filter((m) => m.isPublic).map((m) => m.name)).toEqual(['post', 'act', 'fold', 'forfeit', 'close']);
    expect(art.abi.methods.find((m) => m.name === 'post')!.sigHashType).toBe(0xc3); // SINGLE|ANYONECANPAY|FORKID
  });

  it('a co-signed post at its rung is accepted (exact cover), and a forfeit at the deadline, not before', async () => {
    const provider = new MockProvider('testnet');
    const s0 = new Seat('a1'.repeat(32));
    const s1 = new Seat('b2'.repeat(32));
    const [pk0, pk1] = [await s0.key(), await s1.key()];
    const deployer = await coin(provider, 1_000_000);
    const escrow = new RunarContract(art, [pk0, pk1, BigInt(ANCHOR), BigInt(MAX_SEQ), BigInt(WINDOW), BigInt(BIG_BLIND), BigInt(BAL), BigInt(BAL)]);
    await escrow.deploy(provider, deployer.signer, { satoshis: 2 * BAL });

    // Seat 0 has bet 200 over seat 1's big blind; seat 1 is on the clock.
    const lt = rung(SEQ);
    const args = [BigInt(SEQ), 1n, 200n, 100n, null, null];
    const opts = {
      locktime: lt, sequence: 0xfffffffe,
      outputs: [{ satoshis: 2 * BAL, state: { bal0: BigInt(BAL), bal1: BigInt(BAL), com0: 200n, com1: 100n, phase: 1n, seq: BigInt(SEQ), toAct: 1n, deadline: BigInt(lt + WINDOW) } }],
    } as never;
    // Exact cover (icellan/runar#176: a SINGLE post with change never validates). Size the fee
    // against a large probe coin, then fund with exactly that fee so there is no change output.
    const probe = new MockProvider('testnet');
    const big = await coin(probe, 1_000_000);
    escrow.connect(probe, big.signer);
    const sized = await escrow.prepareCall('post', args, opts);
    const fee = 1_000_000 - sized.tx.outputs[1]!.satoshis!;
    const poster = await coin(provider, fee);
    escrow.connect(provider, poster.signer);
    const prepared = await escrow.prepareCall('post', args, opts);
    expect(prepared.tx.outputs).toHaveLength(1);
    expect(prepared.sighash).toBe(sized.sighash); // SINGLE|ANYONECANPAY: the seats' signatures ignore the fee input
    await escrow.finalizeCall(prepared, { 4: await s0.sign(prepared.sighash, 0xc3), 5: await s1.sign(prepared.sighash, 0xc3) });
    const posted = follow(provider);
    expect(posted.state).toMatchObject({ phase: 1n, seq: BigInt(SEQ), toAct: 1n, deadline: BigInt(lt + WINDOW) });

    // Seat 1 never answers. A forfeit one block before the deadline is refused; at the deadline anyone may forfeit.
    const forfeit = async (locktime: number) => {
      const fc = await coin(provider, 3_000);
      posted.connect(provider, fc.signer);
      return posted.call('forfeit', ['00'.repeat(20), 0n], provider, fc.signer, {
        locktime,
        terminalOutputs: [{ scriptHex: p2pkh(pk0), satoshis: BAL + 100 }, { scriptHex: p2pkh(pk1), satoshis: BAL - 100 }],
        feeUtxo: fc.utxo,
      } as never);
    };
    await expect(forfeit(lt + WINDOW - 1)).rejects.toThrow(/input 0/);
    await forfeit(lt + WINDOW);
    const out = last(provider);
    expect(out.lockTime).toBe(lt + WINDOW);
    expect(out.outputs.map((o) => o.satoshis)).toEqual([BAL + 100, BAL - 100]);
    expect(provider.getValidationStats().skipped).toBe(0);
  }, 120_000);
});
