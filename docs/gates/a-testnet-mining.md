# Gate a: the dispute transactions, mined on testnet

Status: **PENDING (funded run)**. The harness is built, and its dry run passes end to end (below). The gate is
decided by the funded run.

**Pass rule.** Gate (a) passes only if the `prepare` + `fraud` pair is **mined** on testnet, with `MINED` read from
BSV Broadcaster `GET /tx/{txid}`, never from the POST. The other transactions are measured and should mine too.
If one of them fails, record it, but it does not fail the gate.

The design rests on these covenant transactions being relayed and mined by real nodes. That includes a `prepare` of
about 1.8 MB, whose referee unlocking script (about 925 KB) is nearly twice a stock node's 500 KB
`maxscriptsizepolicy`. Nobody has broadcast one yet.

## Run it (Connor)

1. **Create the key and print its address:**

   ```bash
   npm run gate:a -w scripts/gates -- address
   ```

   This creates `secrets/gate-a.key` (mode 0600, gitignored) if it is missing, and prints only the testnet address.
   The key is never printed or logged.
2. **Fund it.** From BSV Desktop on testnet, send **3,000,000 sats** to that address, topping up from
   `https://bsvfaucet.com` first if needed.
   - The run spends about 30,000 sats: about 3,000 in fees at 1 sat/kB, and the rest locked in the test contracts.
   - Wait until the payment shows on `https://test.whatsonchain.com`.
3. **Run it:**

   ```bash
   npm run gate:a -w scripts/gates -- run --funding <txid>
   ```

   - The harness reads the funding transaction from BSV Broadcaster (`rawTx`) or from WhatsOnChain testnet
     `/tx/{txid}/hex`. If neither has it, pass `--funding-hex <file>` with its raw hex.
   - It refuses (G-02) unless:
     - the transaction hashes to `<txid>`;
     - the gate address decodes as a testnet P2PKH address (version `0x6f`);
     - the transaction has a P2PKH output to it. Only those outputs are the gate's; the sender's change is never
       touched.
   - Under `--funding-hex` it also refuses unless the testnet broadcaster itself reports the txid
     `SEEN_ON_NETWORK` (no competing transaction) or `MINED`, since a file proves nothing about the network.
   - The broadcaster must be a testnet host: `https://arcade-v2-testnet-us-1.bsvblockchain.tech` (the default) or
     `https://arcade-testnet-us-1.bsvblockchain.tech`, which `GATE_A_BROADCASTER` may name. Any other URL is
     refused, mainnet and ttn included, and a resume must use the host its run file started on.
4. **Resume.** Run `npm run gate:a -w scripts/gates -- run` again, with no flags, after any interruption: a timeout,
   Ctrl-C, a reboot.
   - Progress lives in `secrets/gate-a-run.json`: each step's txid, raw hex, state (`built`, `unknown`,
     `submitted`, `seen`, `mined`), sizes, fee, responses and timestamps. The file is fsynced before each atomic
     rename.
   - **That file is the wallet.** There is no indexer, so never delete or edit it.
   - A built transaction is written down before it is POSTed, and a re-run resubmits those same bytes. It never
     rebuilds a step, so a crash cannot create a conflicting second spend.
   - Broadcaster answers are classified per G-11:
     - **On resume**, a step that was POSTed is read with `GET /tx/{txid}` first. Only a 404 ("never received")
       sends the same bytes again.
     - **Transient**: a network error, a timeout, a 5xx or `UNREACHABLE`. These are retried with backoff
       (5 s, doubling, up to 5 min).
     - **An uncertain POST** (its fetch failed, a 5xx, or `UNREACHABLE`) leaves the step `unknown`, and it is
       resolved by GET, never re-POSTed blindly.
     - **The run stops with a message, and rebuilds nothing,** on:
       - `REJECTED`, including `parent rejected` (resubmit by hand once the ancestor is accepted);
       - `DOUBLE_SPEND_ATTEMPTED`;
       - an input-blaming 466 for a transaction the network never accepted.
     - **An input-blaming rejection of a transaction already `SEEN`** is no verdict (G-11). It is read again, and
       if it persists the run stops, pointing at WhatsOnChain.
   - `npm run gate:a -w scripts/gates -- status` prints the table at any time.
5. **Expected duration.** The run needs:
   - one block for the GateReferee group;
   - one block for the HeadsUpReferee group;
   - then the tip to reach `h0 + 4` (the forfeit) and `h0 + 6` (the refund), where `h0` is the fraud's block.

   That is at least about 7 blocks, so expect an hour or more, since testnet blocks are irregular.
   - Timeouts: 15 min to `SEEN_ON_NETWORK`, 6 h to `MINED`, 12 h for a height. Past any of them the run stops with
     "re-run to resume".
6. **Record the results.** Fill in the results table below from `status`, then set the status line.

## What the run does

The harness is `scripts/gates/src/gate-a.ts`, with:
- `gate-a/txs.ts`, the transaction builders;
- `gate-a/store.ts`, the key, the run file and the wallet;
- `gate-a/fake-broadcaster.ts`, the dry run's broadcaster;
- `broadcaster.ts`, the BSV Broadcaster client, unit-tested in `test/broadcaster.test.ts`.

The steps run in this order:

| # | Step | Transaction | Waits for |
|---|---|---|---|
| 1 | `/policy` | Read once and stored, with the raw body. Every fee uses its `miningFee`. | — |
| 2 | `referee-deploy` | A `GateReferee` recording an invalid proof: gate-d's pinned vector under domain `brc100-poker/gate-a/v1`, with `z + 1`. 2,000 sats, bond 1,000. | `SEEN` |
| 3 | `prepare` | eq 1, the largest shape. Creates `EcMulGenCheck(z, L)` and `EcMulCheck(P1, c, M)` and posts the bond. | `SEEN` |
| 4 | `fraud` | Spends the referee and both checkers, and returns the bond to the gate key. | `SEEN`, then 2–4 all `MINED` |
| 5 | `hur-deploy` | A `HeadsUpReferee` (W = 2, K = 12, two 10,000-sat stacks, `anchor = h0 − 18`), the post's exact-cover fee coin and a 2-of-2 refund pot. | `SEEN` |
| 5′ | `refund` | Pre-signed by both seats (`ALL\|FORKID`, sequence `0xfffffffe`, `nLockTime = h0 + 6`) before the pot is submitted (G-38). Recorded now, submitted last. | — |
| 6 | `post` | The co-signed state `seq 3` at its rung, `nLockTime = h0`. `SINGLE\|ANYONECANPAY\|FORKID`, funded exact cover with no change (icellan/runar#176). | `SEEN` |
| 7 | `act` | Seat 1 raises to 600 on chain. The deadline moves to `h0 + 4`. | `SEEN`, then 5–7 all `MINED` |
| 8 | `forfeit` | Anyone forfeits seat 0 at `nLockTime = h0 + 4`. The payouts are pinned; the fee coin's change is the contract's trailing output. | the height, then `MINED` |
| 9 | `refund` | The pre-signed refund. | the height, then `MINED` |

Notes on the steps:
- **Seats** sign as a BRC-100 wallet does, with `ProtoWallet.createSignature({ hashToDirectlySign })` under
  `[1, 'brc100 poker escrow']`. Their keys are derived from the gate key.
- **Timelocked steps** (`forfeit`, `refund`) are never POSTed before their height. The harness polls the chain tip
  once a minute and POSTs only once the tip is at least the lock time. The tip is the highest of:
  - the broadcaster's `GET /health` `blockHeight`;
  - WhatsOnChain testnet `GET /chain/info` `blocks` (funded run only; the harness already reads WhatsOnChain for
    the funding transaction);
  - the highest block holding one of the run's own transactions.

  If the broadcaster's view lags and it answers ARC 476 anyway, the step records it in `notFinal`, waits a minute,
  and resubmits the identical bytes.
- **Every output is checked** before a transaction is recorded. It must pay exactly the expected script: the gate
  key, a contract script (a continuation or a checker), or a seat payout the contract pins. `act`'s change is set
  to the gate key explicitly. Rúnar SDK warnings are kept in the step's `notes.sdkWarnings` and printed.
- **Broadcasting.** Every transaction goes through `POST /tx` as EF hex (`text/plain`) to
  `https://arcade-v2-testnet-us-1.bsvblockchain.tech`.
  - A step counts as seen only at `SEEN_ON_NETWORK` or later with an empty `competingTxs`.
  - It counts as mined only from a GET that says `MINED`.
  - `REJECTED` and `DOUBLE_SPEND_ATTEMPTED` stop the run.
- **Fees** are sized from the serialized transaction at the `/policy` rate, on the **EF** size, with a floor of
  1 sat.
  - The EF size is what is POSTed, and it is the larger size. The broadcaster's OpenAPI says fee checks are
    delegated to the network, which prices raw bytes, so this overpays: most on `fraud`, whose EF carries both
    427 KB checker scripts (997 sats against 108 for raw).
  - The fee paid by every transaction is recorded.

## `/policy`, read 2026-10-30

`GET https://arcade-v2-testnet-us-1.bsvblockchain.tech/policy` answered:

```json
{"policy":{"miningFee":{"satoshis":1,"bytes":1000},"maxtxsizepolicy":100000000,"maxscriptsizepolicy":100000000,"maxtxsigopscountspolicy":4294967295,"standardFormatSupported":true},"timestamp":"2026-10-30T12:06:36.626896277Z"}
```

This matches the toolkit's documented shape and values: 1 sat/kB, and 100 MB for both size limits. The funded run
reads it again and stores the body it gets.

## Sizes against policy

These are the dry-run sizes. They are deterministic for this harness and match gate d to within a few bytes, from
the longer domain string.

| Item | Bytes | `/policy` (100,000,000) | Stock node default |
|---|---|---|---|
| `prepare`, whole transaction | 1,814,369 | under | under `maxtxsizepolicy` 10 MB |
| `prepare`, referee unlocking script (input 0) | **924,738** | under | **over `maxscriptsizepolicy` 500,000 (1.85×)** |
| `EcMulCheck` locking script (`prepare` output 2) | 427,300 | under | under, 72,700 B headroom |
| `EcMulGenCheck` locking script (`prepare` output 1) | 426,427 | under | under |
| `fraud`, whole transaction / referee unlocking script | 107,317 / 71,323 | under | under |
| `GateReferee` locking script | 35,631 | under | under |
| `HeadsUpReferee` locking script | 4,314 | under | under |

Nothing exceeds the limits `/policy` reports, so nothing was shrunk.

**Flag.** The `prepare` referee unlocking script is over a stock node's 500 KB script policy.
- The toolkit notes that some peers advertise 500,000 B, so it may reach fewer nodes even though BSV Broadcaster
  accepts it.
- That is exactly what the funded run tests: if `prepare` is relayed and mined, a 925 KB unlocking script is
  minable on testnet today.
- If it is refused, the result is a gate failure with that reason. The real referee must then keep every
  unlocking script under 500 KB, for example by creating one checker per `prepare` step, which also drops the
  second template.

## Dry run

```bash
npm run gate:a -w scripts/gates -- run --dry-run --fresh
```

**The dry-run broadcaster.** The dry run drives the same harness and the same `broadcaster.ts`, through EF hex,
`POST /tx`, `GET /tx/{txid}` and `GET /health`, against an in-process broadcaster behind a fake `fetch`. Every
transaction goes through Rúnar's `MockProvider`, which runs `@bsv/sdk` `Spend` on every input. On top of that the
fake broadcaster:
- checks that each EF's claimed source outputs match its chain;
- refuses double spends;
- requires the fee on the EF size at the `/policy` rate;
- applies the `/policy` size limits;
- answers non-final lock times with ARC 476 and persists nothing.

`test/gate-a-dry-run.test.ts` pins each of those refusals. Blocks are mined only when the harness sleeps, one block
per sleep, so `MINED` is learned from a GET, as on testnet. The dry run serves the `/policy` body above. It uses its
own key and run file (`secrets/gate-a-dry-run.*`), and `--fresh` restarts it.

**Result, 2026-10-30.**
- Every transaction was accepted and "mined".
- `MockProvider` ran `Spend` on **15 of 15 inputs**, skipping none.
- The `forfeit` and the `refund` were not POSTed until the simulated tip reached their lock times (the harness
  waited 3 and 1 blocks), and were accepted at it.

| Step | Size (B) | EF (B) | Fee (sats) | Largest unlocking script (B) | Largest locking script (B) | In / out | `nLockTime` |
|---|---|---|---|---|---|---|---|
| `referee-deploy` | 35,834 | 35,874 | 36 | 107 | 35,631 | 1 / 2 | 0 |
| `prepare` | 1,814,369 | 1,850,051 | 1,851 | 924,738 | 427,300 | 2 / 4 | 0 |
| `fraud` | 107,317 | 996,752 | 997 | 71,323 | 35,631 | 4 / 3 | 0 |
| `hur-deploy` | 4,630 | 4,670 | 5 | 106 | 4,314 | 1 / 4 | 0 |
| `post` | 13,426 | 17,791 | 21 | 8,901 | 4,314 | 2 / 1 | h0 |
| `act` | 13,388 | 17,753 | 21 | 8,829 | 4,314 | 2 / 2 | 0 |
| `forfeit` | 4,802 | 9,167 | 10 | 4,500 | 25 | 2 / 3 | h0 + 4 |
| `refund` | 266 | 352 | 1 | 147 | 25 | 1 / 2 | h0 + 6 |
| **Total** | | | **2,942** | | | | |

One fraud proof, `prepare` + `fraud`, is **1,921,686 B** raw and costs 2,848 sats at 1 sat/kB on EF bytes.

**Resume.** The dry run was also stopped after `prepare`, `fraud`, `hur-deploy`, `post` and `act` in turn
(`--stop-after <step>`), and each re-run resumed where the last one stopped.
- To simulate a crash between the write-ahead and the POST, `act` was set back to `built` in the run file.
- The re-run resubmitted the identical bytes (same txid) and finished the sequence.

## Results (funded run)

Fill in from `npm run gate:a -w scripts/gates -- status` and `secrets/gate-a-run.json`.

| Step | txid | Size (B) | Fee (sats) | `SEEN` after POST | Block | Blocks to mine | Result |
|---|---|---|---|---|---|---|---|
| `referee-deploy` | | | | | | | |
| `prepare` | | | | | | | |
| `fraud` | | | | | | | |
| `hur-deploy` | | | | | | | |
| `post` | | | | | | | |
| `act` | | | | | | | |
| `forfeit` | | | | | | | |
| `refund` | | | | | | | |

- `/policy` as read by the run: _(paste `policy.raw`)_
- Fee rate paid: _(sat/kB)_
- ARC 476 answers at height, if the broadcaster lagged (`forfeit`, `refund`): _(from `notFinal`; expected none)_
- Response shapes that differed from the toolkit's documentation: _(none / list)_
- Verdict: _(PASS if `prepare` and `fraud` are both `MINED`)_

## Scope

- **These are the gate's minimal contracts.** The `prepare` and `fraud` shapes are the ones the real referee will
  use. But the full referee's script, about 200 KB with its evaluator, appears three times in each spend, so its
  sizes are re-measured in its own plan. So is the dispute reserve (spec §10.6).
- **Not covered here:**
  - a late post carrying its inlined committed move;
  - an uncommitted signed `move`;
  - the real escrow's `refund()` covenant path.

  The `HeadsUpReferee` shapes are the spike's. The refund here is the pre-signed form, and the covenant-timeout
  form is exercised by `forfeit`, which has the same anyone-can-call, nLockTime-gated, pinned-outputs shape.
- **Money outside the wallet afterwards:**
  - the referee's phase-2 continuation (2,000 sats; no method spends phase 2);
  - the forfeit payouts (20,000 sats) and the refund outputs (2,000 sats), which go to the seats' derived keys.
    Those keys are derivable from the gate key, but the harness does not sweep them.
