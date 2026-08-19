## 1. Toolchain and repository setup

- [x] 1.1 Upgrade the local Go toolchain to 1.26.3 and verify `go version` reports it (the toolbox requires it; 1.21.6 will not build)
- [x] 1.2 Initialise the Go module and monorepo layout: `cmd/table`, `cmd/agent`, and shared `internal/` packages for game, protocol, and wallet code
- [x] 1.3 Add `github.com/galt-tr/go-arcade-toolbox` pinned to an explicit version, and commit `go.sum`
- [x] 1.4 Verify the dependency works end to end by running the toolbox's own quickstart example against its in-process mocks
- [x] 1.5 Add linting, `go vet`, race-enabled test running, and a CI workflow that runs all three
- [x] 1.6 Write the repository README recording the two source repos, the chosen architecture, and the non-custodial constraint

## 2. Wallet wiring against Teratestnet

- [x] 2.1 Implement wallet construction for Teratestnet: services config, storage provider, migration, and wallet, with the network set explicitly on every component that accepts one
- [x] 2.2 Derive and set the Arcade callback token explicitly; add a startup check that fails if it is unset
- [x] 2.3 Set the fee rate to 125 sat/kB and enable the minimum-broadcast-fee-rate guard so underpayment fails locally
- [x] 2.4 Start the monitor daemon with a status observer that is non-blocking, panic-free, and idempotent; assert exactly one status stream is opened
- [x] 2.5 Implement the broadcast wrapper as the single path to Arcade, classifying accepted / rejected / backpressured / indeterminate, treating the rejection flag as authoritative even when no error is returned
- [x] 2.6 Add retry policy to the wrapper: never retry a rejection, always retry backpressure after its indicated delay, reconcile an indeterminate outcome by querying status
- [x] 2.7 Implement funds entry via internalize-payment (the spendable form), and confirm a credited coin can subsequently be spent
- [x] 2.8 Implement network-value translation so no internal network name is ever returned at an exposed boundary
- [x] 2.9 Write integration tests against live Teratestnet covering funding, spending, and status progression to a settled state

## 3. Co-signing spike — go/no-go gate

- [x] 3.1 Build a standalone spike: two wallets in separate processes, each holding its own key
- [x] 3.2 Fund a 2-of-2 output with a custom locking script from one wallet
- [x] 3.3 Construct the spend with the two-step path, obtaining the signable transaction and its reference
- [x] 3.4 Collect a signature from each process independently and assemble the unlocking script in the order the locking script requires
- [x] 3.5 Disable output randomisation, pin the change output, and confirm signatures stay valid across repeated runs
- [x] 3.6 Keep the shared output in a basket separate from fee-paying coins and confirm it is never selected to pay a fee
- [x] 3.7 Over-declare the unlocking script length and confirm the fee is accepted
- [ ] 3.8 Verify scripts locally before broadcast, then broadcast and confirm the transaction settles on Teratestnet
- [x] 3.9 Build a pre-signed nLockTime refund with a non-final sequence, gate finality client-side, and confirm it is refused before its locktime and accepted after
- [x] 3.10 Record the findings and confirm the design holds; if the spike fails, stop and revisit the approach before proceeding

## 4. BRC-100 substrate

- [x] 4.1 Define the wire protocol: request and response envelopes, method naming, and structured error shape
- [x] 4.2 Implement mutual authentication where both parties prove identity-key control over a challenge binding the request, a nonce, and a timestamp
- [x] 4.3 Reject asserted identity, replayed requests, and tampered requests; add tests for each
- [ ] 4.4 Require transport protection outside local development and refuse to start without it
- [x] 4.5 Implement per-identity least-privilege method grants, and verify a table identity cannot enumerate a player's outputs or actions
- [ ] 4.6 Implement explicit signing consent: present amounts, destinations, and committed outputs, and sign only on approval for that specific request
- [ ] 4.7 Ensure an approval authorises exactly one signature and cannot be replayed for a second
- [ ] 4.8 Add per-caller rate limiting and a maximum request body size
- [ ] 4.9 Write tests proving results match the in-process wallet for every exposed method
- [ ] 4.10 Confirm by test that no private key material appears in any request, response, or log line

## 5. Player agent

- [ ] 5.1 Implement the agent binary: load the player's own key, construct their wallet, and serve the substrate endpoint
- [ ] 5.2 Implement the approval surface presenting each signing request's material terms to the player
- [ ] 5.3 Implement the agent's own record of its stake, refund, and hand state
- [ ] 5.4 Document how a player generates a key, funds it from a BRC-100 wallet on Teratestnet, and starts the agent

## 6. Game core port

- [x] 6.1 Choose the single game stack to carry forward, and record what is deleted and why
- [x] 6.2 Port the card model and deck using a cryptographically secure random source
- [x] 6.3 Port the hand evaluator, including wheel straights, kickers, and the exactly-two-hole-cards constraint; fix the pre-river board bug rather than reproducing it
- [x] 6.4 Port the exhaustive evaluator test across all 2,598,960 distinct five-card hands, asserting zero category and ordering errors
- [x] 6.5 Port the secp256k1 primitives the game protocol needs — point multiply, scalar inverse and arithmetic, card base points, point validation — and omit the wallet-superseded key generation, signing, and DER encoding
- [x] 6.6 Port the commutative-encryption deck: public card encodings, mask application and stripping, and fail-closed validation of hostile input
- [x] 6.7 Add tests proving masks commute, strip in any order, and that a partially unmasked card matches no public card
- [x] 6.8 Port the shuffle protocol with per-player permutation and masking, and the re-mask step giving each position independent per-player secrets
- [x] 6.9 Port the private hole-card deal and prove by test that colluding opponents pooling every other secret still cannot read a hole card
- [x] 6.10 Port the public board deal and the verifiable showdown reveal
- [ ] 6.11 Port the reveal-commitment proof, including the hostile test that derives a forged scalar and asserts the commitment rejects it
- [ ] 6.12 Port commit-reveal seat ordering
- [ ] 6.13 Wire the shuffle proof into live play so a malicious shuffler is constrained during the hand, not merely audited afterwards
- [x] 6.14 Port the betting engine: blinds, streets, legal-action computation, and turn order including the heads-up special case
- [x] 6.15 Port pot resolution with layered side pots, split pots, the deterministic odd-chip rule, and a chip-conservation assertion
- [x] 6.16 Add a deterministic replay test proving two independent participants reach identical state from the same action sequence

## 7. Table coordination

- [x] 7.1 Port the transport interface unchanged, and implement it over WebSocket
- [x] 7.2 Implement sender-inclusive fan-out, since the seating handshake depends on a publish echoing back to its own sender
- [x] 7.3 Implement message de-duplication by id so a redelivered message is applied exactly once
- [ ] 7.4 Replace the old periodic re-broadcast loop with explicit acknowledgement and targeted catch-up on reconnect
- [ ] 7.5 Implement table lifecycle: creation with declared terms, advertisement, join, and close
- [ ] 7.6 Bind each seat to a proven identity and reject messages not attributable to that seat, including an identity taking two seats
- [ ] 7.7 Enforce ordered protocol progression, rejecting or deferring messages for steps that are not current
- [ ] 7.8 Implement action timeouts defaulting to check when facing no bet and fold when facing one
- [ ] 7.9 Implement deal and reveal timeouts that attribute the stall to the responsible seat
- [ ] 7.10 Implement reconnect and resynchronisation without re-sending another seat's private material
- [ ] 7.11 Run a full multi-seat hand with no money over the real transport
- [x] 7.12 Delete the old subnet-sweeping peer discovery and the desktop profile model rather than porting either

## 8. Pot, co-signing, and settlement

- [x] 8.1 Implement the pot locking script requiring every seat's authorisation
- [x] 8.2 Implement the application-owned pot ledger recording pot outputs and their state
- [x] 8.3 Implement the write-ahead record around the sign and broadcast boundary
- [ ] 8.4 Implement refund construction and enforce in code that no stake is committed before its refund is held
- [ ] 8.5 Implement buy-in funding into the pot, refusing to start a hand on a partial pot while preserving funded seats' refunds
- [ ] 8.6 Implement settlement construction paying the winner to a spendable derived payment, with value conservation asserted
- [ ] 8.7 Implement proposal distribution so every seat can reconstruct exactly what it is asked to sign and confirm all seats got the same proposal
- [x] 8.8 Implement independent per-seat verification against the seat's own hand record, refusing a wrong winner, altered amount, unexpected output, or unexpected input
- [x] 8.9 Implement local script evaluation before broadcast
- [x] 8.10 Implement signature collection, ordered assembly, rejection of invalid signatures with attribution, and refusal to broadcast an incomplete set
- [x] 8.11 Prove by test that output order and change presence cannot change after signing, and that modifying a signed field invalidates collected signatures
- [ ] 8.12 Implement non-cooperation handling: bounded outcome, attribution, and a preserved recovery path for every other seat
- [ ] 8.13 Prove by test that a settlement and its hand's refunds cannot both succeed
- [ ] 8.14 Implement crash recovery that determines from the ledger whether a transaction was broadcast, without double-spending or abandoning the pot
- [ ] 8.15 Implement player-visible money state for commitment, settlement, and a stalled hand including refund availability
- [ ] 8.16 Play a complete hand for real value on Teratestnet between independent wallets: buy-in, deal, betting, showdown, settlement, and payout
- [ ] 8.17 Test the stall path for real: fund a pot, have a seat refuse to sign, and confirm every other seat recovers its stake via refund

## 9. Deployment

- [ ] 9.1 Write the container image build for the table service, since the toolbox provides none
- [ ] 9.2 Implement configuration validation that refuses to start on missing settings or an unsafe production configuration, and logs the effective configuration without secrets
- [ ] 9.3 Supply secrets at runtime and add a test asserting no key or credential appears in logs or errors
- [ ] 9.4 Assert by test that the table service holds no player private key
- [ ] 9.5 Provision the Digital Ocean droplet in nyc1 and managed Postgres, keeping unauthenticated interfaces off the public network
- [ ] 9.6 Configure automated database backups and surface backup failure
- [ ] 9.7 Perform and document a restore drill proving a restored backup yields a working wallet, and gate real-value play on it
- [ ] 9.8 Implement liveness and readiness checks, with readiness reflecting database and status-tracking availability
- [ ] 9.9 Implement chain-service health reporting, refusing to start a new real-value hand when the oracle is unreachable and informing players when it fails mid-hand
- [ ] 9.10 Implement money-movement logging recording every broadcast, its purpose, its classified outcome, and any rejection reason
- [ ] 9.11 Verify a hand's full money history is reconstructable from the records
- [ ] 9.12 Deploy from a tagged artifact and verify rollback restores the previous version without reverting the wallet database
- [ ] 9.13 Play a complete real-value hand against the deployed service from at least two independent player agents

## 10. Close-out

- [ ] 10.1 Resolve the deferred parameters and record them: refund locktime duration, whether the table underwrites pot fees or seats pay their own share, and the buy-in and blind sizes
- [ ] 10.2 Document the substrate protocol well enough for a browser wallet to implement it as a second client
- [ ] 10.3 Record the deferred scope — the other variants, group blackjack, chat, card NFTs, hand tape, replay — and what each would require
- [ ] 10.4 Write the operational runbook: startup, health interpretation, dependency outage, stalled hand, and restore
