// Gate (c), MessageBox half: round-trip latency and fees for the deal messages.
//
//   npm run gate:c -w scripts/gates
//
// Two throwaway ProtoWallet identities (random keys, never saved) exchange messages through the
// MessageBoxClient's default host. A sends a numbered ping with its send time, B echoes it, and the
// round trip is timed at A. Then 5 stored messages are sent, listed and acknowledged.
// Exit code is non-zero on any lost, duplicated or unexpected message, or on a fatal error.
import { PrivateKey, ProtoWallet } from '@bsv/sdk';
import { MessageBoxClient, type PeerMessage } from '@bsv/message-box-client';
import { classifyLiveSamples, liveFallbackVerdict, reconcile, summarise, type LatencySummary, type LiveSample } from './latency-stats.js';

const LIVE_COUNT = Number(process.env.GATE_C_COUNT ?? 50);
const STORED_COUNT = 5;
const ECHO_TIMEOUT_MS = 10_000;
const POLL_MS = 250;
const BOX = 'brc100-poker-gate-c';
const STORED_BOX = 'brc100-poker-gate-c-stored';
const PASS_MEDIAN_MS = 2000;

const now = () => performance.now();
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const fmt = (s: LatencySummary) =>
  `n=${s.n} min=${s.min.toFixed(0)} median=${s.median.toFixed(0)} mean=${s.mean.toFixed(0)} p95=${s.p95.toFixed(0)} max=${s.max.toFixed(0)} ms`;

type Mode = 'live' | 'stored-poll';
interface Wire { kind: 'ping' | 'echo'; seq: number; sentAt: number }

function parse(m: PeerMessage): Wire | undefined {
  let b: unknown = m.body;
  if (typeof b === 'string') { try { b = JSON.parse(b); } catch { return undefined; } }
  const w = b as Partial<Wire>;
  return w && (w.kind === 'ping' || w.kind === 'echo') && Number.isInteger(w.seq) && typeof w.sentAt === 'number' ? (w as Wire) : undefined;
}

interface Side {
  name: string; client: MessageBoxClient; key: string;
  /** While true, every sendMessage call is a fallback from sendLiveMessage (installed/read at MessageBoxClient.js:1443,1447,1468,1492). */
  counting: boolean;
  httpFallbacks: number;
  /** Seqs of the ping/echo sends that fell back, parsed from the HTTP body. */
  fellBackSeqs: Set<number>;
}

async function makeSide(name: string): Promise<Side> {
  const wallet = new ProtoWallet(PrivateKey.fromRandom());
  const client = new MessageBoxClient({ walletClient: wallet as never });
  const key = (await wallet.getPublicKey({ identityKey: true })).publicKey;
  const side: Side = { name, client, key, counting: false, httpFallbacks: 0, fellBackSeqs: new Set() };
  // sendLiveMessage silently falls back to this.sendMessage (message-box-client dist/src/MessageBoxClient.js
  // lines 1443, 1447, 1468, 1492). While `counting` (the live phase only) every sendMessage call is therefore a
  // fallback; record which seq it carried so that sample can be excluded.
  const original = client.sendMessage.bind(client);
  client.sendMessage = ((...args: Parameters<MessageBoxClient['sendMessage']>) => {
    if (side.counting) {
      side.httpFallbacks++;
      try {
        const body = args[0]?.body;
        const w = parse({ body } as PeerMessage);
        if (w) side.fellBackSeqs.add(w.seq);
      } catch { /* unattributed: still counted in httpFallbacks */ }
    }
    return original(...args);
  }) as typeof client.sendMessage;
  return side;
}

async function runLive(a: Side, b: Side) {
  const sent: number[] = [];
  const pingsAtB: number[] = [];
  const echoesAtA: number[] = [];
  const rtts: LiveSample[] = [];
  const oneWayAB: LiveSample[] = [];
  const waiters = new Map<number, () => void>();

  await b.client.listenForLiveMessages({
    messageBox: BOX,
    onMessage: (m) => {
      const w = parse(m); const t = now();
      if (!w || w.kind !== 'ping') return;
      pingsAtB.push(w.seq); oneWayAB.push({ seq: w.seq, rttMs: t - w.sentAt });
      void b.client.sendLiveMessage({ recipient: a.key, messageBox: BOX, body: { kind: 'echo', seq: w.seq, sentAt: w.sentAt } })
        .catch((e) => console.error(`  echo ${w.seq} failed:`, e?.message ?? e));
    },
  });
  await a.client.listenForLiveMessages({
    messageBox: BOX,
    onMessage: (m) => {
      const w = parse(m); const t = now();
      if (!w || w.kind !== 'echo') return;
      echoesAtA.push(w.seq);
      if (!waiters.has(w.seq)) return; // late or duplicate: counted above, no RTT
      rtts.push({ seq: w.seq, rttMs: t - w.sentAt });
      waiters.get(w.seq)!(); waiters.delete(w.seq);
    },
  });
  await sleep(500); // let both rooms settle on the server
  a.counting = true; b.counting = true;

  for (let seq = 0; seq < LIVE_COUNT; seq++) {
    sent.push(seq);
    const done = new Promise<void>((res) => waiters.set(seq, res));
    const sentAt = now();
    await a.client.sendLiveMessage({ recipient: b.key, messageBox: BOX, body: { kind: 'ping', seq, sentAt } });
    const ok = await Promise.race([done.then(() => true), sleep(ECHO_TIMEOUT_MS).then(() => false)]);
    if (!ok) { waiters.delete(seq); console.error(`  seq ${seq}: no echo within ${ECHO_TIMEOUT_MS} ms`); }
  }
  await sleep(1500); // catch stragglers that would show up as duplicates
  a.counting = false; b.counting = false;
  return { sent, pingsAtB, echoesAtA, rtts, oneWayAB };
}

async function runStoredPoll(a: Side, b: Side) {
  const sent: number[] = [];
  const pingsAtB: number[] = [];
  const echoesAtA: number[] = [];
  const rtts: LiveSample[] = [];
  for (let seq = 0; seq < LIVE_COUNT; seq++) {
    sent.push(seq);
    const sentAt = now();
    await a.client.sendMessage({ recipient: b.key, messageBox: BOX, body: { kind: 'ping', seq, sentAt } });
    let got = false;
    while (!got && now() - sentAt < ECHO_TIMEOUT_MS) {
      const atB = await b.client.listMessages({ messageBox: BOX });
      for (const m of atB) {
        const w = parse(m);
        if (w?.kind === 'ping') {
          pingsAtB.push(w.seq);
          await b.client.sendMessage({ recipient: a.key, messageBox: BOX, body: { kind: 'echo', seq: w.seq, sentAt: w.sentAt } });
        }
      }
      if (atB.length) await b.client.acknowledgeMessage({ messageIds: atB.map((m) => m.messageId) });
      const atA = await a.client.listMessages({ messageBox: BOX });
      for (const m of atA) {
        const w = parse(m);
        if (w?.kind === 'echo') { echoesAtA.push(w.seq); if (w.seq === seq) { rtts.push({ seq, rttMs: now() - w.sentAt }); got = true; } }
      }
      if (atA.length) await a.client.acknowledgeMessage({ messageIds: atA.map((m) => m.messageId) });
      if (!got) await sleep(POLL_MS);
    }
    if (!got) console.error(`  seq ${seq}: no echo within ${ECHO_TIMEOUT_MS} ms`);
  }
  return { sent, pingsAtB, echoesAtA, rtts, oneWayAB: [] as LiveSample[] };
}

async function drain(side: Side, box: string): Promise<number> {
  const left = await side.client.listMessages({ messageBox: box });
  if (left.length) await side.client.acknowledgeMessage({ messageIds: left.map((m) => m.messageId) });
  return left.length;
}

async function main() {
  const a = await makeSide('A');
  const b = await makeSide('B');
  const host = (a.client as unknown as { host: string }).host;
  console.log(`MessageBox host (client default): ${host}`);
  console.log(`A identity ${a.key.slice(0, 16)}...  B identity ${b.key.slice(0, 16)}... (throwaway, not saved)`);
  let failed = false;

  // Fees first: a nonzero quote would have made every send below a payment, which ProtoWallet cannot make.
  const quote = await a.client.getMessageBoxQuote({ recipient: b.key, messageBox: BOX });
  console.log('quote (A -> B, live box):', JSON.stringify(quote));
  const quoteStored = await a.client.getMessageBoxQuote({ recipient: b.key, messageBox: STORED_BOX });
  console.log('quote (A -> B, stored box):', JSON.stringify(quoteStored));
  const feeOf = (q: unknown) => {
    const x = q as { deliveryFee?: number; recipientFee?: number };
    return { deliveryFee: x.deliveryFee ?? NaN, recipientFee: x.recipientFee ?? NaN };
  };
  const fees = [feeOf(quote), feeOf(quoteStored)];
  const allFeesZero = fees.every((f) => f.deliveryFee === 0 && f.recipientFee === 0);

  // Live round trip, falling back to stored+poll only if live cannot be established.
  let mode: Mode = 'live';
  let run: Awaited<ReturnType<typeof runLive>> | undefined;
  try {
    run = await runLive(a, b);
  } catch (e) {
    console.error('live mode unavailable:', (e as Error)?.message ?? e);
    mode = 'stored-poll';
  }
  a.counting = false; b.counting = false;
  // Whatever the live run did, every send that fell back to HTTP is a live failure (and not a live sample).
  const liveFallbackCount = a.httpFallbacks + b.httpFallbacks;
  const fellBackSeqs = new Set([...a.fellBackSeqs, ...b.fellBackSeqs]);
  const liveVerdict = liveFallbackVerdict(liveFallbackCount);
  if (!liveVerdict.ok) console.error(`LIVE FAIL: ${liveVerdict.reason} (seqs [${[...fellBackSeqs].sort((x, y) => x - y)}])`);
  if (mode === 'live' && run && classifyLiveSamples(run.rtts, fellBackSeqs).live.length === 0) mode = 'stored-poll';
  if (mode === 'stored-poll') {
    console.log('falling back to stored messages with polling');
    await a.client.disconnectWebSocket().catch(() => {}); await b.client.disconnectWebSocket().catch(() => {});
    // Clear leftover pings/echoes from the failed live attempt so they cannot show up as false duplicates.
    const cleared = (await drain(b, BOX)) + (await drain(a, BOX));
    console.log(`cleared ${cleared} leftover message(s) before the stored-poll run`);
    run = await runStoredPoll(a, b);
  }
  const r = run!;

  const pings = reconcile(r.sent, r.pingsAtB);
  const echoes = reconcile(r.sent, r.echoesAtA);
  const liveRtts = mode === 'live' ? classifyLiveSamples(r.rtts, fellBackSeqs).live : r.rtts;
  const liveOneWay = mode === 'live' ? classifyLiveSamples(r.oneWayAB, fellBackSeqs).live : r.oneWayAB;
  const rtt = liveRtts.length ? summarise(liveRtts.map((x) => x.rttMs)) : undefined;
  console.log(`\n== ${mode} round trip, ${LIVE_COUNT} pings A->B, echoes B->A ==`);
  console.log(`RTT at A (${liveRtts.length} of ${LIVE_COUNT} samples counted${mode === 'live' ? ', fallback samples excluded' : ''}):`, rtt ? fmt(rtt) : 'no samples');
  if (liveOneWay.length) console.log('one way A->B (same process clock):', fmt(summarise(liveOneWay.map((x) => x.rttMs))));
  console.log(`HTTP fallbacks during live run: ${liveFallbackCount}`);
  console.log(`pings:  received ${r.pingsAtB.length}/${LIVE_COUNT}, lost [${pings.lost}], duplicated [${pings.duplicated}], unexpected [${pings.unexpected}]`);
  console.log(`echoes: received ${r.echoesAtA.length}/${LIVE_COUNT}, lost [${echoes.lost}], duplicated [${echoes.duplicated}], unexpected [${echoes.unexpected}]`);
  if (!pings.ok || !echoes.ok) failed = true;

  // Clean up whatever the live run left stored on the server (live sends are also stored).
  await a.client.disconnectWebSocket().catch(() => {}); await b.client.disconnectWebSocket().catch(() => {});
  const leftB = await drain(b, BOX); const leftA = await drain(a, BOX);
  console.log(`stored leftovers acknowledged after the live run: B ${leftB}, A ${leftA}`);

  // Stored messages: send 5, list, acknowledge, list again.
  console.log(`\n== stored messages: ${STORED_COUNT} sent, listed, acknowledged ==`);
  const storedSent: number[] = [];
  const storedLat: number[] = [];
  for (let seq = 0; seq < STORED_COUNT; seq++) {
    storedSent.push(seq);
    const t = now();
    await a.client.sendMessage({ recipient: b.key, messageBox: STORED_BOX, body: { kind: 'ping', seq, sentAt: t } });
    storedLat.push(now() - t);
  }
  const listT = now();
  const listed = await b.client.listMessages({ messageBox: STORED_BOX });
  const listMs = now() - listT;
  const storedSeen = listed.map(parse).filter((w): w is Wire => !!w).map((w) => w.seq);
  const rec = reconcile(storedSent, storedSeen);
  const ackT = now();
  if (listed.length) await b.client.acknowledgeMessage({ messageIds: listed.map((m) => m.messageId) });
  const ackMs = now() - ackT;
  const after = await b.client.listMessages({ messageBox: STORED_BOX });
  console.log(`send (HTTP) ${fmt(summarise(storedLat))}; list ${listMs.toFixed(0)} ms; acknowledge ${ackMs.toFixed(0)} ms`);
  console.log(`listed ${listed.length}/${STORED_COUNT}, lost [${rec.lost}], duplicated [${rec.duplicated}], unexpected [${rec.unexpected}]; after ack ${after.length} remain`);
  if (!rec.ok || after.length !== 0) failed = true;

  console.log('\n== gate (c), MessageBox half ==');
  console.log(`mode: ${mode}`);
  console.log(`live sends: ${liveVerdict.ok ? 'all live (0 HTTP fallbacks)' : `FAIL, ${liveVerdict.reason}`}`);
  console.log(`fees: ${JSON.stringify(fees)} -> ${allFeesZero ? 'all zero' : 'NOT ALL ZERO'}`);
  const medianOk = !!rtt && rtt.median <= PASS_MEDIAN_MS;
  console.log(`median RTT ${rtt ? rtt.median.toFixed(0) : 'n/a'} ms vs ${PASS_MEDIAN_MS} ms -> ${medianOk ? 'ok' : 'FAIL'}`);
  const verdict = !failed && medianOk && allFeesZero && liveVerdict.ok;
  console.log(`delivery: ${failed ? 'FAIL (lost/duplicated)' : 'ok'}; live: ${liveVerdict.ok ? 'ok' : 'FAIL'}; overall: ${verdict ? 'PASS' : 'FAIL'}`);
  await a.client.disconnectWebSocket().catch(() => {}); await b.client.disconnectWebSocket().catch(() => {});
  process.exit(failed || !liveVerdict.ok ? 1 : 0);
}

main().catch((e) => { console.error('fatal:', e); process.exit(2); });
