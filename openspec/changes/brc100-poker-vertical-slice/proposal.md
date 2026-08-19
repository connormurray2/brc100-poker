## Why

BSV needs a credible, non-custodial demonstration that BRC-100 wallets can settle real
adversarial multi-party value on Teratestnet. The existing [`bsv-poker`](https://github.com/prof-faustus/bsv-poker)
project already solved the hard cryptographic problem — dealerless mental poker with true
hole-card privacy — but it is a Windows-only WPF desktop app (`poker.exe`) that ships its
own hand-rolled wallet: its own secp256k1 key derivation, its own FORKID sighash builder,
and its own listening BSV P2P node that peers with DNS seeds. That wallet is precisely the
layer BRC-100 exists to standardise, and it cannot be deployed as a service.

This change keeps the game and replaces the money layer: the proven mental-poker protocol
and betting engine are ported to Go, and every satoshi moves through
[`go-arcade-toolbox`](https://github.com/galt-tr/go-arcade-toolbox) — a BRC-100 wallet
library — against public Teratestnet infrastructure, with players holding their own keys.

## What Changes

- **New Go monorepo** replacing the .NET solution. Go is chosen because the wallet toolbox
  is an in-process Go library (`go 1.26.3`), so a Go server embeds it with no wire boundary.
- **Port the game logic from C# to Go.** Of 28,187 LOC, roughly 1,550 is worth keeping
  verbatim and ~1,900 more is a design worth porting; the rest is superseded or discarded:
  - `MentalPokerEC` — Barnett–Smart commutative-encryption shuffle on secp256k1, where card
    *i* is the curve point `(i+1)·G` and masks commute because `a·(b·P) == b·(a·P)`. This is
    the asset worth keeping (~155 LOC of dense, well-documented math with fail-closed hostile
    validation).
  - Hand evaluation and the Hold'em betting engine, including genuine layered side pots. These
    files have **zero** `using` statements and no wallet or chain coupling, so they port
    mechanically — and their correctness was verified exhaustively against all 2,598,960
    distinct five-card hands with zero category or ordering errors.
  - `RevealProof` and `SeatOrder` (commit-reveal seating), plus `ShuffleProof`, which exists and
    is tested but is **not wired into live play** — meaning a malicious shuffler is currently
    unconstrained. Wiring it in is part of this change.
  - The `IGameTransport` seam (`Subscribe(tableId, onEvent)` / `PublishAsync(tableId, payload, id)`).
    The `NetGame` state machine already programs against this interface rather than the TCP
    mesh, so a WebSocket transport is a small job — with one non-obvious constraint: the seating
    handshake requires the transport to echo a publish back to its own sender, so a naive
    "broadcast to everyone except the sender" fan-out breaks seating permanently.
- **Resolve the two parallel game stacks.** The repo contains two non-communicating
  implementations: a `Variant` enum wired into the live engine and networked play, and a
  `PokerGame`/`GameDef` table with the better evaluator and hi-lo/lowball support but **no
  betting engine**, reachable only from test-only helpers. Neither matches the documented set of
  variants. This change standardises on one stack — the live engine plus the better evaluator —
  and deletes the other.
- **BREAKING: delete the entire built-in wallet and chain stack** (~15,000 LOC). Every
  satoshi-touching path is replaced by BRC-100 calls. Discarded as redundant: `Chain.cs`
  (tx serialisation, FORKID sighash, P2PKH/multisig signing), `OnChainWallet.cs`,
  `WalletKeys.cs`, `WalletExtras.cs`, `Identity.cs`, `ScriptEngine.cs`, all 18 files under
  `BsvPoker.Net/Bsv/` (a real but wholly redundant BSV P2P node with header sync and bloom-filter
  SPV), and the Base58Check/RIPEMD-160 primitives. The toolbox owns funding, signing, broadcast,
  merkle-proof verification against ChainTracks, and status tracking.
  - Discarding this layer also retires a real correctness risk: `Chain.SighashForkId` has **no
    known-answer test vector** anywhere in the 9,915 LOC test suite — every sighash test is
    differential, so a wrong-but-self-consistent sighash would pass the whole suite and be
    rejected by every node on the network. Under BRC-100 the wallet owns the sighash.
- **Delete `PeerDiscovery.cs` outright.** It sweeps a /24 with roughly 762 concurrent TCP
  connects every four seconds and uses a shared `/tmp` rendezvous file. On a hosted droplet this
  is indistinguishable from port-scanning.
- **Replace the profile model.** `Profile.cs` holds a process-wide static file lock, caps
  instances at 64, writes the raw 32-byte identity seed to disk in plaintext, and assumes
  per-user desktop paths — a hard blocker for a multi-tenant service.
- **BREAKING: no Windows desktop client, no WPF.** `BsvPoker.App` is discarded. The client
  becomes a browser UI; the table becomes a deployed service.
- **Players hold their own keys.** The server never holds player funds and never sees a player
  private key. The server operates only a table-coordinator wallet that pays its own fees and
  never takes custody of the pot.
- **NEW: a BRC-100 HTTP substrate**, because the toolbox does not ship one. The toolbox
  implements all 28 BRC-100 methods as an **in-process Go library** — a key is passed to
  `wallet.New(chain, key, …)` and never leaves that process. The old JSON-RPC transport was
  deliberately removed in the rewrite, and `GETTING_STARTED.md:271` states the limitation
  plainly: BSV Desktop can *pay* an application but "cannot *drive* a toolbox wallet — there
  is no BRC-100 transport shim in this repo." Without a transport, player-held keys can only
  fund (one-directional, via `InternalizeAction`); they cannot sign a pot settlement. This
  change builds that missing transport so a remote wallet can serve BRC-100 calls over the
  network, with cryptographic mutual authentication.
  - The toolbox's own storage REST API is **not** a template to copy: its default
    authenticator "performs NO cryptographic verification — any caller can claim any identity
    by setting the header" (`pkg/storage/rest_auth.go:26-30`), and it serves no TLS. Anyone
    reaching that port could spend another user's coins. It must stay on a private interface.
- **NEW: a multi-party co-signing protocol.** BRC-100 has no notion of partial signatures.
  A search of the toolbox found **zero** occurrences of `multisig`, `CHECKMULTISIG`, `escrow`,
  `PSBT`, `co-sign`, or `multi-party`, and no example demonstrating a custom script or a
  caller-supplied unlocking script. Collecting N signatures from N machines and assembling the
  pot's unlocking script is ours to define.
- **Non-custodial n-of-n pot on Teratestnet.** Buy-ins fund a shared n-of-n output; the
  winner is paid by a settlement co-signed by all seats; every player holds a pre-signed
  nLockTime refund *before* risking a satoshi. Verified feasible against the toolbox source:
  `CreateActionOutput.LockingScript` takes an arbitrary script, `CreateActionInput` accepts a
  caller-supplied `UnlockingScript`/`SequenceNumber`, `CreateActionArgs.LockTime` sets the
  refund locktime, and `CreateAction(SignAndProcess=false)` returns a
  `SignableTransaction{Tx, Reference}` that `SignAction(Reference, Spends)` completes. This
  two-step form is documented for exactly this case — "if your inputs carry caller-supplied
  unlocking scripts — a covenant, a hash-locked output, anything the wallet cannot sign for
  you — you use the two-step form" (`docs/application-throughput-playbook.md:541-616`) — and is
  load-bearing in the toolbox's reference application, so it is an exercised path.
- **The app owns pot-UTXO bookkeeping.** Custom outputs are returned `Spendable=false` by
  construction; only change is minted into the wallet's UTXO store. The toolbox will not track
  the pot for us, so the table service maintains its own record of pot outputs, with a
  write-ahead record across the sign/broadcast boundary because caller-provided inputs are
  never reserved or spend-checked by the library.
- **The winner is paid to a BRC-29-derived P2PKH.** A basket-inserted coin records no
  derivation material and therefore cannot be signed by the receiving wallet, so the
  settlement output must be a BRC-29 payment the winner can actually spend afterwards.
- **Deployed to Digital Ocean** (nyc1, alongside existing BSV droplets), with managed
  Postgres for wallet storage. Backups are a **correctness** requirement, not ops hygiene:
  the toolbox has no UTXO discovery and no restore-from-seed, so the database *is* the wallet.
- **Scope is one vertical slice**, not feature parity: a single 2–6 seat Texas Hold'em table
  playing a complete hand for real Teratestnet value. Explicitly deferred: the other five
  poker variants, group blackjack, encrypted chat, card NFTs, on-chain hand tape, replay.

## Capabilities

### New Capabilities
- `mental-poker`: dealerless shuffle, deal, and reveal with true hole-card privacy — the
  commutative-encryption deck protocol, per-position mask exchange, showdown reveal, and the
  proofs that make a deal verifiable after the fact.
- `poker-engine`: card model, hand evaluation, and the Texas Hold'em betting engine — seats,
  blinds, street progression, legal-action computation, pot and side-pot resolution.
- `table-coordination`: the transport and session layer — table lifecycle, seat join/leave,
  message ordering and dedup across seats, disconnect/timeout handling, and the transport
  abstraction that carries game messages between players.
- `brc100-wallet`: the wallet integration boundary — toolbox wiring for Teratestnet, the
  mandatory monitor daemon and status observer, funding, broadcast error taxonomy, and how
  player wallets are connected and identified.
- `brc100-substrate`: the missing BRC-100 network transport — how a remote BRC-100 wallet
  receives and answers wallet calls over the network, how caller and wallet mutually
  authenticate cryptographically, and which methods are exposed to which callers. This is the
  critical path: no real hand can settle without it, and the toolbox provides no template.
- `cosigning`: the multi-party signing protocol — how a partially-signed pot transaction is
  distributed to seats, how each seat verifies what it is being asked to sign before signing,
  how signatures are collected and assembled into an unlocking script, and what happens when a
  seat refuses or disappears.
- `onchain-settlement`: the money protocol — buy-in to the n-of-n pot, the pre-signed refund
  every player holds before funding, cooperative settlement to the winner, and the recovery
  paths when a seat stops cooperating.
- `deployment`: running the table service on Digital Ocean — configuration, Postgres storage
  and its backup/restore obligation, secrets, and health/observability.

### Modified Capabilities
<!-- None: this is the first change in a greenfield repo, so there are no existing specs
     under openspec/specs/ whose requirements could change. -->

## Impact

**Source repositories**
- `prof-faustus/bsv-poker` — reference for the protocol and the port source. 28,187 LOC of C#;
  a single squashed commit dated 2026-06-16. **Real code, not scaffolding**: zero
  `NotImplementedException`, zero TODO/FIXME in `src/`, 469 test cases with 1,442 assertions, and
  real external test vectors (RIPEMD-160, secp256k1, BIP37 filterload, network magic). Only ~3%
  of tests are filler. The crypto is genuine, including real threshold ECDSA (JVRSS/PROSS/INVSS)
  where the private key is never assembled.
  - The module boundary is drawn in the right place: `Crypto`, `Core`, and `Net` are all plain
    `net8.0` with **no** Windows APIs, no DPAPI, and no file I/O in `Core` or `Crypto`. Only the
    App project is `net8.0-windows`.
  - But ~3,500–4,000 LOC of non-UI money logic sits inside the WPF layer — `WalletView.cs` alone
    is 4,183 lines mixing UI, persistence, coin control, funding orchestration, and SPV
    coordination. Almost all of it is precisely what a BRC-100 wallet provides, so it is being
    deleted rather than rewritten.
  - Documentation drift is significant and must not be trusted as a specification: the `tools/`
    directory that every "verified on real consensus" claim depends on **does not exist**; the
    documented `ChatService` does not exist; documented chat forward secrecy is contradicted by
    the code; and the documented variant list does not match either implemented stack.
    `RED_TEST.md` and `BUILD_BACKLOG.md` are, by contrast, unusually honest and worth reading.
- `prof-faustus/bsv-poker-web` — a Blazor WASM skeleton (2 `.cs` files) whose transport is a
  same-browser `BroadcastChannel`. Not a usable web app; its README lists "browser BSV wallet"
  as unbuilt roadmap. Superseded by this change.
- `galt-tr/go-arcade-toolbox` — new core dependency. Requires **Go 1.26.3**; local toolchain
  is 1.21.6 and must be upgraded. Local `dotnet` is absent, which also rules out continuing
  the C# path without new tooling.

**External services**
- Arcade tx-oracle and ChainTracks headers on Teratestnet, hosted by the BSV Association:
  `arcade-v2-ttn-us-1.bsvblockchain.tech`. No chain infrastructure of our own to run.
- Arcade is the *only* transaction-truth oracle — no fallback, no multi-arcade HA. A table
  cannot settle while it is unreachable.
- Player funding requires BSV Desktop wallets on `ttn`, which is a real onboarding cost for
  anyone we want to demo to.

**Digital Ocean** (account `connor@murraydt.com`)
- New droplet or App Platform service in `nyc1`; new managed Postgres; container registry
  already exists (`registry.digitalocean.com/frog-smash`, nyc3).
- The toolbox ships **no Dockerfile, no compose file, and no deployment tooling** — its Makefile
  targets are dev-only. Container images and service units are ours to write.
- No environment variable selects the network; it is set in YAML or in Go code only.

**Known risks**
- **The BRC-100 substrate is the critical path and has no in-repo precedent.** It is protocol
  design, not glue code, and it gates every real-value hand. It must be proven — ideally by a
  two-wallet co-signing spike on Teratestnet — before the game work depends on it.
- The n-of-n pot is trust-minimised but liveness-sensitive: an uncooperative seat can stall
  settlement until the refund locktime. The refund is the backstop, and its parameters are a
  design decision, not an afterthought.
- **`RandomizeOutputs` defaults to `true`**, shuffling outputs before vouts are assigned. Any
  signature committing to the output set breaks intermittently unless it is disabled
  explicitly — a silent, load-dependent failure.
- **Sub-dust change is silently dropped**, changing the output count with no construction-time
  error. Signatures that commit to the output set must pin the change output explicitly.
- Caller-provided inputs are not excluded from funding selection, so a pot coin sharing a
  basket with funding coins can be selected twice and rejected as a duplicate input. Pot coins
  must live in a dedicated basket.
- A non-final nLockTime refund is broadcast without a local finality check and rejected by
  Arcade with an unretryable 4xx; finality must be gated client-side.
- Fee rate must be **125 sat/kB**, not the toolbox default of 100 — Arcade's validator prices
  the extended-format size and 100 leaves no margin.
- A 4xx broadcast rejection returns `err == nil` with `Rejected: true`. Code that only checks
  `err != nil` will read a rejection as success. Never retry a 4xx; always retry a 503.
- Without the monitor daemon running, transactions never receive a status at all.
- `GetNetwork` returns internal names and emits the invalid BRC-100 value `"ttn"` on
  Teratestnet; it must be translated at any API boundary we expose.
- Certificate and identity discovery resolve Teratestnet to the **public testnet** overlay and
  certifier set, so BRC-100 identity discovery is wrong-network on `ttn` and cannot be relied
  on for player identity.
- The toolbox is `v0.1.0`, and the `createAction`/`signAction` write path — the part this design
  leans on hardest — has no executing conformance coverage despite 98 vendored vectors.
