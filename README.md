# brc100-poker

Non-custodial peer-to-peer poker on BSV, where every satoshi moves through
[BRC-100](https://brc.dev/100) wallets and no server ever holds a player's keys or funds.

Target network: **Teratestnet** (`ttn`). Deployment target: Digital Ocean.

> Status: early. Planning artifacts live in `openspec/changes/brc100-poker-vertical-slice/`;
> `tasks.md` is the current source of truth for what is and is not built.

## The constraint that shapes everything

**No custody.** A player's private key never leaves a process that player controls. The table
service coordinates play and pays its own fees, but it cannot move the pot — spending the pot
requires an authorisation from every seat.

This is why the architecture looks the way it does, and it has one direct consequence worth
stating up front: **every seat must be online to settle a hand.** The backstop is that each
player holds a fully-signed refund of their own stake *before* funding, so an uncooperative or
absent seat costs everyone a wait, never their money.

## Source repositories

This project is a rewrite, not a fork. Two upstream repositories inform it:

| Repo | Role |
|---|---|
| [`galt-tr/go-arcade-toolbox`](https://github.com/galt-tr/go-arcade-toolbox) | The BRC-100 wallet. Owns funding, signing, broadcast, merkle-proof verification and status tracking. Pinned dependency. |
| [`prof-faustus/bsv-poker`](https://github.com/prof-faustus/bsv-poker) | Port source for the game. 28k LOC of C#/WPF; we port the mental-poker protocol and the betting engine, and discard its hand-rolled wallet and BSV node entirely. |

`prof-faustus/bsv-poker-web` also exists but is a two-file Blazor skeleton, not a usable web app.

### What is ported, and what is deliberately dropped

**Ported** (~1,550 LOC of real value): the Barnett–Smart commutative-encryption shuffle on
secp256k1, the hand evaluators (verified upstream against all 2,598,960 distinct five-card
hands), the Hold'em betting engine with layered side pots, the reveal-commitment proof, and the
transport interface the game engine already programs against.

**Dropped** (~15,000 LOC): the entire built-in wallet and chain stack — transaction
serialisation, FORKID sighash, P2PKH and multisig signing, a self-implemented listening BSV node
with header sync and bloom-filter SPV, Base58Check/RIPEMD-160, the WPF app, the desktop profile
model, and a peer-discovery routine that sweeps a /24 with hundreds of concurrent TCP connects.
BRC-100 supersedes all of it.

Two upstream notes worth knowing before reading that code: its documentation overstates what is
implemented in specific places (the `tools/` directory that its "verified on real consensus"
claims depend on does not exist), and it contains two parallel, non-communicating game stacks.
We standardise on one. See `openspec/changes/brc100-poker-vertical-slice/proposal.md`.

## Architecture

Two binaries, one module:

- **`cmd/table`** — the table service. Coordinates play, holds a fee-paying wallet, holds no pot
  key and no player key. It proposes transactions; it cannot move funds.
- **`cmd/agent`** — the player agent. Runs on a machine the player controls, holds that player's
  key, and serves BRC-100 calls over the substrate. Signs only what the player approves.

```
internal/
  game/        cards, evaluators, betting engine, mental poker, proofs   (no chain dependency)
  protocol/    transport, table coordination, BRC-100 substrate, co-signing
  wallet/      BRC-100 wiring, broadcast classification, pot ledger
  config/      network and fee resolution
```

### Why there is a substrate at all

The toolbox is a BRC-100 wallet *library*: a key is passed to `wallet.New(...)` in-process and
never leaves it. There is **no BRC-100 wire transport** — the old JSON-RPC one was removed in the
rewrite, and upstream states the limitation plainly: BSV Desktop can *pay* an application but
"cannot *drive* a toolbox wallet."

So a remote wallet can fund us, but cannot sign for us — and settling an n-of-n pot requires
exactly that. The substrate is the missing transport, with mutual cryptographic authentication.

The toolbox's own storage REST API is **not** the model for it: its default authenticator
performs no cryptographic verification, so any caller can claim any identity, and it serves no
TLS. It must never be exposed to an untrusted network.

## Operational facts you cannot ignore

**The database is the wallet.** There is no UTXO discovery and no restore-from-seed. If the
Postgres instance is lost, the coins are unspendable even though the keys survive. Automated
backup with a *rehearsed restore* is a correctness requirement, not ops hygiene — and it gates
real-value play.

**The monitor daemon is mandatory.** Without it, transactions never receive a status at all.

**A rejection is not an error.** A 4xx broadcast rejection returns `err == nil` with a rejection
flag set. Code that checks only the error reads a rejection as success. Never retry a 4xx; always
retry a 503.

## Development

Requires **Go 1.26.3+** (the toolbox requires it) and PostgreSQL for anything beyond unit tests.

```sh
make build        # compile
make test         # unit tests
make test-race    # unit tests under the race detector
make lint         # golangci-lint
make check        # the full gate CI enforces
make integration  # tests against live teratestnet; needs a funded wallet
```

Unit tests run with no external services. Tests requiring live Teratestnet are behind the
`integration` build tag so the default suite stays hermetic.

## License

Not yet determined. The upstream toolbox is Open BSV License v5; `bsv-poker` ships its own
license. Both must be reconciled before any public release.
