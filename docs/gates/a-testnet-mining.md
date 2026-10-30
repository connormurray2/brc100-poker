# Gate a: the dispute transactions, accepted on testnet

Status: **PASS (2026-10-30), on the second design.** All eight transaction kinds reached `SEEN_ON_NETWORK` or
later with no competing transaction, within 75 s, paying 692 sats. The first run failed: its 1.85 MB `prepare`
could not be submitted on testnet by any public route. Its checkers were then redesigned so every dispute
transaction fits (Run 1, below).

**Pass rule.** Every transaction kind is final at `SEEN_ON_NETWORK` or later with no competing transaction, read from
BSV Broadcaster `GET /tx/{txid}`, never from the POST (toolkit G-10). Nothing waits for a block (G-14). Gate (a)
passes when all eight are `SEEN`, the `prepare` + `fraud` pair above all.

> The first version of this gate required `MINED`. That contradicted the toolkit (G-10, G-14), which wins. The
> HeadsUpReferee's ladder is now anchored below the tip, so every nLockTime has passed when its transaction is
> built. Maturing on chain (a 476 turning final) is therefore not exercised here.

## Run it

1. BSV Desktop running on **testnet** (the harness refuses any other network, G-02).
2. `npm run gate:a -w scripts/gates -- run`. On the first run the harness:
   - creates `secrets/gate-a.key` (mode 0600, gitignored) if it is missing; only its testnet address is printed;
   - asks BSV Desktop (`WalletClient` over its JSON API on `localhost:3321`) for **9,000 sats** to that address.
     Approve the prompt in the wallet;
   - writes the funding to `secrets/gate-a-run.json`, posts it to its own broadcaster too (G-13), and spends it
     once it is `SEEN_ON_NETWORK`.

   `--funding <txid>` (and `--funding-hex <file>`) fund from P2PKH outputs to the gate address in an existing
   transaction instead.
3. **Resume** with the same command after any interruption. Progress lives in `secrets/gate-a-run.json`: each
   step's txid, raw hex, state (`built`, `unknown`, `submitted`, `seen`), sizes, fee, responses and timestamps,
   fsynced before each atomic rename. **That file is the wallet**: never delete or edit it. A built transaction is
   written down before it is POSTed and is never rebuilt, so a crash cannot create a conflicting second spend. The
   harness refuses to resume a run file with different compiled contracts.
   Broadcaster answers are classified per G-11: transient ones are retried with backoff; an uncertain POST is
   resolved by GET; `REJECTED`, `DOUBLE_SPEND_ATTEMPTED` and an input-blaming 466 on a never-accepted transaction
   stop the run.
4. `npm run gate:a -w scripts/gates -- status` prints the table at any time.
5. `npm run gate:a -w scripts/gates -- run --dry-run --fresh` runs the whole sequence on an in-process broadcaster,
   where `MockProvider` runs `@bsv/sdk` `Spend` on every input (15 of 15) and the fake checks EF source outputs,
   double spends, the fee and the `/policy` limits. `test/gate-a-dry-run.test.ts` pins those refusals.

## What the run does

`scripts/gates/src/gate-a.ts`, with `gate-a/txs.ts` (builders), `gate-a/store.ts` (key, run file, wallet),
`gate-a/desktop.ts` (BSV Desktop funding), `gate-a/fake-broadcaster.ts` (the dry run) and `broadcaster.ts`.

| # | Step | Transaction |
|---|---|---|
| 1 | `/policy` | Read once and stored. Every fee uses its `miningFee`, on the EF size. |
| 2 | `referee-deploy` | A `GateReferee` recording an invalid proof (gate d's pinned vector under domain `brc100-poker/gate-a/v1`, with `z + 1`). 2,000 sats, bond 1,000. |
| 3 | `prepare` | Equation 1. Creates `WitnessEcMulCheck(G, z, L)` and `WitnessEcMulCheck(P1, c, M)` and posts the bond. |
| 4 | `fraud` | Spends the referee and both checkers, each with its 16,929 B slope witness, and returns the bond. |
| 5 | `hur-deploy` | A `HeadsUpReferee` (W = 2, K = 12, two 1,000-sat stacks), the post's exact-cover fee coin and a 2-of-2 refund pot. The ladder's anchor is 3W + 1 blocks below the tip. |
| 5′ | `refund` | Pre-signed by both seats (`ALL\|FORKID`, sequence `0xfffffffe`) before the pot is submitted (G-38). Submitted last. |
| 6 | `post` | The co-signed state `seq 3` at its rung. `SINGLE\|ANYONECANPAY\|FORKID`, exact cover (icellan/runar#176). |
| 7 | `act` | Seat 1 raises to 600 on chain. |
| 8 | `forfeit` | Anyone forfeits seat 0 at the deadline. Payouts pinned; the fee coin's change trails. |
| 9 | `refund` | The pre-signed refund. |

Seats sign as a BRC-100 wallet does (`ProtoWallet.createSignature({ hashToDirectlySign })` under
`[1, 'brc100 poker escrow']`), with keys derived from the gate key. Every output is checked against its expected
script before a transaction is recorded.

## Results

### Run 2: PASS (2026-10-30, 17:07:31–17:08:46 UTC)

Funding: the 6,964-sat change output of run 1's referee deploy (`cfdbb387…aa4e2e:1`), never spent on the network.
`/policy`: 1 sat/kB, `maxtxsizepolicy` and `maxscriptsizepolicy` 100,000,000. Tip 1,766,009; ladder anchor
1,765,984.

| Step | txid | Raw (B) | EF posted (B) | Fee | Largest unlock (B) | `nLockTime` | Status |
|---|---|---|---|---|---|---|---|
| `referee-deploy` | `5504d15ef860f1649f74fb94d0c93d5dadb8fcb25d5fcc4e26f6cd5e113eb59e` | 35,693 | 35,733 | 36 | 106 | 0 | `SEEN_MULTIPLE_NODES` |
| `prepare` | `aa8762a1d7145bd14281e36f89f1d06a3a61611eba7f0d2ab4d96808f9026a95` | 273,496 | 309,038 | 310 | 126,576 | 0 | `SEEN_MULTIPLE_NODES` |
| `fraud` | `d27650327dc6901b6842622ae0f0d11f72737120f7684f7d65eacd83020f22c4` | 140,764 | 287,487 | 288 | 71,042 | 0 | `SEEN_ON_NETWORK`, then `SEEN_MULTIPLE_NODES` |
| `hur-deploy` | `5887c406ce911e1262678f4c3fc7bd6abd2e9d03b3f1a8d7f05cd33e4e0eeb1c` | 4,631 | 4,671 | 5 | 107 | 0 | `SEEN_MULTIPLE_NODES` |
| `post` | `aef1d1bd0cbfb83a97da52322946ee604b12c5ac1dec95416b7f874b1c6c8514` | 13,427 | 17,792 | 21 | 8,901 | 1,766,002 | `SEEN_MULTIPLE_NODES` |
| `act` | `a0e069ae282aeae0c1d012ed581acc370fb2060c8eac73744ad7416595b4b9b6` | 13,388 | 17,753 | 21 | 8,828 | 0 | `SEEN_MULTIPLE_NODES` |
| `forfeit` | `abf62df3c4906a075df9058a805f01342cbbdfaae09ba5e125b3a665853c2f3c` | 4,801 | 9,166 | 10 | 4,499 | 1,766,006 | `SEEN_ON_NETWORK`, then `SEEN_MULTIPLE_NODES` |
| `refund` | `f45f8a10ab0221dd7bd154ce36ef5c68f7b8c2f43289b7bb882275cfdf6a5ebf` | 266 | 352 | 1 | 147 | 1,766,008 | `SEEN_ON_NETWORK`, then `SEEN_MULTIPLE_NODES` |
| **Total** | | | | **692** | | | |

Every status was read from `GET /tx/{txid}` with empty `competingTxs`, and all eight were read again afterwards at
`SEEN_MULTIPLE_NODES`. Each step took 5–11 s from POST to `SEEN`. No answer differed from the toolkit's documented
shapes, and no 476 was returned.

- **One fraud proof** (`prepare` + `fraud`) is 414,260 B raw, 596,525 B posted, 598 sats at 1 sat/kB on EF bytes.
- **No size reaches a stock node's limits**: the largest unlocking script is 126,576 B, against a 500 KB
  `maxscriptsizepolicy`.

### Run 1: FAIL (2026-10-30, 16:13 UTC), with one-`ecMul` checkers

Funded with 9,000 sats from BSV Desktop (`12bcf36a…680e4e26`, `SEEN_ON_NETWORK` 8 s after approval). The
referee deploy (`cfdbb387…aa4e2e`) reached `SEEN_MULTIPLE_NODES`. The `prepare` (`63538c5e…131f186e`) was
1,814,369 B raw and 1,850,051 B posted, with a 924,737 B unlocking script carrying both 427 KB checker templates.
It answered `HTTP 500 {"error":"failed to submit"}` and stayed `RECEIVED`. Its inputs were never spent on the
network; run 2 spent the change. The run file is kept as `secrets/gate-a-run-1-stuck.json`.

Why, all with the identical bytes:

1. **The broadcaster validates it.** A copy with one signature byte flipped is rejected at once with
   `461 TX_INVALID … Signature must be zero for failed CHECK(MULTI)SIG`, exactly as a 35 KB copy is.
2. **The broadcaster cannot pass it on.** In `bsv-blockchain/broadcaster` at `8b48db6`,
   `services/api_server/handlers.go` stores the `RECEIVED` row, then publishes `{txid, raw_tx, input_txids}` to
   Kafka. The publish fails (`"failed to submit"`, line 1026) because `raw_tx` goes base64 in JSON (about
   2.47 MB here) and Sarama's default `Producer.MaxMessageBytes` is 1 MiB. So about 750 KB of posted bytes is
   the real limit, whatever `/policy` says. `arcade-testnet-us-1` answers the same. This is
   bsv-blockchain/broadcaster#329, with a fix in #330 (changes requested 2026-09-01, idle since).
3. **The nodes refuse it too.** `POST <datahub>/tx` on all three datahubs testnet `/health` lists
   (`teranode-eks-testnet-eu-1.bsvb.tech`, `bsva-ovh-seed-testnet-us-1.bsvb.tech:8000`, `testnet.gorillanode.io`)
   answers `413 Request Entity Too Large` from nginx above exactly 1 MiB: a 1,048,000 B body reaches Teranode, and
   a 1,049,000 B body does not. No upstream issue exists.

The `fraud` would have failed the same way: 107 KB raw, but 997 KB posted, because EF repeats each spent checker's
427 KB locking script.

### The fix: `WitnessEcMulCheck`

`contracts/src/WitnessEcMulCheck.runar.ts` proves `out = k·base` by checking instead of computing. The spender
supplies every slope of a right-to-left double-and-add, and the script checks each with a few multiplications mod
p, with no inversions and no branches. The accumulator starts at a fixed point H (x = 2), so all 256 iterations
have the same shape. It is **55,419 B of script plus a 16,929 B witness**, against EcMulCheck's 427,042 B.

- A version with `if` branches compiled to 491,671 B. The size grew faster than the iteration count because each
  unrolled iteration left stack items behind. Branch-free code keeps it at about 206 B per iteration.
- It reads each constructor value once, first, so its script is `push(base) ‖ push(k) ‖ push(out) ‖ body`. That
  lets `GateReferee` bind one 55,416 B body by length and SHA-256, and use it for both checkers on both equations
  (G is the base for equation 1's L). The referee's template shrank from 35,631 B to 34,271 B.
- Tests, all through `Spend` (MockProvider):
  - `contracts/test/witness-ecmul.test.ts`. Accepts honest statements: an arbitrary base, G, k = 1, k = n − 1, and
    8 random ones. Rejects a wrong product, the negated product, a tampered add, double or final slope, a short or
    long witness, k = 0, k = n, and an off-curve base.
  - `contracts/test/gate-d.test.ts`. All 15 gate d cases, rerun on the new checker.
- The witness generator is `ecMulWitness` in `@brc100-poker/protocol`, shared by the tests and gate a.

**Budget for the real referee.** Every referee spend posts the referee's script four times: the code-part push,
the scriptCode inside the preimage, the continuation, and EF's copy of the spent output. Under the ~750 KB relay
limit that leaves the real referee about **130 KB** (from `prepare`; `fraud` allows about 150 KB). Spec §10.6
projected 200 KB.

**Still open upstream.** Raising the two 1 MiB caps (#330 plus `max.message.bytes` in `bsva-infra-flux`, and the
datahubs' nginx `client_max_body_size`) would remove the budget pressure. The design no longer depends on it.

## Scope

- **These are the gate's minimal contracts.** The `prepare` and `fraud` shapes are the ones the real referee will
  use, but the real referee's script is larger and is measured against the 130 KB budget in its own plan. So is
  the dispute reserve (spec §10.6).
- **Not covered here:**
  - a late post carrying its inlined committed move;
  - an uncommitted signed `move`;
  - the real escrow's `refund()` covenant path;
  - a timelocked transaction maturing on chain (every lock time had passed when it was built);
  - mainnet, whose relay limits were not checked.

  The `HeadsUpReferee` shapes are the spike's. The refund here is the pre-signed form, and the covenant-timeout
  form is exercised by `forfeit`, which has the same anyone-can-call, nLockTime-gated, pinned-outputs shape.
- **Money outside the wallet afterwards:**
  - run 2: the referee's phase-2 continuation (2,000 sats; no method spends phase 2), the forfeit payouts
    (2,000 sats) and the refund outputs (1,000 sats) to the seats' derived keys;
  - run 1: the referee deploy's 2,000-sat output, still at phase 0 under the old contract.
