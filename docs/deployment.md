# Deployment

The live teratestnet deployment, as provisioned.

## What exists

| Resource | Detail |
|---|---|
| Droplet | `poker-table`, `167.99.239.236`, nyc1, `s-1vcpu-2gb`, Ubuntu 24.04 |
| Database | `poker-db`, DO managed Postgres 16, nyc1, `db-s-1vcpu-1gb` |
| VPC | `a3417bc5-…` — the same nyc1 VPC as the other BSV services |
| DNS record | `poker.bsvcloudsolutions.com` A → `167.99.239.236` (in DO) |
| Service | `poker-table.service`, listening on `127.0.0.1:8080` |
| Proxy | Caddy, terminating TLS on 443 |
| Table wallet | `02aa7acd60b5ee06b4e473f2aa9550710356cc0ef1b018378e321f8f6499dcaa68`, funded 100,000 sat |

Approximately $27/month: $12 droplet + $15 database.

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
$ curl http://127.0.0.1:8080/readyz
{"ready":true,
 "realValueReady":false,
 "realValueBlockedBy":["real-value play is not enabled",
   "no database restore has been proved: an unrestorable wallet loses the coins, not just availability"],
 "dependencies":[
   {"name":"database","state":"up"},
   {"name":"headers","state":"up","detail":"tip at height 29705"},
   {"name":"oracle","state":"up"},
   {"name":"statusTracking","state":"up"}]}
```

The service connected to managed Postgres over the private VPC endpoint, reached the teratestnet
oracle and header service, started its monitor daemon, and resolved its wallet identity.

`realValueReady` is correctly **false**: real-value play is gated on a proved database restore, and
that drill has not been run. See the runbook.

## Outstanding: domain delegation

**`poker.bsvcloudsolutions.com` does not resolve, and this cannot be fixed from DO.**

The A record exists and is correct — querying DO's nameserver directly returns it:

```
$ dig +short poker.bsvcloudsolutions.com @ns1.digitalocean.com
167.99.239.236
```

But `bsvcloudsolutions.com` has **no nameservers at the `.com` registry**, so no resolver ever
asks DO for it. The same is true of most domains in this DO account; only `connorpmurray.com` is
delegated, and to Namecheap rather than DO.

Two consequences:

1. The service is reachable only by IP until this is resolved.
2. **Caddy cannot obtain a TLS certificate**, because ACME validation requires the name to resolve
   publicly. It is installed and configured correctly and will succeed on its own once delegation
   is in place.

**To fix**, at the registrar for `bsvcloudsolutions.com`, set the nameservers to:

```
ns1.digitalocean.com
ns2.digitalocean.com
ns3.digitalocean.com
```

Then Caddy will issue a certificate within a minute or two of the record propagating, with no
further action here.

**Alternatively**, use a delegated domain. `connorpmurray.com` resolves today, but its DNS is at
Namecheap, so the record would have to be added there rather than in DO.

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
