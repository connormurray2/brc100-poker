// Funds the gate key from BSV Desktop: one createAction paying the gate address, approved in the wallet.
// The wallet's local JSON API (http://localhost:3321) is the substrate; Node has no window for 'auto'.
import { HTTPWalletJSON, P2PKH, Transaction, WalletClient } from '@bsv/sdk';

export const ORIGINATOR = 'gates.brc100-poker.local';

export interface DesktopFunding { txid: string; rawHex: string; efHex: string; version: string }

/** The SDK reports a non-envelope error as only its HTTP status: keep the wallet's own words. */
const reportingFetch: typeof fetch = async (input, init) => {
  const res = await fetch(input, init);
  if (!res.ok) console.error(`BSV Desktop ${String(input).split('/').pop()}: HTTP ${res.status} ${(await res.clone().text()).slice(0, 2000)}`);
  return res;
};

/** Refuses unless BSV Desktop reports testnet (G-02), then asks it to pay `sats` to `address`. */
export async function fundFromDesktop(address: string, sats: number): Promise<DesktopFunding> {
  const wallet = new WalletClient(new HTTPWalletJSON(ORIGINATOR, 'http://localhost:3321', reportingFetch), ORIGINATOR);
  const { network } = await wallet.getNetwork({});
  if (network !== 'testnet') throw new Error(`refusing: BSV Desktop reports ${network}, not testnet`);
  const { version } = await wallet.getVersion({});
  const r = await wallet.createAction({
    description: 'Fund the gate (a) test key',
    outputs: [{ lockingScript: new P2PKH().lock(address).toHex(), satoshis: sats, outputDescription: 'gate (a) test key' }],
    options: { randomizeOutputs: false },
  });
  if (!r.tx) throw new Error('BSV Desktop returned no transaction (was the action declined?)');
  const tx = Transaction.fromAtomicBEEF(r.tx);
  return { txid: tx.id('hex'), rawHex: tx.toHex(), efHex: tx.toHexEF(), version };
}
