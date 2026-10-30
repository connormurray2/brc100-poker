// G-02: each gate (a) guard refuses mainnet (and Teranode test network) input.
import { describe, it, expect } from 'vitest';
import { LockingScript, PrivateKey, Transaction, UnlockingScript } from '@bsv/sdk';
import { TESTNET_BROADCASTER, createBroadcaster } from '../src/broadcaster.js';
import { requireFundingToAddress, requireKnownToTestnet, requireTestnetAddress, requireTestnetBroadcaster } from '../src/gate-a/guards.js';
import { p2pkhScript } from '../src/gate-a/store.js';

const key = PrivateKey.fromHex('3c'.repeat(32));
const testnetAddr = key.toPublicKey().toAddress('testnet');
const mainnetAddr = key.toPublicKey().toAddress('mainnet');
const pkh = key.toPublicKey().toHash('hex') as string;

function funding(script: string) {
  const tx = new Transaction();
  tx.addInput({ sourceTXID: 'ee'.repeat(32), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex('51'), sequence: 0xffffffff });
  tx.addOutput({ satoshis: 5_000, lockingScript: LockingScript.fromHex('76a914' + '99'.repeat(20) + '88ac') }); // the sender's change
  tx.addOutput({ satoshis: 3_000_000, lockingScript: LockingScript.fromHex(script) });
  return tx;
}

const broadcasterAnswering = (txid: string, resp: { status: number; body: unknown }, baseUrl = TESTNET_BROADCASTER) =>
  createBroadcaster({
    baseUrl,
    fetch: (async (input: string | URL) => (new URL(String(input)).pathname === `/tx/${txid}`
      ? new Response(JSON.stringify(resp.body), { status: resp.status })
      : new Response('{}', { status: 404 }))) as typeof fetch,
  });

describe('gate (a) guards refuse mainnet', () => {
  it('the broadcaster must be a testnet host', () => {
    expect(requireTestnetBroadcaster(TESTNET_BROADCASTER + '/')).toBe(TESTNET_BROADCASTER);
    expect(requireTestnetBroadcaster('https://arcade-testnet-us-1.bsvblockchain.tech')).toBe('https://arcade-testnet-us-1.bsvblockchain.tech');
    for (const url of ['https://arcade-us-1.bsvblockchain.tech', 'https://arcade-v2-us-1.bsvblockchain.tech', 'https://arcade.gorillapool.io',
      'https://arcade-v2-ttn-us-1.bsvblockchain.tech', 'https://arcade-v2-testnet-us-1.bsvblockchain.tech.evil.example', 'http://localhost:8080']) {
      expect(() => requireTestnetBroadcaster(url), url).toThrow(/not a BSV testnet broadcaster/);
    }
  });

  it('the gate address must decode as testnet P2PKH', () => {
    expect(testnetAddr).toMatch(/^[mn]/);
    expect(requireTestnetAddress(testnetAddr)).toBe(pkh);
    expect(() => requireTestnetAddress(mainnetAddr)).toThrow(/not a testnet P2PKH address \(version 0x00/);
    expect(() => requireTestnetAddress(testnetAddr.slice(0, -1) + (testnetAddr.endsWith('1') ? '2' : '1'))).toThrow(/not a valid address/);
  });

  it('the funding must pay P2PKH to the testnet address, and only those outputs are the gate\'s', () => {
    const tx = funding(p2pkhScript(pkh));
    expect(requireFundingToAddress(tx, tx.id('hex'), testnetAddr)).toEqual([{ vout: 1, satoshis: 3_000_000 }]);
    expect(() => requireFundingToAddress(tx, tx.id('hex'), mainnetAddr)).toThrow(/not a testnet P2PKH address/);
    expect(() => requireFundingToAddress(tx, 'ab'.repeat(32), testnetAddr)).toThrow(/hashes to/);
    const elsewhere = funding(p2pkhScript('77'.repeat(20)));
    expect(() => requireFundingToAddress(elsewhere, elsewhere.id('hex'), testnetAddr)).toThrow(/no P2PKH output to the testnet address/);
  });

  it('--funding-hex: the testnet broadcaster must report the funding SEEN or MINED', async () => {
    const tx = funding(p2pkhScript(pkh));
    const txid = tx.id('hex');
    const st = (txStatus: string, extra = {}) => ({ status: 200, body: { txid, txStatus, ...extra } });
    await expect(requireKnownToTestnet(broadcasterAnswering(txid, st('MINED', { blockHeight: 1 })), txid)).resolves.toBe('MINED');
    await expect(requireKnownToTestnet(broadcasterAnswering(txid, st('SEEN_ON_NETWORK')), txid)).resolves.toBe('SEEN_ON_NETWORK');
    // A mainnet transaction is unknown to the testnet broadcaster.
    await expect(requireKnownToTestnet(broadcasterAnswering(txid, { status: 404, body: {} }), txid)).rejects.toThrow(/never seen it \(404\)/);
    await expect(requireKnownToTestnet(broadcasterAnswering(txid, st('RECEIVED')), txid)).rejects.toThrow(/reports RECEIVED/);
    await expect(requireKnownToTestnet(broadcasterAnswering(txid, st('SEEN_ON_NETWORK', { competingTxs: ['cd'.repeat(32)] })), txid)).rejects.toThrow(/competing/);
    await expect(requireKnownToTestnet(broadcasterAnswering(txid, st('REJECTED', { extraInfo: 'bad' })), txid)).rejects.toThrow(/REJECTED/);
    // And it must be asked of a testnet broadcaster, not a mainnet one that knows the transaction.
    await expect(requireKnownToTestnet(broadcasterAnswering(txid, st('MINED'), 'https://arcade-us-1.bsvblockchain.tech'), txid)).rejects.toThrow(/not a BSV testnet broadcaster/);
  });
});
