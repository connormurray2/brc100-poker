# Phase-1 gates

Spec §9 (gated unknowns) and §15 (phases and gates) set five go/no-go gates before any build. This page is the
summary. Each row points at its results file, which holds the method, the numbers and the caveats.

| Gate | What it must show | Result | Key number | Results |
|---|---|---|---|---|
| a | The dispute transactions are mined on testnet, including a `prepare` of about 1.8 MB | **PENDING.** Needs Connor: a funded testnet key (3,000,000 sats to the address `npm run gate:a -w scripts/gates -- address` prints), then `npm run gate:a -w scripts/gates -- run --funding <txid>`. The harness is built and its dry run passes on 15 of 15 inputs. | `prepare` unlocking script 924,738 B, over a stock node's 500 KB script policy and under BSV Broadcaster `/policy` (100 MB). The funded run decides it. | [a-testnet-mining.md](a-testnet-mining.md) |
| b | BSV Desktop keeps the foreign-input buy-in's input and output order | **PASS** (2026-10-30) | Foreign input at index 0, continuation and marker first and in order, change after them; `SEEN_ON_NETWORK` with no competitor. The wallet paid about 252 sat/kB. | [b-bsv-desktop-buyin.md](b-bsv-desktop-buyin.md) |
| c | Deal cost, MessageBox latency and fees | **PASS** (2026-10-30) | Median round trip 122 ms against a 2 s limit (50 live messages, none fell back to HTTP); every quoted fee 0 | [c-deal-and-messagebox.md](c-deal-and-messagebox.md) |
| d | The checker wiring convicts a false proof and refuses an honest one | **PASS** (2026-10-30), after fix round 1 | One fraud proof (`prepare` + `fraud`) is 1,921,675 B for equation 1 and 1,496,287 B for equation 2, against spec §10.6's 3.5 MB estimate. Each checker script is about 427 KB, under 500 KB. | [d-checker-wiring.md](d-checker-wiring.md) |
| e | The on-chain evaluator agrees with the Go evaluator | **PASS** (2026-10-30) | 2,000 of 2,000 vectors accepted through `Spend`; an 11,420 B script; a score one too low or too high rejected in all 9 categories | [e-evaluator.md](e-evaluator.md) |

**Phase 1 is go/no-go once gate a reports.** Gates c, d and e passed on `Spend` (MockProvider) and MessageBox.
Gate b passed on testnet with BSV Desktop; its escrow and buy-in are the only transactions broadcast so far. No gate has failed.

Gate d's and gate e's findings that change the real referee's design are carried into the spec (§10.3, §10.6,
§10.7 and §13). Gates b, c, d and e are recorded as evidence in spec §16; a stays in §9 until it runs.
