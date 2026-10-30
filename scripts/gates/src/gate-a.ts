// Gate (a): get every dispute transaction kind accepted by the BSV testnet network.
//
//   npm run gate:a -w scripts/gates -- address                 print the funding address (creates secrets/gate-a.key)
//   npm run gate:a -w scripts/gates -- run                     first run: BSV Desktop funds the key (approve it in the wallet)
//   npm run gate:a -w scripts/gates -- run --funding <txid>    first run: fund from that transaction instead
//   npm run gate:a -w scripts/gates -- run                     resume from secrets/gate-a-run.json
//   npm run gate:a -w scripts/gates -- run --dry-run [--fresh] the whole sequence on an in-process broadcaster
//   npm run gate:a -w scripts/gates -- status [--dry-run]      print the run file's table
//
// Options: --funding-hex <file> (the funding transaction's raw hex, if no service has it),
// --stop-after <step> (stop once that step reaches its target state; re-run to resume).
//
// Sequence (plan Task 9, step 4):
//   1. read /policy;
//   2. deploy a GateReferee holding an invalid proof;
//   3. prepare + fraud (eq 1: the ~1.8 MB prepare);
//   4. deploy a HeadsUpReferee (with the post's exact-cover coin and a 2-of-2 refund pot), post at its
//      rung, act, then forfeit at the deadline;
//   5. the pre-signed nLockTime refund.
// Each step is final at SEEN_ON_NETWORK or later with no competing transaction (G-10); nothing waits for a
// block (G-14). The HeadsUpReferee's ladder is anchored below the tip, so every nLockTime has already
// passed when its transaction is built. Gate (a) passes when all eight are SEEN. Every transaction's txid,
// size, fee, status and timestamps are written to the run file before and after it is submitted.
//
// The key is never printed or logged; only its testnet address is.
import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync, rmSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { Hash, LockingScript, Transaction, UnlockingScript, Utils } from '@bsv/sdk';
import { RunarContract } from 'runar-sdk';
import type { RunarArtifact } from 'runar-ir-schema';
import { TESTNET_BROADCASTER, createBroadcaster, feeFor, type Broadcaster, type FeeRate } from './broadcaster.js';
import {
  STEP_IDS, knownTxs, loadOrCreateKey, loadRun, nowIso, p2pkhScript, pkhOf, saveRun, utxoOf, walletUtxos,
  type HurParams, type RunFile, type StepId, type StepRecord,
} from './gate-a/store.js';
import {
  FRAUD_EQ, POSTED, RAISE_TO, REF_SATS, assertOutputs, buildAct, buildForfeit, buildFraud, buildFunded, buildPost,
  buildPrepare, buildRefund, continuationOf, efSizeOf, fraudExpected, hurArgs, hurState, invalidProof, lookupIn, measure,
  potScript, prepareExpected, refereeLockingScript, seatsOf, sizePostCoin, takeSdkWarnings, bodyOf, walletOf,
  type HurKit, type RefereeKit, type Wallet,
} from './gate-a/txs.js';
import { FakeBroadcaster } from './gate-a/fake-broadcaster.js';
import { Stop, advance, tipNow, type StepCtx, type TipSource } from './gate-a/steps.js';
import { fundFromDesktop } from './gate-a/desktop.js';
import { requireFundingToAddress, requireKnownToTestnet, requireTestnetAddress, requireTestnetBroadcaster } from './gate-a/guards.js';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
const SECRETS = join(ROOT, 'secrets');
const PATHS = {
  real: { key: join(SECRETS, 'gate-a.key'), run: join(SECRETS, 'gate-a-run.json') },
  dry: { key: join(SECRETS, 'gate-a-dry-run.key'), run: join(SECRETS, 'gate-a-dry-run.json') },
};
const FUNDING_SATS = 9_000; // the dry run locks about 5,000 sats in contract outputs and pays about 3,000 in fees at 1 sat/kB
const WOC_TESTNET = 'https://api.whatsonchain.com/v1/bsv/test';

// Testnet constants (spec §10.1: W = 2 on testnet, K = 12 per betting segment).
const HUR = { window: 2, maxSeq: 12, seq: 3, bigBlind: 100, bal: 1_000, refundBal: 500 }; // bal >= RAISE_TO (600)

/** GET /policy as read from the testnet broadcaster on 2026-10-30; the dry run serves this body. */
const OBSERVED_POLICY = {
  policy: { miningFee: { satoshis: 1, bytes: 1000 }, maxtxsizepolicy: 100000000, maxscriptsizepolicy: 100000000, maxtxsigopscountspolicy: 4294967295, standardFormatSupported: true },
  timestamp: '2026-10-30T12:06:36.626896277Z',
};
const DRY_RUN_START_TIP = 1_700_000;

const log = (msg: string) => console.log(`[${new Date().toISOString().slice(11, 19)}] ${msg}`);

// ---- context ------------------------------------------------------------------------------------

interface Ctx extends StepCtx {
  runPath: string;
  dryRun: boolean;
  wallet: Wallet;
  fake?: FakeBroadcaster;
  ref: RefereeKit;
  hur: HurKit;
  rate: FeeRate;
}

function compileArtifacts(): Record<'chk' | 'ref' | 'hur', RunarArtifact> {
  execFileSync('node', [join(ROOT, 'contracts', 'compile.mjs'), 'WitnessEcMulCheck', 'GateReferee', 'HeadsUpReferee'], { stdio: 'pipe' });
  const load = (n: string) => JSON.parse(readFileSync(join(ROOT, 'contracts', 'artifacts', `${n}.json`), 'utf8'), (_k, v) =>
    typeof v === 'string' && /^-?\d+n$/.test(v) ? BigInt(v.slice(0, -1)) : v) as RunarArtifact;
  return { chk: load('WitnessEcMulCheck'), ref: load('GateReferee'), hur: load('HeadsUpReferee') };
}

const scriptHash = (a: RunarArtifact) => Utils.toHex(Hash.sha256(Utils.toArray(a.script, 'hex')));

// ---- recording ----------------------------------------------------------------------------------

const stepTx = (ctx: Ctx, id: StepId) => {
  const s = ctx.run.steps[id];
  if (!s) throw new Error(`step ${id} has no transaction yet`);
  return Transaction.fromHex(s.rawHex);
};

const largestCoin = (ctx: Ctx) => {
  const u = walletUtxos(ctx.run, ctx.wallet.script)[0];
  if (!u) throw new Error('the wallet has no spendable coin (is the funding transaction recorded, and is the previous step SEEN?)');
  return u;
};

/**
 * Write-ahead: check, measure and persist a built transaction before anything is submitted. Refuses
 * unless every output pays exactly the expected script (the gate key, a seat payout pinned by a
 * contract, or a contract script) and the fee covers the EF size.
 */
function record(ctx: Ctx, id: StepId, what: string, tx: Transaction, expected: string[], notes: Record<string, unknown> = {}): StepRecord {
  assertOutputs(id, tx, expected);
  const txs = knownTxs(ctx.run);
  const policy = ctx.run.policy!;
  const m = measure(tx, lookupIn(txs), ctx.rate, policy);
  if (m.fee < m.required) throw new Error(`${id}: pays ${m.fee} sats, needs ${m.required} for ${m.efSize} EF bytes (builder bug)`);
  const warnings = takeSdkWarnings();
  if (warnings.length) notes.sdkWarnings = warnings;
  const rec: StepRecord = {
    what, txid: tx.id('hex'), rawHex: tx.toHex(), size: m.size, efSize: m.efSize, fee: m.fee, feeRate: ctx.rate,
    feeSizedOn: m.efSize, inputs: tx.inputs.length, outputs: tx.outputs.length, lockTime: tx.lockTime,
    maxUnlockingScript: m.maxUnlockingScript, maxLockingScript: m.maxLockingScript, flags: m.flags,
    notes: Object.keys(notes).length ? notes : undefined, state: 'built', builtAt: nowIso(),
  };
  ctx.run.steps[id] = rec;
  ctx.save();
  log(`${id}: built ${rec.txid} — ${m.size} B (EF ${m.efSize} B), fee ${m.fee} sats, largest unlocking script ${m.maxUnlockingScript} B; outputs checked`);
  for (const f of m.flags) log(`${id}: FLAG ${f}`);
  for (const w of warnings) log(`${id}: runar-sdk said (recorded in notes.sdkWarnings): ${w.slice(0, 160)}`);
  return rec;
}

async function ensure(ctx: Ctx, id: StepId, build: () => Promise<void>) {
  if (ctx.run.steps[id]) return;
  takeSdkWarnings(); // drop anything left from an earlier step or a failed build: notes hold only this step's
  await build();
}

/** WhatsOnChain testnet's tip (`/chain/info` blocks): the funded run's second tip source. */
const wocTip: TipSource = {
  name: 'WhatsOnChain /chain/info blocks',
  read: async () => {
    const res = await fetch(`${WOC_TESTNET}/chain/info`, { signal: AbortSignal.timeout(15_000) });
    const h = res.ok ? ((await res.json()) as { blocks?: unknown }).blocks : undefined;
    return typeof h === 'number' && Number.isInteger(h) && h > 0 ? h : undefined;
  },
};

// ---- the sequence --------------------------------------------------------------------------------

async function stepPolicy(ctx: Ctx) {
  if (!ctx.run.policy) {
    const p = await ctx.b.readPolicy();
    ctx.run.policy = { ...p, readAt: nowIso(), source: ctx.dryRun ? `dry run: the body read from ${TESTNET_BROADCASTER}/policy on 2026-10-30` : `${ctx.b.baseUrl}/policy` };
    ctx.save();
  }
  const p = ctx.run.policy;
  ctx.rate = p.miningFee;
  log(`policy: ${p.miningFee.satoshis} sat / ${p.miningFee.bytes} B (${p.satPerKb} sat/kB), maxtxsizepolicy ${p.maxTxSize}, maxscriptsizepolicy ${p.maxScriptSize} (${p.source})`);
}

async function gateReferee(ctx: Ctx) {
  const { wallet: w, rate } = ctx;
  const src = () => lookupIn(knownTxs(ctx.run));
  await ensure(ctx, 'referee-deploy', async () => {
    const tx = await buildFunded(w, largestCoin(ctx), [{ script: refereeLockingScript(ctx.ref), satoshis: REF_SATS }], rate, src());
    record(ctx, 'referee-deploy', 'GateReferee deploy, recording an invalid proof (z + 1)', tx, [refereeLockingScript(ctx.ref), w.script],
      { refereeLockingScript: tx.outputs[0]!.lockingScript.toBinary().length });
  });
  await advance(ctx, 'referee-deploy', 'seen');

  await ensure(ctx, 'prepare', async () => {
    const ref = utxoOf(stepTx(ctx, 'referee-deploy'), 0);
    const tx = await buildPrepare(ctx.ref, w, src(), rate, ref, largestCoin(ctx));
    record(ctx, 'prepare', `GateReferee.prepare (eq ${FRAUD_EQ}): creates two WitnessEcMulCheck outputs`, tx, prepareExpected(ctx.ref, w, ref), {
      refereeUnlock: tx.inputs[0]!.unlockingScript!.toBinary().length,
      checkerLockingScripts: [tx.outputs[1]!.lockingScript.toBinary().length, tx.outputs[2]!.lockingScript.toBinary().length],
    });
  });
  await advance(ctx, 'prepare', 'seen');

  await ensure(ctx, 'fraud', async () => {
    const prep = stepTx(ctx, 'prepare');
    const tx = await buildFraud(ctx.ref, w, src(), rate, utxoOf(prep, 0), [utxoOf(prep, 1), utxoOf(prep, 2)], largestCoin(ctx));
    record(ctx, 'fraud', 'GateReferee.fraud: spends the referee and both checkers, returns the bond', tx, fraudExpected(ctx.ref, w, utxoOf(prep, 0)), {
      refereeUnlock: tx.inputs[0]!.unlockingScript!.toBinary().length,
    });
  });
  await advance(ctx, 'fraud', 'seen');

  log('the prepare + fraud pair is SEEN_ON_NETWORK with no competing transaction (each read from GET /tx/{txid})');
}

/** Anchors the ladder 3W + 1 blocks below the tip, so the post, forfeit and refund lock times have all passed. */
async function hurParams(ctx: Ctx): Promise<Omit<HurParams, 'sdkRate' | 'postCoinSats'>> {
  const { tip, source } = await tipNow(ctx);
  if (tip === undefined) throw new Error(`no chain tip to anchor the ladder on (${source}); re-run`);
  const { window: W, maxSeq, seq } = HUR;
  const h0 = tip - 3 * W - 1;
  log(`hur: tip ${tip} (${source}); ladder anchored so the refund's nLockTime ${h0 + 3 * W} is already final`);
  return {
    h0, anchor: h0 - (maxSeq - seq) * W, maxSeq, window: W, seq, bigBlind: HUR.bigBlind, bal: HUR.bal,
    postLockTime: h0, // the rung for seq: anchor + (maxSeq - seq) * W, already reached
    forfeitLockTime: h0 + 2 * W, // post sets deadline = rung + W; the act adds another W
    refundLockTime: h0 + 3 * W,
    refundBal: HUR.refundBal,
  };
}

async function headsUp(ctx: Ctx) {
  const { wallet: w, rate } = ctx;
  const txs = () => knownTxs(ctx.run);
  const policy = ctx.run.policy!;

  await ensure(ctx, 'hur-deploy', async () => {
    const base = await hurParams(ctx);
    const escrowScript = new RunarContract(ctx.hur.art, hurArgs(ctx.hur, base)).getLockingScript();
    const plan = { seq: base.seq, lockTime: base.postLockTime, deadline: base.postLockTime + base.window, bal: base.bal, window: base.window };
    const { coinSats, sdkRate } = await sizePostCoin(ctx.hur, w, escrowScript, plan, rate, policy.satPerKb);
    // The refund pot pays the refund's own fee: size it on a dummy refund with the longest signatures.
    const pot = potScript(ctx.hur.pk0, ctx.hur.pk1);
    const dummyPot = { txid: 'b0'.repeat(32), outputIndex: 2, satoshis: 0, script: pot };
    const dummy = await buildRefund(ctx.hur.seats, dummyPot, base.refundLockTime, base.refundBal, true);
    const potSats = 2 * base.refundBal + feeFor(efSizeOf(dummy, () => dummyPot), rate);
    ctx.run.hur = { ...base, sdkRate, postCoinSats: coinSats };
    ctx.save();

    const tx = await buildFunded(w, largestCoin(ctx), [
      { script: escrowScript, satoshis: 2 * base.bal },
      { script: w.script, satoshis: coinSats }, // the post's exact-cover fee coin
      { script: pot, satoshis: potSats },
    ], rate, lookupIn(txs()));
    record(ctx, 'hur-deploy', 'HeadsUpReferee deploy, plus the post\'s exact-cover coin and a 2-of-2 refund pot', tx,
      [escrowScript, w.script, pot, w.script], { params: ctx.run.hur });
  });
  const p = ctx.run.hur!;
  const seatPay = [p2pkhScript(pkhOf(ctx.hur.pk0)), p2pkhScript(pkhOf(ctx.hur.pk1))];
  // The refund is signed over the pot's txid before the pot is submitted (G-38), and recorded now;
  // it is submitted last.
  await ensure(ctx, 'refund', async () => {
    const refund = await buildRefund(ctx.hur.seats, utxoOf(stepTx(ctx, 'hur-deploy'), 2), p.refundLockTime, p.refundBal);
    record(ctx, 'refund', `pre-signed 2-of-2 refund, nLockTime ${p.refundLockTime} (sequence 0xfffffffe)`, refund, seatPay);
  });
  await advance(ctx, 'hur-deploy', 'seen');

  await ensure(ctx, 'post', async () => {
    const d = stepTx(ctx, 'hur-deploy');
    const plan = { seq: p.seq, lockTime: p.postLockTime, deadline: p.postLockTime + p.window, bal: p.bal, window: p.window };
    const tx = await buildPost(ctx.hur, w, utxoOf(d, 0), utxoOf(d, 1), plan, p.sdkRate, txs());
    const cont = continuationOf(ctx.hur.art, utxoOf(d, 0), hurState(p, { ...POSTED, seq: p.seq, deadline: plan.deadline }));
    record(ctx, 'post', `HeadsUpReferee.post seq ${p.seq} at its rung (nLockTime ${p.postLockTime}), SINGLE|ANYONECANPAY|FORKID, exact cover`, tx, [cont]);
  });
  await advance(ctx, 'post', 'seen');

  await ensure(ctx, 'act', async () => {
    const escrow = utxoOf(stepTx(ctx, 'post'), 0);
    const st = RunarContract.fromUtxo(ctx.hur.art, escrow).state;
    if (st.phase !== 1n || st.toAct !== 1n || st.deadline !== BigInt(p.postLockTime + p.window)) throw new Error(`post state unexpected: ${JSON.stringify(st, (_k, v) => (typeof v === 'bigint' ? String(v) : v))}`);
    const deadline = p.postLockTime + 2 * p.window;
    const { tx } = await buildAct(ctx.hur, w, escrow, largestCoin(ctx), { bal: p.bal, deadline, seq: p.seq }, rate, policy.satPerKb, txs());
    const cont = continuationOf(ctx.hur.art, escrow, hurState(p, { com0: POSTED.com0, com1: RAISE_TO, seq: p.seq + 1, toAct: 0, deadline }));
    record(ctx, 'act', 'HeadsUpReferee.act: seat 1 raises on chain', tx, [cont, w.script]);
  });
  await advance(ctx, 'act', 'seen');

  await ensure(ctx, 'forfeit', async () => {
    const escrow = utxoOf(stepTx(ctx, 'act'), 0);
    const st = RunarContract.fromUtxo(ctx.hur.art, escrow).state;
    if (st.toAct !== 0n || st.deadline !== BigInt(p.forfeitLockTime)) throw new Error('act state unexpected');
    const tx = await buildForfeit(ctx.hur, w, escrow, largestCoin(ctx), { bal: p.bal, lockTime: p.forfeitLockTime }, rate, txs());
    record(ctx, 'forfeit', `HeadsUpReferee.forfeit at nLockTime ${p.forfeitLockTime} (the deadline): seat 0 missed it`, tx, [...seatPay, w.script]);
  });
  await advance(ctx, 'forfeit', 'seen');

  await advance(ctx, 'refund', 'seen');
}

// ---- funding -------------------------------------------------------------------------------------

async function fetchFunding(ctx: Ctx, txid: string, hexFile?: string): Promise<{ rawHex: string; source: string }> {
  const check = (rawHex: string, source: string) => {
    const tx = Transaction.fromHex(rawHex);
    if (tx.id('hex') !== txid) throw new Error(`funding from ${source} hashes to ${tx.id('hex')}, not ${txid}`);
    return { rawHex: tx.toHex(), source };
  };
  if (hexFile) return check(readFileSync(hexFile, 'utf8').trim(), hexFile);
  const st = await ctx.b.getStatus(txid).catch(() => undefined);
  if (typeof st?.rawTx === 'string') {
    try { return check(Transaction.fromHexEF(st.rawTx).toHex(), `${ctx.b.baseUrl}/tx/${txid} rawTx`); } catch { /* fall through */ }
  }
  const res = await fetch(`${WOC_TESTNET}/tx/${txid}/hex`, { signal: AbortSignal.timeout(15_000) });
  if (!res.ok) throw new Error(`funding ${txid}: WhatsOnChain answered HTTP ${res.status} (unconfirmed transactions can 404 for a while; retry, or pass --funding-hex <file>)`);
  return check((await res.text()).trim(), `${WOC_TESTNET}/tx/${txid}/hex`);
}

function dryRunFunding(w: Wallet): Transaction {
  const tx = new Transaction();
  tx.addInput({ sourceTXID: 'ee'.repeat(32), sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex('51'), sequence: 0xffffffff });
  tx.addOutput({ satoshis: FUNDING_SATS, lockingScript: LockingScript.fromHex(w.script) });
  return tx;
}

// ---- reporting -----------------------------------------------------------------------------------

function table(run: RunFile): string {
  const rows = [['step', 'state', 'txid', 'size B', 'EF B', 'fee', 'max unlock B', 'max lock B', 'nLockTime', 'block', 'blocks to mine', 'flags']];
  for (const id of STEP_IDS) {
    const s = run.steps[id];
    if (!s) { rows.push([id, '-', '', '', '', '', '', '', '', '', '', '']); continue; }
    rows.push([id, s.state, s.txid, String(s.size), String(s.efSize), String(s.fee), String(s.maxUnlockingScript), String(s.maxLockingScript),
      String(s.lockTime), String(s.blockHeight ?? ''), String(s.blocksToMine ?? ''), s.flags.length ? s.flags.map((f) => f.split(':')[0]).join('+') : '']);
  }
  const w = rows[0]!.map((_, i) => Math.max(...rows.map((r) => r[i]!.length)));
  return rows.map((r) => r.map((c, i) => c.padEnd(w[i]!)).join('  ')).join('\n');
}

function summary(ctx: Ctx) {
  const run = ctx.run;
  console.log('\n' + table(run));
  const flags = STEP_IDS.flatMap((id) => (run.steps[id]?.flags ?? []).map((f) => `${id}: ${f}`));
  console.log(flags.length ? `\nFlags:\n  ${flags.join('\n  ')}` : '\nNo size over /policy or the stock defaults.');
  const total = STEP_IDS.reduce((s, id) => s + (run.steps[id]?.fee ?? 0), 0);
  console.log(`Total fees: ${total} sats at ${run.policy?.satPerKb} sat/kB (sized on EF bytes).`);
  const seen = (id: StepId) => run.steps[id]?.state === 'seen' || run.steps[id]?.state === 'mined';
  const all = STEP_IDS.every(seen);
  console.log(`prepare + fraud SEEN: ${seen('prepare') && seen('fraud') ? 'YES' : 'not yet'}; all ${STEP_IDS.length} SEEN with no competitor (gate a): ${all ? 'PASS' : 'not yet'}${run.dryRun ? ' (DRY RUN: simulated chain)' : ''}`);
  if (ctx.fake) {
    const v = ctx.fake.stats();
    console.log(`Dry run: MockProvider ran @bsv/sdk Spend on ${v.validated} inputs, skipped ${v.skipped}; simulated tip ${ctx.fake.tip}.`);
  }
}

// ---- main ----------------------------------------------------------------------------------------

function flag(args: string[], name: string): string | undefined {
  const i = args.indexOf(name);
  if (i < 0) return undefined;
  const v = args[i + 1];
  if (!v || v.startsWith('--')) throw new Error(`${name} needs a value`);
  return v;
}

async function main(argv: string[]) {
  const [cmd = 'help', ...args] = argv;
  const dryRun = args.includes('--dry-run');
  const paths = dryRun ? PATHS.dry : PATHS.real;

  if (cmd === 'address') {
    const { key, created } = loadOrCreateKey(PATHS.real.key);
    const address = key.toPublicKey().toAddress('testnet');
    console.log(created ? `Created ${PATHS.real.key} (mode 0600).` : `Using ${PATHS.real.key}.`);
    console.log(`Gate (a) funding address (testnet): ${address}`);
    console.log(`Next: npm run gate:a -w scripts/gates -- run   (BSV Desktop asks you to approve ${FUNDING_SATS.toLocaleString('en-US')} testnet sats to it)`);
    return;
  }
  if (cmd === 'status') {
    const run = loadRun(paths.run);
    if (!run) throw new Error(`no run file at ${paths.run}`);
    summary({ run } as Ctx);
    return;
  }
  if (cmd !== 'run') {
    console.log('usage: gate-a address | run [--funding <txid>] [--funding-hex <file>] [--dry-run [--fresh]] [--stop-after <step>] | status [--dry-run]');
    process.exitCode = cmd === 'help' ? 0 : 2;
    return;
  }

  const stopAfter = flag(args, '--stop-after') as StepId | undefined;
  if (stopAfter && !STEP_IDS.includes(stopAfter)) throw new Error(`--stop-after must be one of ${STEP_IDS.join(', ')}`);
  if (args.includes('--fresh')) {
    if (!dryRun) throw new Error('--fresh only applies to --dry-run: a funded run file is the wallet and is never discarded');
    rmSync(paths.run, { force: true });
  }

  const { key } = dryRun ? loadOrCreateKey(paths.key) : (() => {
    if (!existsSync(paths.key)) throw new Error(`no ${paths.key}: run \`npm run gate:a -w scripts/gates -- address\` first`);
    return loadOrCreateKey(paths.key);
  })();
  const wallet = await walletOf(key);
  const address = key.toPublicKey().toAddress('testnet');
  // G-02: the key's address must decode as testnet P2PKH and pay the wallet's own script.
  if (p2pkhScript(requireTestnetAddress(address)) !== wallet.script) throw new Error(`refusing: ${address} does not pay the gate key`);
  // G-02, G-09: the funded run broadcasts only to a testnet BSV Broadcaster (GATE_A_BROADCASTER may name the other one).
  const broadcasterUrl = dryRun ? 'https://dry-run.invalid' : requireTestnetBroadcaster(process.env.GATE_A_BROADCASTER ?? TESTNET_BROADCASTER);

  const arts = compileArtifacts();
  const hashes = { WitnessEcMulCheck: scriptHash(arts.chk), GateReferee: scriptHash(arts.ref), HeadsUpReferee: scriptHash(arts.hur) };

  let run = loadRun(paths.run);
  if (run && run.address !== address) throw new Error(`${paths.run} belongs to ${run.address}, not this key's ${address}`);
  if (run && !dryRun) {
    requireTestnetAddress(run.address);
    requireTestnetBroadcaster(run.broadcaster);
    if (run.broadcaster !== broadcasterUrl) throw new Error(`${paths.run} was started on ${run.broadcaster}; resume it there (GATE_A_BROADCASTER)`);
  }
  if (run?.artifacts && JSON.stringify(run.artifacts) !== JSON.stringify(hashes)) {
    throw new Error('the compiled contracts differ from the ones this run started with; refusing to resume with different scripts');
  }
  run ??= { version: 1, dryRun, network: 'testnet', broadcaster: broadcasterUrl, address, createdAt: nowIso(), updatedAt: nowIso(), steps: {} };
  run.artifacts = hashes;

  let fake: FakeBroadcaster | undefined;
  let sleep: (ms: number) => Promise<void>;
  let now: () => number;
  let b: Broadcaster;
  if (dryRun) {
    const p = OBSERVED_POLICY.policy;
    fake = new FakeBroadcaster(run.fakeChain?.tip ?? DRY_RUN_START_TIP, OBSERVED_POLICY, p.miningFee, { maxTxSize: p.maxtxsizepolicy, maxScriptSize: p.maxscriptsizepolicy });
    let t = Date.now();
    now = () => t;
    sleep = async (ms) => { t += ms; fake!.mine(); }; // simulated time: every sleep is one block
    b = createBroadcaster({ baseUrl: broadcasterUrl, fetch: fake.fetch, now, sleep });
  } else {
    sleep = (ms) => new Promise((r) => setTimeout(r, ms));
    now = Date.now;
    b = createBroadcaster({ baseUrl: requireTestnetBroadcaster(broadcasterUrl) });
  }

  const seats = seatsOf(key);
  const runFile = run;
  const ctx: Ctx = {
    run, runPath: paths.run, dryRun, wallet, b, fake, sleep, now, stopAfter, log,
    save: () => {
      if (fake) runFile.fakeChain = { tip: fake.tip };
      saveRun(paths.run, runFile);
    },
    // Chain tip: the broadcaster's GET /health blockHeight, plus WhatsOnChain /chain/info in the funded run.
    tipSources: dryRun ? [] : [wocTip],
    ref: { refArt: arts.ref, body: bodyOf(arts.chk), rec: invalidProof() },
    hur: { art: arts.hur, seats, pk0: await seats[0].pub(), pk1: await seats[1].pub() },
    rate: { satoshis: 1, bytes: 1000 },
  };
  log(`gate (a) ${dryRun ? 'DRY RUN (in-process broadcaster, MockProvider + Spend)' : `on ${b.baseUrl}`}; wallet ${address}; run file ${paths.run}`);

  // Funding.
  if (!run.funding) {
    if (dryRun) {
      const tx = dryRunFunding(wallet);
      run.funding = { txid: tx.id('hex'), rawHex: tx.toHex(), source: 'dry run: synthesized' };
    } else {
      const txid = flag(args, '--funding');
      if (txid) {
        if (!/^[0-9a-f]{64}$/.test(txid)) throw new Error('--funding needs a 64-hex txid');
        const hexFile = flag(args, '--funding-hex');
        const funding = { txid, ...(await fetchFunding(ctx, txid, hexFile)) };
        requireFundingToAddress(Transaction.fromHex(funding.rawHex), txid, address);
        // A file proves nothing about the network: the testnet broadcaster itself must report it.
        if (hexFile) log(`funding ${txid}: the testnet broadcaster reports ${await requireKnownToTestnet(b, txid)}`);
        run.funding = funding;
      } else {
        log(`asking BSV Desktop for ${FUNDING_SATS} testnet sats to ${address}: approve it in the wallet`);
        const f = await fundFromDesktop(address, FUNDING_SATS);
        requireFundingToAddress(Transaction.fromHex(f.rawHex), f.txid, address);
        run.funding = { txid: f.txid, rawHex: f.rawHex, source: `BSV Desktop ${f.version} createAction` };
        ctx.save(); // write-ahead: the run is funded from here on, whatever the broadcaster says next
        // G-13: track the wallet-broadcast funding in our own broadcaster too, and spend it only once SEEN.
        await b.submit(f.efHex).catch((e: unknown) => log(`funding: POST /tx answered ${e instanceof Error ? e.message : String(e)} (BSV Desktop broadcast it too; reading its status)`));
        const st = await b.waitForStatus(f.txid, 'SEEN_ON_NETWORK', 15 * 60_000);
        log(`funding ${f.txid}: ${st.txStatus}, no competing transaction`);
      }
    }
  } else if (flag(args, '--funding') && flag(args, '--funding') !== run.funding.txid) {
    throw new Error(`this run is already funded by ${run.funding.txid}`);
  }
  const coins = requireFundingToAddress(Transaction.fromHex(run.funding.rawHex), run.funding.txid, address);
  ctx.save();
  log(`funding ${run.funding.txid}: ${coins.reduce((s, c) => s + c.satoshis, 0)} sats in ${coins.length} P2PKH output(s) to the testnet gate address (${run.funding.source})`);
  if (fake) {
    fake.seed(Transaction.fromHex(run.funding.rawHex));
    for (const id of STEP_IDS) {
      const s = run.steps[id];
      if (s && (s.state === 'seen' || s.state === 'mined')) fake.restore(s.rawHex, s.state === 'mined' ? { blockHeight: s.blockHeight! } : undefined);
    }
  }

  try {
    await stepPolicy(ctx);
    await gateReferee(ctx);
    await headsUp(ctx);
    log('every transaction SEEN_ON_NETWORK with no competing transaction');
  } catch (e) {
    if (!(e instanceof Stop)) {
      summary(ctx);
      throw e;
    }
    log(e.message);
  }
  summary(ctx);
}

main(process.argv.slice(2)).catch((e: unknown) => {
  console.error(`gate (a): ${e instanceof Error ? e.message : String(e)}`);
  process.exitCode = 1;
});
