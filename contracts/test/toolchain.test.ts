import { describe, it, expect } from 'vitest';
import { execFileSync } from 'node:child_process';
import { join } from 'node:path';
import { RunarContract, MockProvider, LocalSigner, buildP2PKHScript } from 'runar-sdk';
import { contractsRoot, loadArtifact } from './helpers';

async function deploy() {
  execFileSync('node', [join(contractsRoot, 'compile.mjs'), 'Trivial'], { stdio: 'pipe' });
  const provider = new MockProvider('testnet'); // validateBroadcasts: every spend runs through @bsv/sdk Spend
  const signer = new LocalSigner('99'.repeat(32));
  provider.addUtxo(await signer.getAddress(), {
    txid: '99'.repeat(32), outputIndex: 0, satoshis: 50_000_000,
    script: buildP2PKHScript(await signer.getPublicKey()),
  });
  const contract = new RunarContract(loadArtifact('Trivial'), []);
  await contract.deploy(provider, signer, { satoshis: 1000 });
  return { provider, signer, contract };
}

describe('toolchain', () => {
  it('compiles a trivial contract and spends it through Spend', async () => {
    const ok = await deploy();
    await ok.contract.call('check', [3n], ok.provider, ok.signer, {});
    expect(ok.provider.getBroadcastedTxs()).toHaveLength(2); // the lock and the spend

    const bad = await deploy();
    await expect(bad.contract.call('check', [4n], bad.provider, bad.signer, {})).rejects.toThrow(/input 0: Script evaluation error/);
  }, 120_000);
});
