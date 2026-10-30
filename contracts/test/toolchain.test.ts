import { describe, it, expect } from 'vitest';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, readFileSync } from 'node:fs';
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

describe('vendored Rúnar', () => {
  // npm's runar-* 0.4.6 has a forgeable stateless checkPreimage (G-25). The root overrides keep every workspace on
  // the vendored build; this fails if anything (a new dependency, a stray `npm install runar-*`) resolves elsewhere.
  const lock = JSON.parse(readFileSync(join(contractsRoot, '..', 'package-lock.json'), 'utf8'));
  const runar = Object.entries<{ resolved?: string; integrity?: string }>(lock.packages).filter(([k]) => /(^|\/)node_modules\/runar-/.test(k));

  it('every runar-* package resolves to the vendored tarballs', () => {
    expect(runar.map(([k]) => k).sort()).toEqual([
      'node_modules/runar-compiler', 'node_modules/runar-ir-schema', 'node_modules/runar-lang',
      'node_modules/runar-sdk', 'node_modules/runar-testing',
    ]);
    for (const [k, v] of runar) {
      expect(v.resolved, k).toMatch(/^file:contracts\/vendor\/runar\//);
    }
  });

  it('each lock integrity matches the tarball on disk', () => {
    // npm install keeps a stale hash (and a stale node_modules) when a tarball is replaced under the same version.
    for (const [k, v] of runar) {
      const file = join(contractsRoot, '..', v.resolved!.replace(/^file:/, ''));
      expect(existsSync(file), `${k}: ${file}`).toBe(true);
      const sri = 'sha512-' + createHash('sha512').update(readFileSync(file)).digest('base64');
      expect(v.integrity, k).toBe(sri);
    }
  });
});
