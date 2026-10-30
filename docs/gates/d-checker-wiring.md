# Gate d: checker wiring

Status: **PASS** (2026-10-30), with one size finding for stock nodes (below).

A Rúnar covenant, `GateReferee`, convicts a recorded Chaum–Pedersen proof that is false, and cannot be made to convict one that is true. The two expensive EC multiplications live in separate checker outputs:
- `prepare` creates the checkers;
- `fraud` spends them alongside the referee.

Every transaction runs through `@bsv/sdk` `Spend` (MockProvider), with every input validated: `contracts/test/gate-d.test.ts`, 12 tests. Not broadcast to testnet yet; that is gate (a).

## What was built

`contracts/src/GateReferee.runar.ts`, a `StatefulSmartContract`; every method is `ALL|FORKID`.

**Recorded at deploy (readonly):**
- the statement `P1, B2, P2`, with `B1 = G` implied;
- the proof `A1, A2, z`;
- the domain, `bondSats`, and the checker-template constants.

**State:**
- `phase`: 0 = recorded, 1 = prepared, 2 = fraud proven;
- `eq`;
- the claimed products `L` and `M`;
- `bondPKH`.

**`prepare(eq, L, M, mulA, mulB, genBody, refundPKH)`**, from phase 0 to phase 1:
- It recomputes `c = sha256(domain ‖ 00 ‖ G ‖ P1 ‖ B2 ‖ P2 ‖ A1 ‖ A2)` on chain, read big-endian and reduced mod n. Each point is 64-byte affine. Because `bin2num` is little-endian and signed, the hash is reversed and a `00` byte appended first.
- It builds both checker locking scripts by splicing minimal pushes into the caller's template segments:
  - eq 1: `EcMulGenCheck(z, L)` and `EcMulCheck(P1, c, M)`;
  - eq 2: `EcMulCheck(B2, z, L)` and `EcMulCheck(P2, c, M)`.
- Its outputs, pinned through `hashOutputs`, are:
  0. the continuation, holding the referee's value plus the bond;
  1. the `L` checker;
  2. the `M` checker;
  3. the caller's change.

**`fraud(feeOutpoint)`**, from phase 1 to phase 2:
- It requires `hashPrevouts = sha256d(own outpoint ‖ (prepTxid, 1) ‖ (prepTxid, 2) ‖ feeOutpoint)`, where `prepTxid` comes from the referee's own outpoint and that outpoint's vout must be 0.
- It asserts `L !== ecAdd(A, M)`, where `A` is `A1` or `A2` depending on the disputed equation.
- Its outputs are:
  0. the continuation, holding the value minus the bond;
  1. the bond, paid as P2PKH to the `refundPKH` that `prepare` stored;
  2. the caller's change.

**Template binding.**
- `prepare`'s caller supplies only the large segments:
  - `mulA` (841 B) and `mulB` (426,190 B) of `EcMulCheck`;
  - `genBody` (426,256 B) of `EcMulGenCheck`.
- The tiny segments are deploy-time constants: `mulMid` (5 B), `mulTail` (1 B), `genHead` (34 B), `genMid` (5 B) and `genTail` (1 B).
- The referee checks each caller segment's length and the SHA-256 of `mulA ‖ mulB` (or of `genBody`) against constants baked at deploy.
- The length check is what stops a boundary shift. Moving bytes between `mulA` and `mulB` keeps the hash and changes the script. The test pins this, and a mutant without the length asserts accepts the shifted template.

## Locking-script layout

Template offsets come from `artifact.constructorSlots`. The tests derive the segments from the artifact and fail if the slot shape moves. Points are pushed as `0x40 ‖ x ‖ y`. Scalars are pushed as minimal Script numbers: `OP_1`..`OP_16`, otherwise `len ‖ little-endian`, with a `00` pad byte when the top bit is set.

| Checker | Template | Slots (template byte offsets) | Deployed layout |
|---|---|---|---|
| `EcMulCheck` | 427,042 B | `base@0 k@842 base@848 k@849 out@427040` | `push(base) ‖ mulA ‖ push(k) ‖ mulMid ‖ push(base) ‖ push(k) ‖ mulB ‖ push(out) ‖ mulTail` |
| `EcMulGenCheck` | 426,299 B | `k@34 k@40 out@426297` | `genHead ‖ push(k) ‖ genMid ‖ push(k) ‖ genBody ‖ push(out) ‖ genTail` |

**The referee's on-chain `pushNum` is checked against the SDK for every encoding shape a cheater could pick for `z`:** 1, 16, 17, 127, 128, 255, 0x7fff, 0x8000, 2^255 and n−1. Each shape is prepared and convicted. If any of them spliced differently, a well-chosen `z` would make the lie unprovable.

## Results

| Test | Result |
|---|---|
| Fraud against a tampered `z`, eq 1 and eq 2: accepted, and the bond is returned | pass |
| A wrong `P2` (eq 1 true, eq 2 false): convicted on eq 2, refused on eq 1 | pass |
| An honest proof, eq 1 and eq 2: `prepare` accepted (true statements), `fraud` refused at input 0 | pass |
| An honest proof `prepare`d with a false `L` or a false `M`: the referee input alone validates, but the false checker (input 1 or 2) cannot be spent, so `fraud` fails | pass |
| Checkers not created by this referee, all refused at input 0: byte-identical copies from another transaction, a mix of copy and genuine, and the genuine pair swapped | pass |
| A forged template: a flipped byte in `mulB` or `genBody`, and a boundary shift in each direction | pass (refused) |
| The on-chain `c` equals `challenge()` for the pinned vector (`c = be78ee…6a0e`). `prepare`'s checker outputs equal the SDK-built `EcMulCheck(P1, challenge(), M)` and `EcMulGenCheck(z, L)` | pass |
| Phases: no `fraud` before `prepare`, no second `prepare`, no `prepare` after `fraud` | pass |

**Mutation checks.** Removing any one guard fails exactly the test aimed at it:
- the inequality;
- the segment lengths;
- the `mulA ‖ mulB` hash;
- the `genBody` hash;
- the `hashPrevouts` binding;
- the phase checks;
- the `OP_1..16` branch or a length threshold in `pushNum`.

## Sizes

Measured in the tests. The deployed checker sizes depend on the push length of `k`, shown here with a 32-byte `z` and a 33-byte `c`.

| Item | Bytes | vs the stock 500 KB (500,000 B) per-script policy |
|---|---|---|
| `EcMulCheck` locking script, as deployed | 427,300 | under, with 72,700 B headroom |
| `EcMulGenCheck` locking script, as deployed | 426,427 | under, with 73,573 B headroom |
| `GateReferee` locking script (template 34,278) | 35,265 | under |
| `prepare` eq 1, whole tx (2 inputs, 4 outputs) | 1,813,271 | its input-0 **unlocking** script is **924,006 B: over** |
| `prepare` eq 2, whole tx | 1,387,883 | its input-0 unlocking script is 497,746 B: under, by only 2,254 B |
| `fraud`, whole tx (4 inputs, 3 outputs) | 106,219 | its input-0 unlocking script is 70,591 B |
| **One fraud proof, `prepare` + `fraud`**, eq 1 | **1,919,490** | |
| **One fraud proof, `prepare` + `fraud`**, eq 2 | **1,494,103** | |

**Against spec §10.6's ~3.5 MB.**
- A lie needs one false equation, so it needs two checkers, not four.
- One fraud proof therefore measures 1.92 MB (eq 1) or 1.49 MB (eq 2), about 55% and 43% of the estimate.
- At 1 sat/kB that is about 2 sats; at 100 sat/kB about 192 sats.
- **The real referee will be larger.** Each referee spend carries the referee script three times:
  - the code-part push in the unlocking script;
  - the scriptCode inside the preimage push;
  - the continuation output.
- At this gate that is about 103 KB of each transaction, so `fraud` is 106 KB, almost all referee script. With §10.6's ~200 KB referee, each of `prepare` and `fraud` grows by about 0.5 MB. One fraud proof then comes to about 2.9 MB (eq 1) or 2.5 MB (eq 2), still under 3.5 MB. Re-size the reserve from the real referee's script once it exists.

## Findings for the real referee

1. **The eq-1 `prepare` unlocking script is 924 KB, over the stock 500 KB per-script policy, if that policy applies to unlocking scripts.** It carries both templates, `EcMulCheck`'s and `EcMulGenCheck`'s.
   - Eq 2 needs only one template (both of its checkers are `EcMulCheck`) and sits 2.3 KB under.
   - BSV Broadcaster reported `maxscriptsizepolicy` 100 MB on testnet and mainnet (spike, 2026-10-29), so gate (a) can still mine it.
   - For stock-policy nodes, the real referee should:
     - create one checker per `prepare` step (two steps), so each unlocking script holds one template;
     - and shrink its own script, which appears twice in every unlocking script.
2. **Fees and change are solved.** Change works under `ALL|FORKID`: Rúnar's continuation hashes `continuation ‖ data outputs ‖ change`, with the change gated on `_changeAmount != 0`. Both `prepare` and `fraud` carry a change output to the caller. The icellan/runar#176 limitation is specific to `SINGLE`, and it does not apply here.
3. **Record-time format checks are load-bearing, as spec §10.3 requires.** The gate records at deploy, so the deployer is trusted. The real referee must refuse these at record, or a cheater escapes:
   - a `z` outside `[1, n)`: a `z ≡ 0` checker is unspendable by design;
   - an off-curve `B2`, `P1` or `P2`: `EcMulCheck` asserts `ecOnCurve(base)`;
   - a non-canonical `A1` or `A2`: `ecAdd` rejects non-canonical coordinates with `OP_VERIFY`.
4. **Bond.** The bond rides in the referee's continuation (+`bondSats` at `prepare`, −`bondSats` at `fraud`) and is returned to the `refundPKH` that `prepare` stored. It is not a separate output of `prepare`.
   - A `fraud` caller other than the preparer cannot take the bond.
   - Forfeiting the bond when a checker lapses unspent is out of scope here (no timeout path).
5. **`prepare` and `fraud` must still be broadcast as a pair (spec §10.7).** Nothing stops a third party spending the anyone-can-spend checkers on their own. Doing so only destroys the dispute and forfeits the preparer's bond.

## Verdict

**PASS.** A referee-created pair of one-`ecMul` checkers convicts a false recorded proof on either equation. It refuses to convict a true one, including when the caller lies about a product. It refuses foreign or reordered checkers, and refuses forged or boundary-shifted templates. Each checker script is under 500 KB.

Open for gate (a): the eq-1 `prepare` unlocking script (924 KB) on a stock-policy node, and actual mining.
