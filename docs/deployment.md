# Deployment

The live teratestnet deployment, as provisioned.

## What exists

| Resource | Detail |
|---|---|
| Droplet | `poker-table`, `167.99.239.236`, nyc1, `s-1vcpu-2gb`, Ubuntu 24.04 |
| Database | `poker-db`, DO managed Postgres 16, nyc1, `db-s-1vcpu-1gb` |
| VPC | `a3417bc5-…` — the same nyc1 VPC as the other BSV services |
| DNS record | `poker.siftbitcoin.com` A → `167.99.239.236` (in DO) |
| Service | `poker-table.service`, listening on `127.0.0.1:8080` |
| Proxy | Caddy, terminating TLS on 443 |
| Table wallet | `02aa7acd60b5ee06b4e473f2aa9550710356cc0ef1b018378e321f8f6499dcaa68`, funded 100,000 sat |

Approximately $27/month: $12 droplet + $15 database.

## Endpoints

| Path | |
|---|---|
| `/` | JSON index: version, network, the table's identity key, stakes, and the endpoint list |
| `/livez` | Liveness — the process only |
| `/readyz` | Readiness, plus whether it is fit to hold value |
| `/table?table=<id>` | Game transport; a WebSocket upgrade, so a plain GET returns 426 |

The root index exists so opening the URL in a browser explains what the service is and hands over
the identity key a player's agent needs to authorise. Any other path is a genuine 404 rather than
the index served for everything, so a wrong path says so instead of letting a client believe it
reached something.

## Security posture

- **The database is reachable only from the droplet.** A DO database firewall restricts access to
  droplet `593606554`; nothing else on the internet or in the VPC can connect.
- **The service listens on localhost only.** Its port is never world-reachable; Caddy fronts it.
- **UFW allows 22, 80 and 443 only.**
- **The service runs unprivileged** as `poker`, under systemd hardening: `ProtectSystem=strict`,
  `NoNewPrivileges`, `MemoryDenyWriteExecute`, and no filesystem write access beyond
  `/var/lib/poker` — its state is in Postgres.
- **Secrets live in `/etc/poker/table.env`**, mode 0640 owned by the service account, supplied at
  runtime and never baked into a binary or an image.
- **The startup log omits the wallet key and the DSN password**, verified in the live log line.

## Verified working

```
$ curl https://poker.siftbitcoin.com/readyz
{"ready":true,
 "realValueReady":true,
 "dependencies":[
   {"name":"database","state":"up"},
   {"name":"headers","state":"up","detail":"tip at height 29705"},
   {"name":"oracle","state":"up"},
   {"name":"statusTracking","state":"up"}]}
```

The service connected to managed Postgres over the private VPC endpoint, reached the teratestnet
oracle and header service, started its monitor daemon, and resolved its wallet identity.

`realValueReady` is **true** because the restore drill was performed. Before the drill it was
correctly false, which is the interlock working: real value is gated on a proved restore, not on an
operator's intention to run one.

## Domain and TLS

Live at **https://poker.siftbitcoin.com** with a Let's Encrypt certificate
(`CN=poker.siftbitcoin.com`, issued 2026-08-19).

`siftbitcoin.com` is delegated to DigitalOcean's nameservers, so ACME validation succeeded on the
first attempt. An earlier attempt on `bsvcloudsolutions.com` failed for a reason worth recording:
the A record was correct and DO's own nameserver returned it, but the domain has **no nameservers at
the `.com` registry**, so no resolver ever asks DO for the zone. Most domains in this account are in
that state; check `dig +short NS <domain>` returns something before pointing a service at it.

Note `siftbitcoin.com` has a wildcard `A *` record, so `poker.siftbitcoin.com` resolved to the wrong
host until an explicit record was added. An explicit record takes precedence over the wildcard.

## Restore drill: performed

Run 2026-08-19 against a fork of the live database. The restored wallet reported its 100,000 sat
**and signed and broadcast** `9a55d640ee987d9d3c1e0576cd1f02f10798b75c4f01d277ba626573e8e98e61`,
which is the step that actually matters — reading a balance proves nothing about signing.

The fork was destroyed, `POKER_BACKUP_VERIFIED_AT=2026-08-19` recorded, and `POKER_REAL_VALUE_PLAY`
enabled. `/readyz` now reports `realValueReady: true`.

## Deploying a new version

```sh
# Build for the droplet. CGO is off, so this cross-compiles cleanly.
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags="-s -w -X main.version=$VERSION" -o dist/table-linux ./cmd/table

scp dist/table-linux root@167.99.239.236:/usr/local/bin/poker-table.new
ssh root@167.99.239.236 '
  mv /usr/local/bin/poker-table /usr/local/bin/poker-table.prev
  mv /usr/local/bin/poker-table.new /usr/local/bin/poker-table
  chmod 0755 /usr/local/bin/poker-table
  systemctl restart poker-table
  sleep 5 && systemctl is-active poker-table'
```

Rollback is `mv /usr/local/bin/poker-table.prev /usr/local/bin/poker-table && systemctl restart
poker-table`. **Never revert the database alongside it** — an older wallet snapshot can permanently
lose knowledge of coins created since.

## Before enabling real-value play

1. Run the restore drill in the runbook. Reading a balance from a restored wallet is not proof;
   spend from it.
2. Set `POKER_BACKUP_VERIFIED_AT` in `/etc/poker/table.env` to the date.
3. Set `POKER_REAL_VALUE_PLAY=true`.
4. `systemctl restart poker-table` and confirm `/readyz` reports `realValueReady: true`.

Until then the service serves play but refuses to put real value at risk, which is the intended
interlock.

## Real-value hand through independent agents

Run 2026-08-19 against the deployed service, with each seat signing through its **own** agent over
the substrate rather than one process holding both keys — the difference that makes the
non-custodial claim real:

```
agent 0 identity 02f327c0fd9bd2bb…
agent 1 identity 03ad23a40e43c21a…
pot funded: dd1be89101f1d672…:0 for 4000 sat
seat 0 sees: 2000 sat committed to a 4000 sat pot. If the hand stalls you can
             reclaim it from block 29870.
seat 0 signed through its own agent
seat 1 signed through its own agent
SETTLEMENT BROADCAST: d1939e8c9efd781f…
seat 1 sees: Hand settled, 3600 sat to you. Not spendable until the settlement is mined.
WINNER BALANCE: 100000 -> 103600 sat (delta +3600)
seat 1 now sees: Hand settled, 3600 sat received and spendable.
```

Each signature was produced by an agent that verified the settlement against its own record of the
hand and could have refused. The winner's payout was internalized through its own agent, so the
coin became spendable without the coordinating process ever holding that seat's key.

The deployed service was independently confirmed ready first: all four dependencies up, a valid
Let's Encrypt certificate for `poker.siftbitcoin.com`, and `realValueReady: true`.
