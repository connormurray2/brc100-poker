## Context

See `proposal.md` — Why. What follows is only the constraint set that shapes the approach.

Four facts do most of the design work here:

1. **The wallet is an in-process Go library.** `wallet.New(chain, key, provider, …)` takes a key
   in memory; it never leaves that process. There is no BRC-100 wire transport in the toolbox —
   the old JSON-RPC one was removed in the rewrite. Whoever runs the process holds the keys.
2. **The pot cannot be signed by one party.** An n-of-n output needs a signature from every
   seat, produced on every seat's own machine. BRC-100 has no partial-signature concept, and the
   toolbox has no multisig, escrow, or co-signing code or examples at all.
3. **The wallet will not track the pot.** Custom-script outputs come back `Spendable=false` by
   construction; only change is minted into the UTXO store. Caller-provided inputs are never
   reserved or spend-checked.
4. **The database is the wallet.** No UTXO discovery, no restore-from-seed. Losing Postgres
   loses the coins even though the keys survive.

The game side is comparatively well-understood: the mental-poker math and evaluators are verified
correct, and `IGameTransport` is already the seam the engine programs against.

## Goals / Non-Goals

**Goals:**
- A player's private key never leaves a process that player controls.
- A complete Hold'em hand settles real Teratestnet value between 2–6 independent wallets.
- Every seat can verify what it signs, and can recover its stake unilaterally if cooperation fails.
- The riskiest unknown — remote co-signing — is proven before the game work depends on it.

**Non-Goals:**
- A general-purpose BRC-100 wallet server for arbitrary third-party applications. The substrate
  is scoped to what this game needs; generalising it is a later, separate concern.
- Throughput engineering. The toolbox sustains hundreds of TPS; a poker table needs single-digit.
  Every fuel-pool and high-throughput knob is deliberately left at its simple setting.
- Reusing any of the old chain layer, or preserving the .NET test suite as a running artifact.
- Tournament structures, multi-table play, or the deferred variants and side features.

## Decisions

### D1: Go, single binary per role, one repository

Go, because the wallet is a Go library and any other language reintroduces the wire boundary this
change exists to remove. Two binaries: a **table service** (game coordination, its own fee-paying
wallet) and a **player agent** (one player's wallet plus the substrate endpoint). Shared game and
protocol packages live in one module.

*Alternatives:* keeping C# and wrapping the toolbox in a Go sidecar — rejected because the sidecar
boundary is exactly the BRC-100 transport we would have to design anyway, and it doubles the
runtime surface. A full greenfield rewrite ignoring the old repo — rejected because the
mental-poker math and evaluators are verified correct and expensive to re-derive.

### D2: Ship the player agent; design the substrate as its transport

The substrate is built as a real network protocol, but the first thing that speaks it is a player
agent the player runs. This is deliberate sequencing rather than a compromise: it makes the
protocol concrete and testable immediately, keeps custody with the player from day one, and leaves
a browser-wallet implementation as a drop-in second client against a protocol already proven by a
working one. Browser wallets cannot drive a toolbox wallet today regardless of what we build, so
nothing is lost by proving the protocol with an agent first.

*Alternatives:* server-held keys — rejected, custodial. Waiting for a browser wallet to implement
the substrate before building anything — rejected, it blocks all progress on a third party.

### D3: Mutual authentication over an authenticated channel, not asserted identity

Both sides prove control of their identity key by signing a challenge that binds the request, a
nonce, and a timestamp. The toolbox's own storage REST authenticator is explicitly **not** the
model: it verifies nothing, so any caller can claim any identity and spend that user's coins. TLS
is required outside local development; the identity proof is layered on top rather than trusted to
the channel alone.

*Alternatives:* header-asserted identity behind a private network — rejected, it makes every
network boundary a custody boundary. Bearer tokens — rejected, a stolen token is a stolen wallet
and we already have identity keys.

### D4: The table proposes, the seats verify independently

The table service assembles candidate transactions but holds no pot key and cannot move funds.
Each agent independently recomputes the hand outcome from the message log and refuses to sign
anything inconsistent with it. This is the property that makes a coordinating server safe: it is
a convenience, not an authority, and a malicious table can stall a hand but never steal a pot.

### D5: Pot mechanics — the two-step path, with the sharp edges pinned

Funding uses a custom `LockingScript` output. Settlement uses
`CreateAction(SignAndProcess=false)` → `SignableTransaction{Tx, Reference}` → collect signatures →
`SignAction(Reference, Spends)`. This two-step form is documented for exactly this case and is
load-bearing in the toolbox's reference application, so it is an exercised path rather than a
theoretical one.

Non-negotiable settings, each corresponding to a way this silently breaks:

| Setting | Why |
|---|---|
| `RandomizeOutputs: false` | Defaults to **true**; shuffles outputs before vouts are assigned, breaking any signature that commits to the output set — intermittently, under load |
| Pin the change output | Sub-dust change is silently dropped, changing the output count with no construction-time error |
| Pot coins in a dedicated basket | Caller-provided inputs are not excluded from funding selection, so a pot coin can be selected twice and rejected as a duplicate input |
| Fee rate 125 sat/kB | Arcade prices the extended-format size; the toolbox default of 100 leaves no margin |
| Over-declare `unlockingScriptLength` | Fee-sizing only, and under-declaring earns an unretryable 4xx |
| Verify scripts locally before broadcast | The library does verify, but a local check turns a remote rejection into a local error |
| Gate nLockTime finality client-side | A non-final refund is broadcast anyway and rejected 4xx; the library's finality helper is never called |

The winner is paid to a **BRC-29-derived P2PKH**, not an arbitrary script or basket insertion —
a basket-inserted coin records no derivation material and so cannot be spent by the receiver.

### D6: Refund before funding, unconditionally

No seat's funds enter the pot until that seat holds a fully-signed nLockTime refund of its own
stake. This is the liveness backstop for every failure mode below, so it is a precondition
enforced in code rather than a recovery procedure.

### D7: Application-owned pot ledger with a write-ahead record

Because the wallet tracks neither pot outputs nor caller-provided inputs, the table keeps its own
ledger of pot UTXOs and writes intent **before** signing or broadcasting, so a crash between the
two is recoverable by reading back what was attempted. Postgres, same instance as wallet storage.

### D8: WebSocket transport implementing the existing seam

`IGameTransport` ports as-is. Two constraints inherited from the old protocol: the transport must
**echo publishes back to the sender** (the seating handshake depends on it, and a naive
exclude-sender fan-out breaks seating permanently), and it must de-duplicate by message id. The
old 120 ms re-broadcast-everything loop is **not** ported — it is a polling protocol in
event-driven clothing that will not scale past a few tables. Replaced by explicit acknowledgement
and targeted catch-up on reconnect.

### D9: Wire ShuffleProof into live play

The proof exists and is tested but is not wired into the live game, so a malicious shuffler is
presently unconstrained. Since this change puts real value on the table, it is wired in as part of
the deal protocol rather than left as a post-hoc audit.

### D10: Deployment — one droplet, managed Postgres, hosted chain services

Table service on a droplet in `nyc1` (co-located with existing BSV infrastructure), DO managed
Postgres, and the BSV Association's hosted Arcade and ChainTracks on `ttn`. No chain
infrastructure of our own. The toolbox ships no Dockerfile or deployment tooling, so the container
image and service units are ours. Automated Postgres backup with a **tested restore drill** is a
release gate, not a follow-up, because the database is the wallet.

The monitor daemon runs in-process with a non-blocking, non-panicking, idempotent status observer,
and exactly one SSE stream. The callback token must be derived explicitly — the library does not
do it, and omitting it measurably strands transactions with no status at all.

## Risks / Trade-offs

- **The substrate is the critical path with no in-repo precedent** → Prove it first with a
  standalone 2-of-2 co-signing spike on Teratestnet, before any game code depends on it. If the
  spike fails, the whole approach is reconsidered while it is still cheap.
- **n-of-n is liveness-sensitive: one seat can stall settlement** → Pre-signed refunds bound the
  worst case to a wait, not a loss. Refund locktime is a tuned parameter: short enough to be a
  credible remedy, long enough not to race normal play.
- **Every seat must be online to settle** → Accepted for a 2–6 seat single-table slice. A
  threshold scheme would relax it and is the natural follow-up, but it is materially more complex
  and out of scope here.
- **A `4xx` broadcast rejection returns `err == nil` with `Rejected: true`** → A single broadcast
  wrapper is the only path to Arcade, and it treats the rejection flag as authoritative. Never
  retry a 4xx; always retry a 503.
- **Arcade is the only transaction oracle, with no fallback or HA** → A table cannot settle while
  it is unreachable. Funds are never at risk, but play stops; surface it as an explicit health
  state rather than a hang.
- **Losing Postgres loses the coins** → Automated backup plus a rehearsed restore, gated before
  real value. On Teratestnet the value is worthless, which makes now the right time to rehearse.
- **Toolbox is `v0.1.0` and its `createAction`/`signAction` path has no executing conformance
  coverage** → We depend on that path most heavily, so our own integration tests against live
  `ttn` are the real coverage. Pin the dependency version.
- **BRC-100 identity discovery resolves `ttn` to the public testnet overlay** → Do not use it for
  player identity. Identity is the seat's own key, exchanged in the game protocol.
- **`GetNetwork` emits `"ttn"`, which is not a valid BRC-100 network value** → Translate at the
  substrate boundary; never pass the library's internal name to a client.
- **Porting verified crypto risks introducing bugs the original did not have** → Port the
  exhaustive evaluator check across all 2,598,960 five-card hands, and the hostile-forgery tests
  that prove the reveal commitment rejects a derived forged scalar. These are the tests worth
  carrying over; the network-tier tests are not.
- **Two parallel game stacks with divergent behaviour** → Standardise on one and delete the other
  before porting, so the ambiguity is resolved once rather than carried forward.
- **Go 1.26.3 required; local toolchain is 1.21.6** → Upgrade before starting; the toolchain gap
  is a hard build blocker, not a warning.

## Migration Plan

Greenfield, so "migration" is really sequencing and rollback of a deployment.

1. **Toolchain and dependency pin.** Go 1.26.3, toolbox pinned by version.
2. **Spike: 2-of-2 co-signing on `ttn`.** Two wallets in separate processes fund a shared output
   and co-sign a settlement, exercising every pinned setting in D5. This is the go/no-go gate for
   the whole design.
3. **Substrate + player agent**, proven against the spike's transaction shapes.
4. **Port the game core** — mental poker, evaluators, betting engine — with the exhaustive and
   hostile tests carried over. No chain dependency, so this proceeds in parallel with 3.
5. **Table service and WebSocket transport**, playing complete hands with no money.
6. **Join the two**: buy-in, refund-before-funding, settlement, payout on `ttn`.
7. **Deploy**: container, droplet, managed Postgres, backup and a rehearsed restore.

**Rollback:** the table service is stateless apart from its Postgres ledger, so rollback is
redeploying the previous image. The wallet database is **not** rollback-safe — restoring an older
snapshot can lose knowledge of coins permanently — so schema changes are additive and forward-only,
and a restore is a deliberate recovery action, never a routine rollback step.

## Open Questions

- Refund locktime duration, and whether it scales with the number of seats.
- Whether the table service's fee wallet also underwrites pot transaction fees, or each seat pays
  its own share.
- Buy-in denomination and blind sizes for the demo table.
- Whether the browser client talks to the player agent over a local port or a browser extension
  surface — deferrable, since both speak the same substrate protocol.
