# Phase-1 gates

Spec §9 (gated unknowns) and §15 (phases and gates) set five go/no-go gates before any build. This page is the
summary. Each row points at its results file, which holds the method, the numbers and the caveats.

| Gate | What it must show | Result | Key number | Results |
|---|---|---|---|---|
| a | The dispute transactions are accepted on testnet (`SEEN_ON_NETWORK`, G-10) | **PASS** (2026-10-30), on the second design | All 8 kinds `SEEN` with no competitor in 75 s, 692 sats. The first run failed: no public route takes a transaction over ~1 MiB, so the 1.85 MB `prepare` was stuck. `WitnessEcMulCheck` (55 KB, against 427 KB) brought `prepare` to 309 KB posted. | [a-testnet-mining.md](a-testnet-mining.md) |
| b | BSV Desktop keeps the foreign-input buy-in's input and output order | **PASS** (2026-10-30) | Foreign input at index 0, continuation and marker first and in order, change after them; `SEEN_ON_NETWORK` with no competitor. The wallet paid about 252 sat/kB. | [b-bsv-desktop-buyin.md](b-bsv-desktop-buyin.md) |
| c | Deal cost, MessageBox latency and fees | **PASS** (2026-10-30) | Median round trip 122 ms against a 2 s limit (50 live messages, none fell back to HTTP); every quoted fee 0 | [c-deal-and-messagebox.md](c-deal-and-messagebox.md) |
| d | The checker wiring convicts a false proof and refuses an honest one | **PASS** (2026-10-30), after fix round 1; rerun 15 of 15 on `WitnessEcMulCheck` | One fraud proof (`prepare` + `fraud`) is 414,250 B on either equation; first measured at 1,921,675 B with one-`ecMul` checkers. | [d-checker-wiring.md](d-checker-wiring.md) |
| e | The on-chain evaluator agrees with the Go evaluator | **PASS** (2026-10-30) | 2,000 of 2,000 vectors accepted through `Spend`; an 11,420 B script; a score one too low or too high rejected in all 9 categories | [e-evaluator.md](e-evaluator.md) |

**Phase 1's gates all pass.** Gates a and b ran on testnet (BSV Desktop for b, and for funding a); c ran on live
MessageBox; d and e on `Spend` (MockProvider). Gate a failed once, on relay limits in front of the nodes, and passed
after the checkers were redesigned to fit them. That leaves the real referee a script budget of about 130 KB
(spec §10.6).

Gate d's and gate e's findings that change the real referee's design are carried into the spec (§10.3, §10.6,
§10.7 and §13). All five are recorded as evidence in spec §16.
