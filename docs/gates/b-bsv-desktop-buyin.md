# Gate b: BSV Desktop and the foreign-input buy-in

Status: **PASS** (2026-10-30). Connor ran the page against BSV Desktop on testnet. The wallet kept the foreign
input first and the pinned outputs in order, and BSV Broadcaster saw the buy-in with no competitor. Spec 10.5's
pinned-prefix rule stands as written.

Every buy-in and top-up spends the table escrow (a foreign input) next to the player's own coins, and the
escrow's covenant pins the first outputs by position (spec 10.5: a prefix compare on the serialised
outputs). If BSV Desktop reorders the inputs or outputs, that design needs its fallback. This gate asks the
wallet for exactly that transaction and reports whether the order survived.

## What was built

- `scripts/gates/buyin/checks.ts`: pure checks. Given the final `Transaction`, the expected foreign outpoint and
  the expected pinned outputs, it returns `{ fact, pass, detail }` lines. Unit-tested on hand-built
  transactions in `scripts/gates/test/buyin-checks.test.ts` (good order, swapped outputs, foreign input moved
  or missing, extra outputs inside and after the prefix, wrong continuation amount).
- `scripts/gates/buyin/main.ts` and `index.html`: the page. It only wires the wallet calls to the checks and
  renders the lines.
- `scripts/gates/buyin/vite.config.ts`: Vite 8, bound to `127.0.0.1` only. The recorded run used a static build of
  the same page on Vercel instead.

## How to run (Connor)

1. Start BSV Desktop, set it to **testnet**, and make sure it holds at least about 2,000 testnet sats.
2. From the repo root: `npm run gate:b -w scripts/gates`
3. Open **http://127.0.0.1:5173/** in a browser on the same machine.
4. Note the page's test address. Its key is in memory only: do not reload mid-run, or the 1,000-sat test escrow
   is lost with it.
5. Click **Run the buy-in test** and approve **two** wallet prompts:
   - the 1,000-sat test escrow (an ordinary payment to the page's test address);
   - the buy-in: spends that escrow plus some of your coins, and pays a 2,800-sat continuation and a 1-sat marker.
6. Wait for the **Facts** list and the **Broadcaster status** line (it polls `GET /tx/{txid}` on
   `https://arcade-v2-testnet-us-1.bsvblockchain.tech` for up to about 60 s, treating 404 and `UNKNOWN` as "not yet").
   If the browser cannot reach the broadcaster, run the `curl` the page prints.
   The page cannot see who broadcast the transaction. Note whether BSV Desktop did it itself, and through what
   (its settings or logs), or check the broadcaster for the txid.
7. Copy the block under "Copy into docs/gates/b-bsv-desktop-buyin.md" into the results below.

If the wallet reports a network other than testnet, the page refuses and shows an error. Nothing is sent.

## Results

Run on 2026-10-30 at 15:47:43Z, from the page deployed at `https://brc100-poker-gate-b.vercel.app` (the same
build as `npm run gate:b`, served over HTTPS instead of from `127.0.0.1`). BSV Desktop accepted calls from that
origin.

| Item | Value |
| --- | --- |
| Date | 2026-10-30T15:47:43Z |
| BSV Desktop version | `wallet-brc100-1.0.0` (as the wallet reports it) |
| Escrow txid:vout | `4c8c8861fb50e44a63388d3c90d84cfbf91655ddd55c50ea7ddd843344df9787:0` |
| Buy-in txid | `0405af3266bad3507f7d4f96fa10f5c0efffbde52e4ce587d1190129c9efe8cb` |
| Broadcaster status of the buy-in txid | PASS. `SEEN_ON_NETWORK` at 15:47:41Z, no competing transactions; `SEEN_MULTIPLE_NODES` at 15:47:50Z |
| Signed transaction is the one `signAction` returned | PASS. 2 inputs and 3 outputs unchanged |
| BSV Desktop broadcast it itself, and through what | BSV Desktop: the page only reads the broadcaster, so the wallet's `signAction` sent it. The route it used was not noted. |
| Foreign input is at index 0 | PASS. The escrow outpoint is input 0 of 2 |
| Pinned outputs first and in order (continuation, then marker) | PASS. Outputs 0 and 1 |
| Extra outputs, listed | One, after the prefix: #2, 4,206 sats of wallet change to `76a91478e4df…40d788ac` |
| Continuation carries 2,800 sats | PASS |
| Order as first built (before signing) matches the final | PASS. Every fact passes on the unsigned transaction too |

Checked again from the broadcaster's raw transaction with `@bsv/sdk`: inputs are the escrow `:0`, then the
wallet's `16d26c00…1c5582:1`; outputs are 2,800, 1 and 4,206 sats.

The wallet paid a 103-sat fee on 408 bytes, about 252 sat/kB. That is above both the broadcaster's `/policy`
rate (1 sat/kB) and the 100 sat/kB the toolkit records as BSV Desktop's default (spec 9, the fee rate).

## Decision rule

- **Every fact PASSES** (the four ordering facts, the signed transaction equal to the returned one, and the broadcaster at
  `SEEN_ON_NETWORK`, `SEEN_MULTIPLE_NODES`, `STUMP_PROCESSING`, `MINED` or `IMMUTABLE` with no competing
  transactions, the same set gate (a) accepts): gate (b) passes. The
  pinned-prefix rule in 10.5 stands as written. Record the version, txid and facts above and change the
  status to **PASS**.
- **Extra outputs after the pinned prefix** (change, a wallet commission) are fine: the covenant ignores
  trailing outputs. They only fail the fact if they sit inside the prefix.
- **Any ordering fact FAILS** (foreign input not first, a pinned output moved, an extra output inside the prefix):
  record it exactly as shown, set the status to **FAIL (ordering not preserved)**, and the design's
  pinned-prefix rule needs the fallback noted in spec 10.5 before any buy-in code is written. Note that
  10.5 as written states the assumption but does not spell out the fallback, so naming it is part of acting on a FAIL.
- **The continuation amount is not 2,800** with the order intact: the wallet changed an output. Record it and treat it
  as a FAIL for the same reason.
- **Ordering passes but the signed-transaction or network fact fails**: the page's verdict is FAIL. An altered
  transaction means the signature did not cover what was broadcast. A network failure may only be slow
  propagation: check the txid on the broadcaster by hand and rerun once before concluding anything.
- **The run itself fails** (wallet error, `signAction` rejected, transaction not broadcast): that is not a pass
  or a fail on ordering. Record the error text, and rerun once before concluding anything.

## Notes on the page

- Test keys: a fresh random `PrivateKey`, kept in memory only. Never in `localStorage`, never logged, never in
  the URL. A second random key receives the 1-sat marker and is discarded, so that sat is spent for good.
- Cost. The wallet spends about 2,801 sats plus fees in total (the 1,000-sat escrow, then the buy-in's
  1,800-odd sats of its own coins). The 2,800-sat continuation lands on the page's test key, and the marker sat and
  fees are gone for good. The continuation is recoverable only while the page is open; a reload loses it, so the
  worst case is about 2,800 testnet sats.
- The wallet chooses the player's coins. The page sets the outputs (2,801 sats) and a 1,000-sat foreign input,
  so the wallet must add at least about 1,800 sats plus the fee. The "2,000 sats of the player's coins" in the task is therefore
  nominal.
