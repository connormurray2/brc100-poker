# Operational runbook

For whoever is on the other end of a page. Written around the one fact that makes this system
unusual to operate:

> **The database is the wallet.** The toolbox has no UTXO discovery and no restore-from-seed. If
> the database is lost, the coins are unspendable even though the keys survive. Backup is a
> correctness requirement, not ops hygiene.

## Deployment

| | |
|---|---|
| Host | `poker.bsvcloudsolutions.com` (nyc1) |
| Database | DO managed Postgres `poker-db`, nyc1, same VPC |
| Network | Teratestnet (`ttn`). Coins are worthless; the design is not. |
| Chain services | `arcade-v2-ttn-us-1.bsvblockchain.tech` — hosted by the BSV Association, not by us |

Two binaries: `table` (the coordinating service) and `agent` (a player's own signer, which runs
on the player's machine, not here).

## Startup

The service **refuses to start** on an unsafe configuration rather than degrading quietly, and
reports every problem at once. If it exits immediately, read the message — it names the variable
and says why:

```
POKER_POSTGRES_DSN is required in production: the database is the wallet, and it must be backed up
POKER_REQUIRE_TLS must be true in production: substrate calls carry signing authority
```

Required in production: `POKER_ENV=production`, `POKER_POSTGRES_DSN`, `POKER_WALLET_KEY`,
`POKER_REQUIRE_TLS=true`. Real-value play additionally requires `POKER_BACKUP_VERIFIED_AT` — see
the restore drill.

The effective configuration is logged at startup with the wallet key and the DSN password
omitted. If you need to confirm what the service actually read, that line is the answer.

## Health

Two probes, and the distinction matters:

- **`/livez`** — is the process running. **Deliberately ignores dependencies.** Restarting the
  process because a remote oracle is unreachable makes an outage worse, not better. If this fails,
  restart.
- **`/readyz`** — can it serve play. Fails (503) if the database, the oracle, the header service,
  or status tracking is down *or has not been checked*. An unchecked dependency counts as
  not-ready: the service proves it can work rather than assuming so.

`/readyz` says **why**:

```json
{"ready": false,
 "reasons": ["oracle is down: 504 from the gateway"],
 "realValueReady": false,
 "realValueBlockedBy": ["no database restore has been proved: an unrestorable wallet loses the coins, not just availability"],
 "dependencies": [...]}
```

`realValueReady` is stricter than `ready` on purpose. A service can be perfectly able to serve
play while still being unfit to hold value.

## Dependency outage

**Oracle or headers unreachable.** A hand cannot settle: the oracle is the only broadcast target
and the only double-spend adjudicator, and there is no fallback or multi-arcade HA. The service
stops starting new real-value hands and reports itself degraded. **Funds are not at risk** —
players' refunds remain available. Do not restart; wait, and check the BSV Association's status.

**Database unreachable.** Readiness fails. Do not fail over to a fresh database: an empty wallet
database is not a working one, and starting against one means the service cannot find its own
coins. Restore from backup if the instance is genuinely lost.

**Status tracking not running.** Readiness fails, and it should: without the monitor daemon,
transactions never receive a status at all. A broadcast will appear to succeed and never resolve.

## Chain liveness

Teratestnet block production is **intermittent**. Observed: no block for six minutes at one point,
roughly ten-minute intervals at another. Anything waiting on a merkle proof needs a generous
timeout.

A settlement that is broadcast but unmined is **not** a fault. It means exactly what it says:
accepted, not settled. The winner cannot receive the payout until a block arrives, because
internalizing verifies a proof.

## A stalled hand

A hand stalls when a seat stops cooperating — it will not shuffle, will not disclose a scalar, or
will not sign the settlement.

**What is true:** nobody's money is lost. Every seat holds a signed refund from *before* it funded,
and can recover its stake unilaterally once the refund's locktime matures.

**What to do:**

1. `/readyz` and the logs will name the responsible seat. A stall must record a reason, so an
   unexplained one is itself a bug.
2. Tell the players which seat stalled and when their refunds mature.
3. Do **not** try to force a settlement. A pot needs every seat; there is no override, by design.

**Asymmetry worth knowing:** withholding blocks the deal for everyone who depended on that seat's
scalar, but not for the withholder — it already holds its own. A griefing seat suffers nothing from
stalling, which is why attribution matters more than detection.

## Money movement

Every broadcast is recorded with its purpose, its classified outcome, and — for a rejection — the
network's own reason. Rejections and unknown outcomes log at **error** level so they survive a
threshold set to mute routine traffic.

To reconstruct what happened to a hand's funds, filter the audit log by `handId`. That is the
reconstruction query, and it works without consulting the chain — which matters because the most
likely time to need it is when the chain view is the thing in doubt.

**Reading an outcome:**

| Outcome | Meaning | Action |
|---|---|---|
| `accepted` | In the pipeline. **Not settled.** | Wait for a status. |
| `rejected` | Final verdict. | **Never retry.** The reason is in the record. |
| `backpressure` | Never queued. | Safe to retry after the advised delay. |
| `indeterminate` | Fate unknown. | Reconcile by querying the transaction, do not resend. |

## "The wallet says it has money but cannot spend it"

A real failure mode, diagnosed the hard way. Symptom: a healthy balance and a funding failure —
a 500 sat payment refused against 95,936 sat of otherwise-claimable coins.

**Cause:** an action that was built and never broadcast leaves its change recorded against an
**all-zero txid**. That phantom coin blocks the funder from selecting anything at all.

**Diagnose:** list the wallet's outputs and look for an outpoint beginning
`0000000000000000…`.

**Prevent:** the service aborts actions it will not complete, so provisional change is released.
If you see this, an abort path was missed — that is a bug, not an ops problem.

**Ruled out:** it is not `WithRequiredChangeOutput` and not a real shortfall. Both were tested.

## Backup and the restore drill

Managed Postgres takes automated daily backups with point-in-time recovery. **Verify they exist;
do not assume.**

The drill, which gates real-value play:

1. Fork the database to a new instance from a backup:
   `doctl databases create poker-db-drill --engine pg --restore-from-cluster-name poker-db`
2. Point a *scratch* table service at the fork.
3. Confirm the wallet reports the balance and outputs you expect, and that
   `/readyz` reports the database up.
4. Spend a small amount from the restored wallet. Reading a balance is not proof it can sign.
5. Destroy the fork.
6. Record the date in `POKER_BACKUP_VERIFIED_AT` and redeploy.

Until step 6, `realValueReady` is false and the service will not put real value at risk. That is
the intended interlock: an unrestorable wallet does not lose availability, it loses the coins.

**Do not restore an older snapshot as a routine rollback.** The wallet database is not
rollback-safe: an older snapshot can permanently lose knowledge of coins created since. Schema
changes are additive and forward-only, and a restore is a deliberate recovery action.

## Rollback

The service is stateless apart from Postgres, so rollback is redeploying the previous tagged
image:

```sh
docker pull registry.digitalocean.com/<registry>/poker-table:<previous-tag>
systemctl restart poker-table
```

**Never revert the database alongside it.** See above.

## Key handling

- The table's wallet key pays fees only. It **never** holds player funds and never sees a player
  key.
- Player keys live in player agents, on players' own machines.
- Keys are supplied at runtime, never baked into an image, so an image can be shared without
  sharing custody.
- Back up the table's key **and** its database. Either alone is insufficient: the key without the
  database cannot find the coins, and the database without the key cannot sign for them.

## Escalation

| Symptom | Likely cause | First move |
|---|---|---|
| Exits at startup | Configuration | Read the error; it names the variable |
| `/readyz` 503, oracle down | Upstream outage | Wait; check BSV Association status |
| `/readyz` 503, database down | Instance or network | Check `doctl databases get poker-db` |
| Hand stalled | A seat stopped cooperating | Identify the seat; inform players of refund timing |
| Broadcast rejected | See the recorded reason | Never retry; fix the cause |
| Balance present, funding fails | Phantom zero-txid change | Look for a `0000…` outpoint; a missed abort |
| Settlement not mining | Teratestnet block interval | Wait; it is not a fault |
