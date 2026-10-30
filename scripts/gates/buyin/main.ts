// Gate (b), browser page. Wires BSV Desktop (a BRC-100 wallet) to the pure checks in ./checks.ts.
//
//   npm run gate:b -w scripts/gates      then open http://127.0.0.1:5173/
//
// Flow (spec 10.5):
//   1. getNetwork(), refuse unless testnet.
//   2. createAction: a 1,000-sat P2PKH "escrow" locked to a page-held test key.
//   3. createAction with the escrow as a foreign input (inputBEEF), the wallet's own coins, and two pinned
//      outputs (2,800-sat continuation, 1-sat marker), signAndProcess:false, randomizeOutputs:false.
//   4. Sign the foreign input here (P2PKH, SIGHASH_ALL|FORKID) over the wallet's transaction.
//   5. signAction. Then check the final transaction and ask BSV Broadcaster for the txid's status.
//
// The test keys exist only in this page's memory. They are never persisted, logged or placed in a URL.
import { P2PKH, PrivateKey, Transaction, WalletClient } from '@bsv/sdk';
import { CONTINUATION_SATS, checkBuyIn, inputOutpoint, isTerminal, networkFact, requireTestnet, sameSignedTx, type BroadcasterStatus, type Fact, type PinnedOutput } from './checks.js';

const ORIGINATOR = 'gates.brc100-poker.local';
const BROADCASTER = 'https://arcade-v2-testnet-us-1.bsvblockchain.tech';
const ESCROW_SATS = 1000;
const MARKER_SATS = 1;
const STATUS_DEADLINE_MS = 60_000;
const STATUS_POLL_MS = 2000;
const STATUS_REQUEST_TIMEOUT_MS = 8000;

// Fresh per page load. Not persisted anywhere.
const testKey = PrivateKey.fromRandom();
const markerKey = PrivateKey.fromRandom();
const testAddress = testKey.toPublicKey().toAddress('testnet');

const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

$('addr').textContent = testAddress;

const stepsEl = $('steps');
function step(text: string): HTMLLIElement {
  const li = document.createElement('li');
  li.textContent = text;
  stepsEl.appendChild(li);
  return li;
}
const finish = (li: HTMLLIElement, text?: string) => { li.classList.add('done'); if (text) li.textContent = text; };
const failStep = (li: HTMLLIElement, text: string) => { li.classList.add('fail'); li.textContent = text; };

function renderFacts(facts: Fact[]) {
  const ul = $('facts');
  ul.replaceChildren();
  for (const f of facts) {
    const li = document.createElement('li');
    li.className = f.pass ? 'pass' : 'fail';
    const tag = document.createElement('span');
    tag.className = 'tag';
    tag.textContent = f.pass ? 'PASS' : 'FAIL';
    const fact = document.createElement('strong');
    fact.textContent = f.fact;
    const detail = document.createElement('div');
    detail.className = 'muted';
    detail.textContent = f.detail;
    li.append(tag, ' ', fact, detail);
    ul.appendChild(li);
  }
}

/**
 * Poll GET /tx/{txid} until a terminal answer or the deadline. 404 and UNKNOWN mean "not yet", and so do
 * network errors (reported in `note` if nothing better arrives). Each request has its own timeout.
 */
async function pollBroadcaster(txid: string): Promise<{ status: BroadcasterStatus | undefined; note: string }> {
  const url = `${BROADCASTER}/tx/${txid}`;
  const deadline = Date.now() + STATUS_DEADLINE_MS;
  let status: BroadcasterStatus | undefined;
  let note = 'no response';
  for (;;) {
    try {
      const res = await fetch(url, { signal: AbortSignal.timeout(STATUS_REQUEST_TIMEOUT_MS) });
      const body = await res.text();
      note = `HTTP ${res.status}: ${body}`;
      if (res.ok) {
        try { status = JSON.parse(body) as BroadcasterStatus; } catch { status = undefined; }
        if (isTerminal(status)) break;
      }
    } catch (e) {
      note = `fetch failed (${(e as Error).message}); check by hand: curl ${url}`;
    }
    if (Date.now() + STATUS_POLL_MS > deadline) break;
    $('status').textContent = `${url}\nwaiting: ${note}`;
    await sleep(STATUS_POLL_MS);
  }
  return { status, note };
}

async function run() {
  const errorEl = $('error');
  const runBtn = $<HTMLButtonElement>('run');
  errorEl.textContent = '';
  stepsEl.replaceChildren();
  $('facts').replaceChildren();
  runBtn.disabled = true;

  const wallet = new WalletClient('auto', ORIGINATOR);
  let reference: string | undefined;
  let current = step('Connecting to the wallet and checking the network');
  try {
    // 1. Network guard (G-02).
    const { network } = await wallet.getNetwork();
    requireTestnet(network);
    let version = 'unknown';
    try { version = (await wallet.getVersion()).version; } catch { /* informational only */ }
    finish(current, `Wallet is on testnet (reported version: ${version})`);

    // 2. The escrow: an ordinary action paying 1,000 sats to the page-held key.
    const escrowScript = new P2PKH().lock(testAddress);
    current = step(`Creating the ${ESCROW_SATS}-sat test escrow (approve the wallet prompt)`);
    const escrow = await wallet.createAction({
      description: 'Gate b: 1000-sat test escrow',
      outputs: [{ lockingScript: escrowScript.toHex(), satoshis: ESCROW_SATS, outputDescription: 'Test escrow for gate b' }],
      options: { acceptDelayedBroadcast: false, randomizeOutputs: false },
    });
    if (!escrow.tx || !escrow.txid) throw new Error('The wallet returned no transaction for the escrow action.');
    const escrowTx = Transaction.fromAtomicBEEF(escrow.tx);
    const escrowVout = escrowTx.outputs.findIndex((o) => o.lockingScript.toHex() === escrowScript.toHex() && o.satoshis === ESCROW_SATS);
    if (escrowVout < 0) throw new Error('The escrow output is not in the wallet\'s transaction.');
    const foreign = { txid: escrow.txid, vout: escrowVout };
    finish(current, `Escrow ${foreign.txid}:${foreign.vout} created`);

    // 3. The buy-in: foreign input + wallet coins, pinned outputs, no signing, no reordering.
    const unlocker = new P2PKH().unlock(testKey, 'all', false, ESCROW_SATS, escrowScript);
    const unlockLen = await unlocker.estimateLength(); // 108, the P2PKH worst case
    const pinned: PinnedOutput[] = [
      { name: 'continuation', lockingScript: escrowScript.toHex(), satoshis: CONTINUATION_SATS },
      { name: 'marker', lockingScript: new P2PKH().lock(markerKey.toPublicKey().toAddress('testnet')).toHex(), satoshis: MARKER_SATS },
    ];
    current = step('Asking the wallet for the buy-in transaction (approve the wallet prompt)');
    const created = await wallet.createAction({
      description: 'Gate b: test buy-in with a foreign input',
      inputBEEF: escrow.tx,
      inputs: [{ outpoint: `${foreign.txid}.${foreign.vout}`, unlockingScriptLength: unlockLen, inputDescription: 'Test escrow (foreign input)' }],
      outputs: pinned.map((p) => ({ lockingScript: p.lockingScript, satoshis: p.satoshis, outputDescription: p.name === 'continuation' ? 'Escrow continuation' : 'Buy-in marker' })),
      options: { signAndProcess: false, randomizeOutputs: false, acceptDelayedBroadcast: false },
    });
    const signable = created.signableTransaction;
    if (!signable) throw new Error('The wallet returned no signableTransaction.');
    reference = signable.reference;
    const wip = Transaction.fromAtomicBEEF(signable.tx);
    finish(current, `Wallet built a ${wip.inputs.length}-input, ${wip.outputs.length}-output transaction`);

    // 4. Sign the foreign input here. It is index 0 if the wallet kept the order; if not, sign wherever it is
    // so the run still completes and the facts below report the move.
    current = step('Signing the foreign input locally');
    const wipChecks = checkBuyIn(wip, { foreign, pinned });
    const signAt = wip.inputs.findIndex((_, i) => { const o = inputOutpoint(wip, i); return o?.txid === foreign.txid && o.vout === foreign.vout; });
    if (signAt < 0) throw new Error('The foreign input is not in the wallet\'s transaction at all.');
    const unlockingScript = (await unlocker.sign(wip, signAt)).toHex();
    finish(current, `Signed input ${signAt} (SIGHASH_ALL|FORKID, ${unlockingScript.length / 2} bytes)`);

    // 5. Complete.
    current = step('Completing with signAction');
    const done = await wallet.signAction({ reference, spends: { [signAt]: { unlockingScript } }, options: { acceptDelayedBroadcast: false } });
    reference = undefined; // consumed
    if (!done.txid) throw new Error('signAction returned no txid.');
    finish(current, `Completed: txid ${done.txid}`);

    const finalTx = done.tx ? Transaction.fromAtomicBEEF(done.tx) : undefined;
    const ordering = checkBuyIn(finalTx ?? wip, { foreign, pinned });
    const same = sameSignedTx(wip, finalTx);
    renderFacts([...ordering, same]);

    $('status').style.whiteSpace = 'pre-wrap';
    $('status').textContent = `${BROADCASTER}/tx/${done.txid}\nasking BSV Broadcaster (up to ${STATUS_DEADLINE_MS / 1000} s)...`;
    const { status, note } = await pollBroadcaster(done.txid);
    $('status').textContent = `${BROADCASTER}/tx/${done.txid}\n${note}`;
    const networkResult = networkFact(status);
    const facts = [...ordering, same, networkResult];
    renderFacts(facts);

    // PASS only if every fact passes: ordering, the signed transaction is the returned one, and the network has it.
    const allPass = facts.every((f) => f.pass);
    $('report').textContent = [
      `Date: ${new Date().toISOString()}`,
      `BSV Desktop (wallet-reported version): ${version}`,
      `Escrow txid:vout: ${foreign.txid}:${foreign.vout}`,
      `Buy-in txid: ${done.txid}`,
      `Input count / output count (final): ${finalTx ? `${finalTx.inputs.length} / ${finalTx.outputs.length}` : 'n/a'}`,
      `Order as the wallet first built it: ${wipChecks.every((f) => f.pass) ? 'all facts pass' : `${wipChecks.filter((f) => !f.pass).map((f) => f.fact).join('; ')} FAIL`}`,
      ...facts.map((f) => `${f.pass ? 'PASS' : 'FAIL'}  ${f.fact}: ${f.detail}`),
      `Broadcaster: ${note}`,
      'Who broadcast it (BSV Desktop itself, and through what): not visible to this page; note it by hand.',
      `Verdict: ${allPass ? 'PASS' : 'FAIL (see the FAIL lines; an ordering failure means the spec needs a revision, see docs/gates/b-bsv-desktop-buyin.md)'}`,
    ].join('\n');
  } catch (e) {
    failStep(current, `${current.textContent} - failed`);
    errorEl.textContent = (e as Error).message ?? String(e);
    if (reference) {
      // Release the wallet's reserved inputs if we built but never completed an action.
      try { await wallet.abortAction({ reference }); } catch { /* best effort */ }
    }
  } finally {
    runBtn.disabled = false;
  }
}

$('run').addEventListener('click', () => { void run(); });
