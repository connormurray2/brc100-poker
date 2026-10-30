// Gate (a) is testnet only (G-02). Each guard checks something that could actually be wrong, and
// refuses mainnet (or Teranode test network) input with a reason; none is implied by a constant.
import { Transaction, Utils } from '@bsv/sdk';
import { classifyStatusRead, TESTNET_BROADCASTER, type Broadcaster } from '../broadcaster.js';
import { p2pkhScript } from './store.js';

/** The BSV Broadcaster hosts that serve BSV testnet (toolkit networks facts, 2026-10-29). */
export const TESTNET_BROADCASTERS = [TESTNET_BROADCASTER, 'https://arcade-testnet-us-1.bsvblockchain.tech'];

/** Base58Check version byte of a testnet P2PKH address. Mainnet is 0x00. */
const TESTNET_P2PKH = 0x6f;

/** Refuses any broadcaster URL that is not a known testnet host (mainnet, ttn, anything else). */
export function requireTestnetBroadcaster(url: string): string {
  const u = url.replace(/\/+$/, '');
  if (!TESTNET_BROADCASTERS.includes(u)) {
    throw new Error(`refusing: ${url} is not a BSV testnet broadcaster (G-02); gate (a) accepts only ${TESTNET_BROADCASTERS.join(' or ')}`);
  }
  return u;
}

/** Decodes the address and refuses unless it is a testnet P2PKH address. Returns its 20-byte hash. */
export function requireTestnetAddress(address: string): string {
  let prefix: number[];
  let data: number[];
  try {
    const d = Utils.fromBase58Check(address);
    prefix = typeof d.prefix === 'string' ? Utils.toArray(d.prefix, 'hex') : d.prefix;
    data = typeof d.data === 'string' ? Utils.toArray(d.data, 'hex') : d.data;
  } catch (e) {
    throw new Error(`refusing: ${address} is not a valid address (${(e as Error).message})`);
  }
  if (prefix.length !== 1 || prefix[0] !== TESTNET_P2PKH || data.length !== 20) {
    throw new Error(`refusing: ${address} is not a testnet P2PKH address (version 0x${Utils.toHex(prefix)}; G-02)`);
  }
  return Utils.toHex(data);
}

/**
 * The funding coins: the outputs of `tx` that are P2PKH to `address`, which must be a testnet
 * address. Refuses if `tx` is not `txid` or pays nothing to it. Other outputs (the sender's change)
 * are not the harness's and are never spent.
 */
export function requireFundingToAddress(tx: Transaction, txid: string, address: string): Array<{ vout: number; satoshis: number }> {
  const pkh = requireTestnetAddress(address);
  if (tx.id('hex') !== txid) throw new Error(`refusing: the funding transaction hashes to ${tx.id('hex')}, not ${txid}`);
  const script = p2pkhScript(pkh);
  const coins = tx.outputs.flatMap((o, vout) => (o.lockingScript.toHex() === script ? [{ vout, satoshis: o.satoshis! }] : []));
  if (coins.length === 0) throw new Error(`refusing: funding ${txid} has no P2PKH output to the testnet address ${address}`);
  return coins;
}

/**
 * For a funding transaction supplied as a file: refuses unless the testnet broadcaster itself
 * reports it SEEN_ON_NETWORK (no competing transaction) or MINED. A mainnet transaction is unknown there.
 */
export async function requireKnownToTestnet(b: Broadcaster, txid: string): Promise<string> {
  requireTestnetBroadcaster(b.baseUrl);
  const v = classifyStatusRead(txid, await b.readStatus(txid));
  if (v.kind === 'seen' || v.kind === 'mined') return v.status.txStatus;
  const why = v.kind === 'absent' ? 'it has never seen it (404)'
    : 'reason' in v ? v.reason
    : `it reports ${v.status.txStatus}${v.status.competingTxs?.length ? ' with competing transactions' : ''}`;
  throw new Error(`refusing --funding-hex: the testnet broadcaster ${b.baseUrl} does not report ${txid} SEEN_ON_NETWORK or MINED: ${why}`);
}
