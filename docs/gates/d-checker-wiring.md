# Gate d: checker wiring (in progress)

Status: **in progress.** Task 4 (the checkers) is done. Task 5 (the referee that creates and spends them) fills in the rest.

A Chaum-Pedersen lie is convicted by spending two stateless checkers on chain: `L = z·B` and `M = c·P`. Each multiplication gets its own script because one `ecMul` is about 427 KB.

## Checker sizes

Locking-script template sizes from `node contracts/compile.mjs EcMulCheck EcMulGenCheck`. The deployed script is the template with its constructor slots filled in, so it is a little larger (see the layout section for Task 5).

| Contract | Constructor | Template bytes |
|---|---|---|
| `EcMulCheck` | `base: Point, k: bigint, out: Point` | 427,042 |
| `EcMulGenCheck` | `k: bigint, out: Point` | 426,299 |

Both are stateless, with one public method `verify()` that takes no arguments, and neither needs a signature. `ecMulGen` saves only about 0.7 KB over `ecMul` here.

## Locking-script layout

_Task 5: copied from the Task 4 report once the covenant rebuilds these scripts._

## Fraud transaction size and cost

_Task 5._

## Verdict

_Task 5._
