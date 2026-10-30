# Phase-1 Gates Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Settle the five go/no-go gates of phase 1 (spec §15) with measured, committed evidence, building the reusable foundations they need (workspace, vendored Rúnar, card encoding, Chaum–Pedersen proofs) along the way.

**Architecture:**
- **One npm workspace at the repo root**, beside the existing Go module, with three members:
  - `packages/protocol`: pure TypeScript primitives;
  - `contracts`: Rúnar sources, vendored Rúnar, a compile script and real-`Spend` tests;
  - `scripts/gates`: harnesses that talk to testnet, BSV Desktop and MessageBox.
- **A Go program** generates evaluator vectors from the existing `internal/game/eval`.
- **Each gate ends in a short results file** under `docs/gates/`, recording pass or fail and its numbers.

**Tech Stack:**
- TypeScript (Node ≥ 22), vitest, `@bsv/sdk` `^2.0.7` (the version `runar-sdk` depends on, so only one copy is installed);
- Rúnar `1.0.0-rc.1` built from upstream source and vendored as tarballs;
- `@bsv/message-box-client`;
- Go 1.26 for vector generation;
- Vite for the one browser page (gate b).

**Spec:** `docs/superpowers/specs/2026-10-29-p2p-poker-design.md` (draft 6, approved 2026-10-30).

**Out of scope:** the full table escrow, dispute referee, client, lobby and watcher, and retiring the Go table (spec §17). Each gets its own plan after the gates pass. The gate contracts here are minimal, but their checker, `prepare` and `fraud` shapes are the ones the real referee will use.

## Global Constraints

- Rúnar is built from upstream `icellan/runar` source and vendored. Never install `runar-*` from npm: 0.4.6 has a forgeable `checkPreimage` (G-25).
- Every vendored Rúnar build records its exact commit(s) in `contracts/vendor/runar/PROVENANCE.md`.
- Every contract path is tested through `@bsv/sdk` `Spend`. Rúnar's own interpreter (`TestContract`) agreeing proves nothing (spec §13.7; icellan/runar#175, #178).
- Never name a Rúnar local, parameter or property `t<digits>` (icellan/runar#175).
- Testnet only. Money paths refuse unless `getNetwork()` returns `testnet` (G-02).
- Broadcast only through BSV Broadcaster `POST /tx`, as EF when sources are attached. Count a transaction at `SEEN_ON_NETWORK` with no competing transaction (G-09, G-10).
  - Testnet BSV Broadcaster: `https://arcade-v2-testnet-us-1.bsvblockchain.tech`.
- Card *i* is the first `02 ‖ SHA-256("brc100-poker/card/v1/" ‖ i ‖ "/" ‖ t)` for `t = 0, 1, …` that decodes to a canonical point.
  - `i` and `t` are written as ASCII decimal.
  - Card index = `(rank − 2) · 4 + suit` (`internal/game/cards` `Card.Index()`).
- Every received point is 33 bytes, prefix 02/03, with x < p checked **before** parsing, on curve, and not infinity.
- Every hashed proof statement uses **64-byte affine points** (`x ‖ y`, big-endian).
- Proof nonces are `HMAC-SHA256(secret, "nonce" ‖ SHA-256(statement))`. In production the secret comes from `createHmac`; tests pass a fixed 32-byte secret.
- Secrets never go in the repo. The funded gate key lives in `secrets/` (gitignored).

## Review Focus

Most likely first:

1. **A point whose x ≥ p** (e.g. `02` + `ff…ff`) must be rejected, not silently reduced. Task 2 pins it.
2. **A proof statement re-encoded with 33-byte points** must give a different challenge from the canonical 64-byte one, so off-chain and on-chain `c` can never silently agree on the wrong bytes. Task 3 pins it.
3. **A `fraud` claim against an honest proof** must be refused. A true statement can always create its checker outputs, so only the final inequality decides. Task 5 pins it.
4. **Hands with ties and wheels** (A-2-3-4-5, two trips, three pairs, a six-card flush) are where evaluators diverge. Task 6 includes them besides the random corpus.
5. **A broadcast answered 2xx but later `REJECTED` or 404**, and a testnet that mines slowly: gate (a) must keep polling and never record "mined" from the POST alone. Task 9 pins it.

---

### Task 1: Workspace and vendored Rúnar

**Files:**
- Create: `package.json` (root; `"private": true`, `"workspaces": ["packages/*", "contracts", "scripts/gates"]`, `"overrides"` pointing every `runar-*` name at `contracts/vendor/runar/*.tgz`)
- Create: `contracts/package.json`, `contracts/compile.mjs`, `contracts/vendor/runar/PROVENANCE.md`, `contracts/vendor/runar/*.tgz`
- Create: `packages/protocol/package.json`, `packages/protocol/tsconfig.json`
- Create: `scripts/gates/package.json`
- Modify: `.gitignore` (add `node_modules/`, `contracts/artifacts/`)
- Test: `contracts/test/toolchain.test.ts`

**Interfaces:**
- Produces: `node contracts/compile.mjs <Name>` reads `contracts/src/<Name>.runar.ts` and writes `contracts/artifacts/<Name>.json`. It prints script bytes and fails on any compiler error. BigInts are serialised as `"<n>n"` strings.
- Produces: `loadArtifact(name: string): RunarArtifact` in `contracts/test/helpers.ts`, which reverses that encoding.

- [ ] **Step 1:** Build Rúnar at upstream `main` (`d207ee8` or newer) with `pnpm install --frozen-lockfile && pnpm build`.
- [ ] **Step 2:** Run `pnpm pack` in `packages/runar-{lang,compiler,ir-schema,sdk,testing}` and copy the tarballs to `contracts/vendor/runar/`.
- [ ] **Step 3:** Write `PROVENANCE.md`: the commit, the rebuild steps, and the rule that `runar-*` never comes from npm.
- [ ] **Step 4:** Write the failing test `toolchain.test.ts > compiles a trivial contract and spends it through Spend`. It compiles a one-method `SmartContract` that asserts `x === 3n` and deploys it on `MockProvider`. It calls the method with 3 (accepted) and with 4 (`rejects.toThrow(/input 0/)`).
- [ ] **Step 5:** Run `npm install && npx vitest run --root contracts`. Expected: FAIL until `compile.mjs` and the helpers exist; then PASS.
- [ ] **Step 6:** Commit: `build: add the npm workspace and vendored Rúnar`.

### Task 2: Card encoding and point validation

**Files:**
- Create: `packages/protocol/src/points.ts`, `packages/protocol/src/cards.ts`
- Test: `packages/protocol/test/points.test.ts`, `packages/protocol/test/cards.test.ts`

**Interfaces:**
- Produces, in `points.ts`:
  - `parsePoint(bytes: Uint8Array): Point`, which throws `PointError` on a wrong length or prefix, x ≥ p, off-curve, or infinity;
  - `toAffine64(p: Point): Uint8Array`;
  - `fromAffine64(b: Uint8Array): Point`, which also checks y < p and on-curve;
  - `toCompressed(p: Point): Uint8Array`.
- Produces, in `cards.ts`:
  - `cardPoint(index: number): Point`, for 0..51 (throws outside);
  - `CARD_POINTS: readonly Point[]`;
  - `cardIndexOf(p: Point): number | null`.

- [ ] **Step 1: Write the failing tests.**
  - `parsePoint` rejects `02` + 32 × `ff` with message `/x is not a canonical field element/`.
  - It rejects 32 bytes, prefix `04`, and an off-curve x.
  - `fromAffine64(toAffine64(P))` equals `P` for `G` and `7·G`.
  - `CARD_POINTS` has 52 pairwise-distinct points.
  - `cardIndexOf(cardPoint(i)) === i` for every `i`, and `cardIndexOf(G) === null`.
  - Pinned values (compressed hex, computed 2026-10-30 with `@bsv/sdk`):
    - `cardPoint(0)` = `021738215e26042202a099ecc26fe754243449ad80eb1b4fc83831fc77a5ebcc4c` (found at `t = 5`);
    - `cardPoint(1)` = `02ac72bf790d18e083964db15028f69b4016f4e12773dcce0e0a7515eb8944cbbe` (`t = 0`);
    - `cardPoint(51)` = `02439e62c5b93bdbf44c8306df4bb3e5745fe46eb7c8bb7f49f2693ff8662a001c` (`t = 0`).
- [ ] **Step 2:** Run `npx vitest run --root packages/protocol`. Expected: FAIL (module missing).
- [ ] **Step 3:** Implement both files with `@bsv/sdk` `Point`/`BigNumber`. Check the range before calling `Point.fromString`.
- [ ] **Step 4:** Run again. Expected: PASS.
- [ ] **Step 5:** Commit: `feat(protocol): card points and strict point parsing`.

### Task 3: Chaum–Pedersen proofs

**Files:**
- Create: `packages/protocol/src/dleq.ts`
- Test: `packages/protocol/test/dleq.test.ts`

**Interfaces:**
- Consumes: `toAffine64` (Task 2).
- Produces:
  - `type DleqStatement = { domain: string; B1: Point; P1: Point; B2: Point; P2: Point }`. This asserts `log_B1 P1 = log_B2 P2`.
  - `statementBytes(st: DleqStatement, A1: Point, A2: Point): Uint8Array`. The layout is UTF-8 `domain` ‖ `0x00` ‖ `toAffine64` of `B1, P1, B2, P2, A1, A2`, in that order.
  - `challenge(bytes: Uint8Array): BigNumber` = big-endian SHA-256 of `bytes`, mod n.
  - `prove(st: DleqStatement, s: BigNumber, nonceSecret: Uint8Array): DleqProof`, where `DleqProof = { A1: Point; A2: Point; z: BigNumber }`.
  - `verify(st: DleqStatement, pr: DleqProof): boolean`.
  - `nonce(nonceSecret, st)` follows the Global Constraints formula.

- [ ] **Step 1: Write the failing tests.**
  - `verify(st, prove(st, s, secret))` is `true` for `B1 = G, P1 = s·G, B2 = X, P2 = s·X`.
  - It is `false` when `P2` is replaced by `(s+1)·X`.
  - It is `false` when `z` is incremented.
  - `prove` is deterministic: same inputs give an identical proof.
  - Two statements differing only in `domain` give different `A1`, because the nonce depends on the statement.
  - `challenge(statementBytes(...))` differs when a point is re-encoded 33-byte instead of 64-byte (Review Focus 2).
  - The remask pair from spec §7.3 step 2 verifies: proof (i) with `B1 = G, P1 = S_p, B2 = X_p, P2 = Y_p`, and proof (ii) with `B1 = G, P1 = K_used, B2 = S_p, P2 = P_p`, for `s_p = r·k⁻¹`.
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3:** Implement as specified in spec §7.2. `z = w + c·s mod n`.
- [ ] **Step 4:** Run. Expected: PASS.
- [ ] **Step 5:** Commit: `feat(protocol): Chaum-Pedersen proofs with statement-derived nonces`.

### Task 4: The one-multiplication checker

**Files:**
- Create: `contracts/src/EcMulCheck.runar.ts`
- Test: `contracts/test/ecmulcheck.test.ts`

**Interfaces:**
- Produces: stateless `EcMulCheck` with readonly `base: Point`, `k: bigint`, `out: Point` (the statement, baked in at creation) and one method `verify()`. That method asserts:
  - `ecOnCurve(base)`;
  - `k % n !== 0`;
  - `r = ecMul(base, k)`, with `ecOnCurve(r)`;
  - `r === out`.

  A `base` equal to `EC_G` uses `ecMulGen(k)`. There are two compiled variants, `EcMulCheck` and `EcMulGenCheck`, so each script holds one multiplication.

- [ ] **Step 1: Write the failing tests** (through `Spend`).
  - An honest statement `(X, k, k·X)` is spendable.
  - `(X, k, (k+1)·X)` is rejected with `/input 0/`.
  - `k = 0` is rejected.
  - The test prints the script size of both variants.
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3:** Implement. Record both script sizes in `docs/gates/d-checker-wiring.md` (created here, completed in Task 5).
- [ ] **Step 4:** Run. Expected: PASS. Each variant prints a script size near 429 KB.
- [ ] **Step 5:** Commit: `feat(contracts): single-multiplication checkers`.

### Task 5: Gate (d), the checker wiring

**Files:**
- Create: `contracts/src/GateReferee.runar.ts`
- Test: `contracts/test/gate-d.test.ts`
- Modify: `docs/gates/d-checker-wiring.md`

**Interfaces:**
- Consumes: `prove`, `statementBytes`, `challenge` (Task 3); `EcMulCheck`, `EcMulGenCheck` (Task 4).
- Produces: stateful `GateReferee` holding one recorded statement and proof (`B2, P1, P2, A1, A2, z`; `B1 = G` implied), a `bondSats` constant, and `phase` (0 = recorded, 1 = fraud proven). Methods:

  **`prepare(eq: bigint, tmplA: ByteString, tmplB: ByteString)`**
  - The caller supplies the fee input.
  - `eq ∈ {1, 2}` picks the equation to dispute.
  - The method recomputes `c` from the recorded statement on chain, using `sha256` and a big-endian bytes→bigint conversion, so it must match `challenge`.
  - Outputs, in order:
    1. the continuation;
    2. a checker for the left side, `z·B` (an `EcMulGenCheck` for eq 1, an `EcMulCheck` over `B2` for eq 2);
    3. a checker for `c·P` (over `P1` or `P2`);
    4. a bond output of `bondSats`.
  - Each checker's locking script is the supplied template with the statement spliced in, pinned through `hashOutputs`. Its `computeTemplateHash` must equal a constant baked at deploy.

  **`fraud(L: Point, M: Point)`**
  - Requires inputs 1 and 2 to be `(the previous transaction's txid, 1)` and `(…, 2)`, using `extractOutpoint` and `extractHashPrevouts`. Spending them proves `L = z·B` and `M = c·P`.
  - Asserts `L !== ecAdd(A, M)`, where `A` is `A1` or `A2` for the disputed equation.
  - Pays the bond back to the caller and sets `phase = 1`.

  **Checker-to-referee binding.** The checker contracts must not check outputs, so the binding is only the referee's outpoint check above.

- [ ] **Step 1: Write the failing tests** (through `Spend`, `MockProvider`).
  - `gate-d > proves fraud against a recorded invalid proof`: record a proof whose `z` was tampered with, then `prepare(1)` + `fraud` succeeds and the bond is returned.
  - `gate-d > refuses fraud against an honest proof` (Review Focus 3): record a valid proof, and `prepare(1)` succeeds (true statements), but `fraud` is rejected (`/input 0/`). The same holds for eq 2.
  - `gate-d > refuses checkers not created by the referee`: spend checkers from an unrelated transaction, and `fraud` is rejected.
  - Each test prints the serialized sizes of the `prepare` and `fraud` transactions.
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3:** Implement. Follow the spike rules: pass continuations explicitly and re-read state with `fromUtxo`, pin the full value with `addOutput(extractAmount(…), …)`, and use `ALL|FORKID` only.
- [ ] **Step 4:** Run. Expected: PASS.
- [ ] **Step 5: Write the results file.** Fill `docs/gates/d-checker-wiring.md`:
  - pass or fail;
  - the four sizes (`prepare`, `fraud`, each checker);
  - total bytes per fraud proof against spec §10.6's ~3.5 MB estimate;
  - each checker script's size against the stock node's 500 KB policy.
- [ ] **Step 6:** Commit: `feat(contracts): gate d - referee-created checkers and fraud proofs`.

### Task 6: Gate (e), the on-chain evaluator

**Files:**
- Create: `scripts/gates/evalvectors/main.go` (package `main`, in the existing Go module)
- Create: `contracts/gen/gen-eval7.py` (copy of `spikes/referee/gen-eval7.py`, already free of `t<digits>` names), `contracts/src/Eval7.runar.ts` (generated)
- Create: `packages/protocol/src/eval.ts`
- Test: `packages/protocol/test/eval.test.ts`, `contracts/test/gate-e.test.ts`
- Create: `docs/gates/e-evaluator.md`

**Interfaces:**
- Consumes: `internal/game/eval.Best([]cards.Card) (Result, error)`, `cards.FromIndex`.
- Produces:
  - `go run ./scripts/gates/evalvectors -n 2000 -seed 1` prints JSON `[{ "cards": [7 indices], "category": int, "score": int64 }]`. The corpus is seeded random hands plus the fixed set from Review Focus 4.
  - `bestScore(cards: number[]): number` in `eval.ts`: the contract's encoding, `category·16⁵ + five tiebreak values (rank + 1, 0 = none)`. It is computed as the best of all 21 five-card subsets (port the reference from `spikes/referee/eval7.test.ts`).

- [ ] **Step 1: Write the failing test** `eval.test.ts > agrees with the Go evaluator`. For every vector:
  - the category from `bestScore` equals the Go `category`;
  - for every consecutive pair, `sign(bestScore(a) − bestScore(b)) === sign(scoreA − scoreB)`.
- [ ] **Step 2:** Run. Expected: FAIL. Then implement `bestScore` and the Go generator until it PASSES.
- [ ] **Step 3: Re-vendor Rúnar with the #178 fix.** Build from `fix/if-arm-dead-local-desync` (icellan/runar#179, head `9fb51ea5`), merged with `fix/temp-name-collision` (icellan/runar#177), unless either is already on `main`. Record both heads in `PROVENANCE.md`.
- [ ] **Step 4: Write the failing test** `gate-e > the compiled evaluator accepts bestScore for every vector`. Through `Spend`, for all 2,000 vectors, `check(…cards, bestScore(cards))` is accepted. For one vector per category, `bestScore − 1` is rejected.
- [ ] **Step 5:** Run with `npx vitest run --root contracts test/gate-e.test.ts`. Expected: PASS.
  - If it fails, record the first failing hand and stop. Gate (e) is then a no-go, and Connor decides (spec §9).
- [ ] **Step 6: Write the results file.** Record in `docs/gates/e-evaluator.md`: pass or fail, the script size, the Rúnar heads used, and the corpus size.
- [ ] **Step 7:** Commit: `feat(contracts): gate e - on-chain evaluator agrees with the Go evaluator`.

### Task 7: Gate (c) remainder, MessageBox

**Files:**
- Create: `scripts/gates/src/messagebox-latency.ts`
- Create: `docs/gates/c-deal-and-messagebox.md`

**Interfaces:**
- Produces: `npm run gate:c -w scripts/gates`. It creates two `ProtoWallet` identities from random keys and sends 50 live messages each way through `MessageBoxClient` (the client's default host; the script prints which one it used), and 5 stored messages that are listed and acknowledged. It prints:
  - median and p95 round-trip time;
  - any quoted `recipientFee` or `deliveryFee` from `getMessageBoxQuote`.

- [ ] **Step 1:** Write the script. It exits non-zero if any message is lost or duplicated.
- [ ] **Step 2:** Run it. Expected: it completes and prints latency and fees.
- [ ] **Step 3: Write the results file.** Record the numbers in `docs/gates/c-deal-and-messagebox.md`, together with the deal cost already measured (about 1.6 s per seat per heads-up hand, `spikes/deal-cost/bench.mjs`).
  - Pass when median round trip ≤ 2 s and all fees are 0.
  - Otherwise record the fees for the reserve sizing.
- [ ] **Step 4:** Commit: `test(gates): gate c - MessageBox latency and fees`.

### Task 8: Gate (b), the BSV Desktop buy-in flow

**Files:**
- Create: `scripts/gates/buyin/index.html`, `scripts/gates/buyin/main.ts`, `scripts/gates/buyin/vite.config.ts`
- Create: `docs/gates/b-bsv-desktop-buyin.md`

**Interfaces:**
- Produces: `npm run gate:b -w scripts/gates` serves a page that:
  1. connects with `WalletClient('auto', 'gates.brc100-poker.local')`, and refuses unless `getNetwork()` is `testnet`;
  2. creates a 1,000-sat "escrow" P2PKH controlled by a page-held test key, via an ordinary `createAction`;
  3. calls `createAction` with that output as a foreign input, plus 2,000 sats of the player's coins, and two outputs (a 2,800-sat "continuation" P2PKH to the test key, and a 1-sat marker). It passes `signAndProcess: false` and `randomizeOutputs: false`.
  4. signs the foreign input locally;
  5. completes with `signAction`;
  6. reports the final transaction's input order (foreign input at index 0?), output order (pinned outputs first?), any extra outputs, and the txid's status from BSV Broadcaster.

- [ ] **Step 1:** Write the page. It shows every reported fact as pass or fail lines.
- [ ] **Step 2: Manual run (Connor).** With released BSV Desktop on testnet, open the page and approve both prompts. Expected: all lines pass.
- [ ] **Step 3: Write the results file.** Record in `docs/gates/b-bsv-desktop-buyin.md`: the BSV Desktop version, the txid, and each ordering fact.
  - If ordering is not preserved, record it. The design's pinned-prefix rule then needs the fallback noted in spec §10.5.
- [ ] **Step 4:** Commit: `test(gates): gate b - BSV Desktop foreign-input buy-in`.

### Task 9: Gate (a), mining the dispute transactions on testnet

**Files:**
- Create: `scripts/gates/src/broadcaster.ts`, `scripts/gates/src/gate-a.ts`
- Create: `contracts/src/HeadsUpReferee.runar.ts` (copied from the uncommitted `spikes/referee/contracts/`, which this plan does not commit)
- Test: `scripts/gates/test/broadcaster.test.ts`
- Create: `docs/gates/a-testnet-mining.md`

**Interfaces:**
- Consumes: `GateReferee`, `EcMulCheck`, `EcMulGenCheck` (Tasks 4–5), and `HeadsUpReferee` (post, act and forfeit shapes, compiled with the Task 1 toolchain).
- Produces:
  - `submit(efHex: string): Promise<{ txid: string; status: string }>`;
  - `waitFor(txid: string, until: 'SEEN_ON_NETWORK' | 'MINED', timeoutMs: number): Promise<string>`. It polls `GET /tx/{txid}` with per-request timeouts (G-15) and treats 404 and `UNKNOWN` as "not yet".
  - `readPolicy(): Promise<{ satPerKb: number; maxTxSize: number; maxScriptSize: number }>`.
  - `npm run gate:a -w scripts/gates -- address` prints the funding address of `secrets/gate-a.key`, creating the key if absent. `npm run gate:a -w scripts/gates -- run` runs the sequence.

- [ ] **Step 1: Write the failing tests** (Review Focus 5) against a stubbed `fetch`:
  - a POST answered 200 followed by `REJECTED` on the first GET makes `waitFor` reject with `/REJECTED/`;
  - 404 then `SEEN_ON_NETWORK` resolves;
  - `waitFor` never resolves `MINED` without a GET saying so.
- [ ] **Step 2:** Run. Expected: FAIL. Implement `broadcaster.ts`, then PASS.
- [ ] **Step 3: Fund the key (Connor).**
  1. Run `npm run gate:a -w scripts/gates -- address`.
  2. From BSV Desktop on testnet (topped up from a testnet faucet if needed), send 3,000,000 sats to the printed address.
- [ ] **Step 4: Run** `npm run gate:a -w scripts/gates -- run`. It must, in order:
  1. read `/policy`;
  2. deploy a `GateReferee` holding an invalid proof;
  3. broadcast a `prepare` + `fraud` pair, and wait until both are `MINED`;
  4. deploy a spike `HeadsUpReferee`, broadcast a post at its rung and an act, and a forfeit once its height has passed;
  5. broadcast a timelocked refund-shaped transaction after its height.

  Expected: every transaction reaches `MINED`. The script records each txid, serialized size, fee and blocks-to-mine.
- [ ] **Step 5: Write the results file.** Write `docs/gates/a-testnet-mining.md` with the table and the policy read.
  - Gate (a) passes only if the `prepare` + `fraud` pair is mined.
  - Note that the full referee's sizes are re-measured in its own plan.
- [ ] **Step 6:** Commit: `test(gates): gate a - dispute transactions mined on testnet`.

### Task 10: Gate summary and spec update

**Files:**
- Create: `docs/gates/README.md`
- Modify: `docs/superpowers/specs/2026-10-29-p2p-poker-design.md` §9 (gated unknowns) and §16 (evidence)

- [ ] **Step 1: Summarise.** Write `docs/gates/README.md`: one row per gate (a–e) with pass or fail, its key number and its results file.
- [ ] **Step 2: Update the spec.**
  - Move each passed gate from §9 to §16.
  - Re-size the dispute reserve in §10.6 from the measured gate (a) and (d) sizes.
  - Record the fee rate that gate (a) actually paid.
- [ ] **Step 3: Check go/no-go.** If any gate failed, stop and bring Connor the failure with its numbers before any further plan is written (spec §9).
- [ ] **Step 4:** Commit: `docs: phase-1 gate results`.
