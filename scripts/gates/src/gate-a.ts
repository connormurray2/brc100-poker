// Gate (a): mine every dispute transaction kind on BSV testnet.
//
//   npm run gate:a -w scripts/gates -- address                 print the funding address (creates secrets/gate-a.key)
//   npm run gate:a -w scripts/gates -- run --funding <txid>    first run: fund from that transaction
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
//   3. prepare + fraud (eq 1: the ~1.8 MB prepare), each SEEN, then all three MINED;
//   4. deploy a HeadsUpReferee (with the post's exact-cover coin and a 2-of-2 refund pot), post at its
//      rung, act; all MINED; then forfeit once the deadline height has passed;
//   5. the pre-signed nLockTime refund, once its height has passed.
// Gate (a) passes only if the prepare + fraud pair is mined. Every transaction's txid, size, fee,
// status and timestamps are written to the run file before and after it is submitted.
//
// The key is never printed or logged; only its testnet address is.
import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync, rmSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { Hash, LockingScript, Transaction, UnlockingScript, Utils } from '@bsv/sdk';
import { RunarContract } from 'runar-sdk';
import type { RunarArtifact } from 'runar-ir-schema';
import { BroadcastError, TESTNET_BROADCASTER, createBroadcaster, feeFor, type Broadcaster, type FeeRate } from './broadcaster.js';
import {
  STEP_IDS, knownTxs, loadOrCreateKey, loadRun, nowIso, saveRun, utxoOf, walletUtxos,
  type HurParams, type RunFile, type StepId, type StepRecord,
} from './gate-a/store.js';
import {
  FRAUD_EQ, REF_SATS, buildAct, buildForfeit, buildFraud, buildFunded, buildPost,
  buildPrepare, buildRefund, efHexOf, efSizeOf, hurArgs, invalidProof, lookupIn, measure, potScript, refereeLockingScript,
  seatsOf, sizePostCoin, templatesOf, walletOf, type HurKit, type RefereeKit, type Wallet,
} from './gate-a/txs.js';
import { FakeBroadcaster } from './gate-a/fake-broadcaster.js';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..');
const SECRETS = join(ROOT, 'secrets');
const PATHS = {
  real: { key: join(SECRETS, 'gate-a.key'), run: join(SECRETS, 'gate-a-run.json') },
  dry: { key: join(SECRETS, 'gate-a-dry-run.key'), run: join(SECRETS, 'gate-a-dry-run.json') },
};
const FUNDING_SATS = 3_000_000;
const WOC_TESTNET = 'https://api.whatsonchain.com/v1/bsv/test';

// Testnet constants (spec §10.1: W = 2 on testnet, K = 12 per betting segment).
const HUR = { window: 2, maxSeq: 12, seq: 3, bigBlind: 100, bal: 10_000, refundBal: 1_000 };

/** GET /policy as read from the testnet broadcaster on 2026-10-30; the dry run serves this body. */
const OBSERVED_POLICY = {
  policy: { miningFee: { satoshis: 1, bytes: 1000 }, maxtxsizepolicy: 100000000, maxscriptsizepolicy: 100000000, maxtxsigopscountspolicy: 4294967295, standardFormatSupported: true },
  timestamp: '2026-10-30T12:06:36.626896277Z',
};
const DRY_RUN_START_TIP = 1_700_000;

const TIMEOUTS = { seen: 15 * 60_000, mined: 6 * 3600_000, height: 12 * 3600_000, retry: 30 * 60_000 };
const HEIGHT_POLL_MS = 60_000;

const log = (msg: string) => console.log(`[${new Date().toISOString().slice(11, 19)}] ${msg}`);

// ---- context ------------------------------------------------------------------------------------

interface Ctx {
  run: RunFile;
  runPath: string;
  dryRun: boolean;
  wallet: Wallet;
  b: Broadcaster;
  fake?: FakeBroadcaster;
  sleep: (ms: number) => Promise<void>;
  now: () => number;
  ref: RefereeKit;
  hur: HurKit;
  rate: FeeRate;
  stopAfter?: StepId;
}

class Stop extends Error {}

function save(ctx: Ctx) {
  if (ctx.fake) ctx.run.fakeChain = { tip: ctx.fake.tip };
  saveRun(ctx.runPath, ctx.run);
}

function compileArtifacts(): Record<'mul' | 'gen' | 'ref' | 'hur', RunarArtifact> {
  execFileSync('node', [join(ROOT, 'contracts', 'compile.mjs'), 'EcMulCheck', 'EcMulGenCheck', 'GateReferee', 'HeadsUpReferee'], { stdio: 'pipe' });
  const load = (n: string) => JSON.parse(readFileSync(join(ROOT, 'contracts', 'artifacts', `${n}.json`), 'utf8'), (_k, v) =>
    typeof v === 'string' && /^-?\d+n$/.test(v) ? BigInt(v.slice(0, -1)) : v) as RunarArtifact;
  return { mul: load('EcMulCheck'), gen: load('EcMulGenCheck'), ref: load('GateReferee'), hur: load('HeadsUpReferee') };
}

const scriptHash = (a: RunarArtifact) => Utils.toHex(Hash.sha256(Utils.toArray(a.script, 'hex')));

// ---- step machinery ----------------------------------------------------------------------------

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

/** Write-ahead: measure and persist a built transaction before anything is submitted. */
function record(ctx: Ctx, id: StepId, what: string, tx: Transaction, notes?: Record<string, unknown>): StepRecord {
  const txs = knownTxs(ctx.run);
  const policy = ctx.run.policy!;
  const m = measure(tx, lookupIn(txs), ctx.rate, policy);
  if (m.fee < m.required) throw new Error(`${id}: pays ${m.fee} sats, needs ${m.required} for ${m.efSize} EF bytes (builder bug)`);
  const rec: StepRecord = {
    what, txid: tx.id('hex'), rawHex: tx.toHex(), size: m.size, efSize: m.efSize, fee: m.fee, feeRate: ctx.rate,
    feeSizedOn: m.efSize, inputs: tx.inputs.length, outputs: tx.outputs.length, lockTime: tx.lockTime,
    maxUnlockingScript: m.maxUnlockingScript, maxLockingScript: m.maxLockingScript, flags: m.flags, notes,
    state: 'built', builtAt: nowIso(),
  };
  ctx.run.steps[id] = rec;
  save(ctx);
  log(`${id}: built ${rec.txid} — ${m.size} B (EF ${m.efSize} B), fee ${m.fee} sats, largest unlocking script ${m.maxUnlockingScript} B`);
  for (const f of m.flags) log(`${id}: FLAG ${f}`);
  return rec;
}

async function ensure(ctx: Ctx, id: StepId, build: () => Promise<void>) {
  if (!ctx.run.steps[id]) await build();
}

/** The best tip we know: /health blockHeight, or at least the highest block one of our transactions is in. */
async function tipNow(ctx: Ctx): Promise<{ tip?: number; source: string }> {
  const health = await ctx.b.tipHeight();
  const mined = Math.max(-1, ...STEP_IDS.map((id) => ctx.run.steps[id]?.blockHeight ?? -1));
  if (health !== undefined && health >= mined) return { tip: health, source: '/health blockHeight' };
  if (mined >= 0) return { tip: mined, source: 'highest block holding a run transaction (lower bound)' };
  return { source: 'unknown' };
}

const isTimelocked = (tx: Transaction) => tx.lockTime > 0 && tx.lockTime < 500_000_000 && tx.inputs.some((i) => (i.sequence ?? 0xffffffff) !== 0xffffffff);

async function submitStep(ctx: Ctx, id: StepId, rec: StepRecord) {
  const tx = Transaction.fromHex(rec.rawHex);
  const ef = efHexOf(tx, knownTxs(ctx.run));
  const timelocked = isTimelocked(tx);
  const deadline = ctx.now() + (timelocked ? TIMEOUTS.height : TIMEOUTS.retry);
  for (;;) {
    const { tip, source } = await tipNow(ctx);
    // One early attempt is allowed (it records the network refusing a non-final transaction, ARC
    // 476, which persists nothing); after that, wait for the height before submitting again.
    if (timelocked && tip !== undefined && tip < rec.lockTime && (rec.notFinal?.length ?? 0) > 0) {
      if (ctx.now() + HEIGHT_POLL_MS > deadline) throw new Error(`${id}: height ${rec.lockTime} not reached (tip ${tip}); re-run to resume`);
      log(`${id}: waiting for height ${rec.lockTime} (tip ${tip}, ${source})`);
      await ctx.sleep(HEIGHT_POLL_MS);
      continue;
    }
    try {
      log(`${id}: POST /tx ${rec.txid} (${ef.length / 2} EF bytes)${timelocked ? `, nLockTime ${rec.lockTime}, tip ${tip ?? '?'}` : ''}`);
      rec.submitResponse = await ctx.b.submit(ef);
      Object.assign(rec, { state: 'submitted', submittedAt: nowIso(), tipAtSubmit: tip, tipSource: source });
      save(ctx);
      log(`${id}: POST answered ${rec.submitResponse.status} (provisional; waiting for GET /tx)`);
      return;
    } catch (e) {
      if (!(e instanceof BroadcastError) || !e.retryable) throw e;
      if (e.notFinal) {
        (rec.notFinal ??= []).push({ at: nowIso(), tip, message: e.message });
        save(ctx);
        log(`${id}: not final yet (ARC 476): ${e.message}`);
      } else {
        log(`${id}: retryable failure: ${e.message}`);
      }
      if (ctx.now() + HEIGHT_POLL_MS > deadline) throw new Error(`${id}: still not accepted (${e.message}); re-run to resume`);
      await ctx.sleep(HEIGHT_POLL_MS);
    }
  }
}

/** Takes a recorded step to SEEN (no competing transaction) or MINED. Resumes from whatever state it is in. */
async function advance(ctx: Ctx, id: StepId, until: 'seen' | 'mined') {
  const rec = ctx.run.steps[id]!;
  if (rec.state === 'mined' || (until === 'seen' && rec.state === 'seen')) return;
  if (rec.state !== 'built') {
    // Resuming: only resubmit if this broadcaster has no record (404). Submission is idempotent anyway.
    const st = await ctx.b.getStatus(rec.txid).catch(() => undefined);
    if (st === undefined) rec.state = 'built';
  }
  if (rec.state === 'built') await submitStep(ctx, id, rec);
  if (rec.state === 'submitted') {
    const st = await ctx.b.waitForStatus(rec.txid, 'SEEN_ON_NETWORK', TIMEOUTS.seen);
    Object.assign(rec, { state: 'seen', seenAt: nowIso(), seenStatus: st });
    save(ctx);
    log(`${id}: ${st.txStatus}, no competing transaction`);
  }
  if (until === 'mined' && rec.state === 'seen') {
    const st = await ctx.b.waitForStatus(rec.txid, 'MINED', TIMEOUTS.mined);
    const { merklePath: _m, ...kept } = st;
    Object.assign(rec, {
      state: 'mined', minedAt: nowIso(), minedStatus: kept, blockHeight: st.blockHeight, blockHash: st.blockHash,
      blocksToMine: st.blockHeight !== undefined && rec.tipAtSubmit !== undefined ? st.blockHeight - rec.tipAtSubmit : undefined,
    });
    save(ctx);
    log(`${id}: MINED in block ${st.blockHeight} (${rec.blocksToMine ?? '?'} block(s) after submit)`);
  }
  if (ctx.stopAfter === id) throw new Stop(`stopped after ${id} (--stop-after); re-run to resume`);
}

// ---- the sequence --------------------------------------------------------------------------------

async function stepPolicy(ctx: Ctx) {
  if (!ctx.run.policy) {
    const p = await ctx.b.readPolicy();
    ctx.run.policy = { ...p, readAt: nowIso(), source: ctx.dryRun ? `dry run: the body read from ${TESTNET_BROADCASTER}/policy on 2026-10-30` : `${ctx.b.baseUrl}/policy` };
    save(ctx);
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
    record(ctx, 'referee-deploy', 'GateReferee deploy, recording an invalid proof (z + 1)', tx, { refereeLockingScript: tx.outputs[0]!.lockingScript.toBinary().length });
  });
  await advance(ctx, 'referee-deploy', 'seen');

  await ensure(ctx, 'prepare', async () => {
    const ref = utxoOf(stepTx(ctx, 'referee-deploy'), 0);
    const tx = await buildPrepare(ctx.ref, w, src(), rate, ref, largestCoin(ctx));
    record(ctx, 'prepare', `GateReferee.prepare (eq ${FRAUD_EQ}): creates EcMulGenCheck and EcMulCheck`, tx, {
      refereeUnlock: tx.inputs[0]!.unlockingScript!.toBinary().length,
      checkerLockingScripts: [tx.outputs[1]!.lockingScript.toBinary().length, tx.outputs[2]!.lockingScript.toBinary().length],
    });
  });
  await advance(ctx, 'prepare', 'seen');

  await ensure(ctx, 'fraud', async () => {
    const prep = stepTx(ctx, 'prepare');
    const tx = await buildFraud(ctx.ref, w, src(), rate, utxoOf(prep, 0), [utxoOf(prep, 1), utxoOf(prep, 2)], largestCoin(ctx));
    record(ctx, 'fraud', 'GateReferee.fraud: spends the referee and both checkers, returns the bond', tx, {
      refereeUnlock: tx.inputs[0]!.unlockingScript!.toBinary().length,
    });
  });
  await advance(ctx, 'fraud', 'seen');

  for (const id of ['referee-deploy', 'prepare', 'fraud'] as const) await advance(ctx, id, 'mined');
  log('GATE (a) PASS CONDITION: the prepare + fraud pair is MINED (each read from GET /tx/{txid})');
}

function hurParams(ctx: Ctx): Omit<HurParams, 'sdkRate' | 'postCoinSats'> {
  const h0 = ctx.run.steps.fraud!.blockHeight!;
  const { window: W, maxSeq, seq } = HUR;
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
    const base = hurParams(ctx);
    const escrowScript = new RunarContract(ctx.hur.art, hurArgs(ctx.hur, base)).getLockingScript();
    const plan = { seq: base.seq, lockTime: base.postLockTime, deadline: base.postLockTime + base.window, bal: base.bal, window: base.window };
    const { coinSats, sdkRate } = await sizePostCoin(ctx.hur, w, escrowScript, plan, rate, policy.satPerKb);
    // The refund pot pays the refund's own fee: size it on a dummy refund with the longest signatures.
    const pot = potScript(ctx.hur.pk0, ctx.hur.pk1);
    const dummyPot = { txid: 'b0'.repeat(32), outputIndex: 2, satoshis: 0, script: pot };
    const dummy = await buildRefund(ctx.hur.seats, dummyPot, base.refundLockTime, base.refundBal, true);
    const potSats = 2 * base.refundBal + feeFor(efSizeOf(dummy, () => dummyPot), rate);
    ctx.run.hur = { ...base, sdkRate, postCoinSats: coinSats };
    save(ctx);

    const tx = await buildFunded(w, largestCoin(ctx), [
      { script: escrowScript, satoshis: 2 * base.bal },
      { script: w.script, satoshis: coinSats }, // the post's exact-cover fee coin
      { script: pot, satoshis: potSats },
    ], rate, lookupIn(txs()));
    record(ctx, 'hur-deploy', 'HeadsUpReferee deploy, plus the post\'s exact-cover coin and a 2-of-2 refund pot', tx, { params: ctx.run.hur });
  });
  const p = ctx.run.hur!;
  // The refund is signed over the pot's txid before the pot is submitted (G-38), and recorded now;
  // it is submitted last, once its height has passed.
  await ensure(ctx, 'refund', async () => {
    const refund = await buildRefund(ctx.hur.seats, utxoOf(stepTx(ctx, 'hur-deploy'), 2), p.refundLockTime, p.refundBal);
    record(ctx, 'refund', `pre-signed 2-of-2 refund, nLockTime ${p.refundLockTime} (sequence 0xfffffffe)`, refund);
  });
  await advance(ctx, 'hur-deploy', 'seen');

  await ensure(ctx, 'post', async () => {
    const d = stepTx(ctx, 'hur-deploy');
    const plan = { seq: p.seq, lockTime: p.postLockTime, deadline: p.postLockTime + p.window, bal: p.bal, window: p.window };
    const tx = await buildPost(ctx.hur, w, utxoOf(d, 0), utxoOf(d, 1), plan, p.sdkRate, txs());
    record(ctx, 'post', `HeadsUpReferee.post seq ${p.seq} at its rung (nLockTime ${p.postLockTime}), SINGLE|ANYONECANPAY|FORKID, exact cover`, tx);
  });
  await advance(ctx, 'post', 'seen');

  await ensure(ctx, 'act', async () => {
    const escrow = utxoOf(stepTx(ctx, 'post'), 0);
    const st = RunarContract.fromUtxo(ctx.hur.art, escrow).state;
    if (st.phase !== 1n || st.toAct !== 1n || st.deadline !== BigInt(p.postLockTime + p.window)) throw new Error(`post state unexpected: ${JSON.stringify(st, (_k, v) => (typeof v === 'bigint' ? String(v) : v))}`);
    const { tx } = await buildAct(ctx.hur, w, escrow, largestCoin(ctx), { bal: p.bal, deadline: p.postLockTime + 2 * p.window, seq: p.seq }, rate, policy.satPerKb, txs());
    record(ctx, 'act', 'HeadsUpReferee.act: seat 1 raises on chain', tx);
  });
  await advance(ctx, 'act', 'seen');

  for (const id of ['hur-deploy', 'post', 'act'] as const) await advance(ctx, id, 'mined');

  await ensure(ctx, 'forfeit', async () => {
    const escrow = utxoOf(stepTx(ctx, 'act'), 0);
    const st = RunarContract.fromUtxo(ctx.hur.art, escrow).state;
    if (st.toAct !== 0n || st.deadline !== BigInt(p.forfeitLockTime)) throw new Error('act state unexpected');
    const tx = await buildForfeit(ctx.hur, w, escrow, largestCoin(ctx), { bal: p.bal, lockTime: p.forfeitLockTime }, rate, txs());
    record(ctx, 'forfeit', `HeadsUpReferee.forfeit at nLockTime ${p.forfeitLockTime} (the deadline): seat 0 missed it`, tx);
  });
  await advance(ctx, 'forfeit', 'mined');

  await advance(ctx, 'refund', 'mined');
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
  const passed = run.steps.prepare?.state === 'mined' && run.steps.fraud?.state === 'mined';
  const all = STEP_IDS.every((id) => run.steps[id]?.state === 'mined');
  console.log(`prepare + fraud mined: ${passed ? 'YES' : 'not yet'}; every transaction mined: ${all ? 'YES' : 'not yet'}${run.dryRun ? ' (DRY RUN: simulated chain)' : ''}`);
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
    console.log(`Send ${FUNDING_SATS.toLocaleString('en-US')} sats from BSV Desktop on testnet, then: npm run gate:a -w scripts/gates -- run --funding <txid>`);
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
  if (!/^[mn]/.test(address)) throw new Error(`refusing: ${address} is not a testnet address (G-02)`);

  const arts = compileArtifacts();
  const hashes = { EcMulCheck: scriptHash(arts.mul), EcMulGenCheck: scriptHash(arts.gen), GateReferee: scriptHash(arts.ref), HeadsUpReferee: scriptHash(arts.hur) };

  let run = loadRun(paths.run);
  if (run && run.address !== address) throw new Error(`${paths.run} belongs to ${run.address}, not this key's ${address}`);
  if (run?.artifacts && JSON.stringify(run.artifacts) !== JSON.stringify(hashes)) {
    throw new Error('the compiled contracts differ from the ones this run started with; refusing to resume with different scripts');
  }
  run ??= { version: 1, dryRun, network: 'testnet', broadcaster: TESTNET_BROADCASTER, address, createdAt: nowIso(), updatedAt: nowIso(), steps: {} };
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
    b = createBroadcaster({ baseUrl: 'https://dry-run.invalid', fetch: fake.fetch, now, sleep });
  } else {
    sleep = (ms) => new Promise((r) => setTimeout(r, ms));
    now = Date.now;
    b = createBroadcaster({ baseUrl: TESTNET_BROADCASTER });
  }
  if (!dryRun && b.baseUrl !== TESTNET_BROADCASTER) throw new Error('refusing: gate (a) broadcasts to the testnet BSV Broadcaster only (G-02, G-09)');

  const seats = seatsOf(key);
  const ctx: Ctx = {
    run, runPath: paths.run, dryRun, wallet, b, fake, sleep, now, stopAfter,
    ref: { refArt: arts.ref, tmpl: templatesOf(arts.mul, arts.gen), rec: invalidProof() },
    hur: { art: arts.hur, seats, pk0: await seats[0].pub(), pk1: await seats[1].pub() },
    rate: { satoshis: 1, bytes: 1000 },
  };
  log(`gate (a) ${dryRun ? 'DRY RUN (in-process broadcaster, MockProvider + Spend)' : `on ${TESTNET_BROADCASTER}`}; wallet ${address}; run file ${paths.run}`);

  // Funding.
  if (!run.funding) {
    if (dryRun) {
      const tx = dryRunFunding(wallet);
      run.funding = { txid: tx.id('hex'), rawHex: tx.toHex(), source: 'dry run: synthesized' };
    } else {
      const txid = flag(args, '--funding');
      if (!txid || !/^[0-9a-f]{64}$/.test(txid)) throw new Error('first run needs --funding <txid> (the BSV Desktop payment to the gate address)');
      run.funding = { txid, ...(await fetchFunding(ctx, txid, flag(args, '--funding-hex'))) };
    }
    const paid = Transaction.fromHex(run.funding.rawHex).outputs.filter((o) => o.lockingScript.toHex() === wallet.script);
    if (paid.length === 0) throw new Error(`funding ${run.funding.txid} pays nothing to ${address}`);
    save(ctx);
    log(`funding ${run.funding.txid}: ${paid.reduce((s, o) => s + o.satoshis!, 0)} sats to the gate address (${run.funding.source})`);
  } else if (flag(args, '--funding') && flag(args, '--funding') !== run.funding.txid) {
    throw new Error(`this run is already funded by ${run.funding.txid}`);
  }
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
    log('every transaction MINED');
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
