## Purpose

Runs the table service as a deployed system on Digital Ocean, with the operational guarantees the
wallet requires — above all a backed-up database, because the database is the only record of the
coins the service controls.

## ADDED Requirements

### Requirement: Configuration validation at startup

The system SHALL validate its configuration before serving traffic, and SHALL refuse to start on
an invalid or unsafe configuration rather than degrade silently.

#### Scenario: Missing required configuration prevents startup
- **WHEN** a required setting is absent
- **THEN** the service exits with a message identifying the setting

#### Scenario: An unsafe production configuration prevents startup
- **WHEN** the service is configured for a non-local deployment without transport protection or
  without authenticated identity
- **THEN** it refuses to start

#### Scenario: Configuration is reported at startup
- **WHEN** the service starts successfully
- **THEN** it logs the network, storage backend, and chain service endpoints in use
- **AND** does not log secrets

### Requirement: Secret handling

The system SHALL keep private keys and database credentials out of source control, images, logs,
and error output.

#### Scenario: Secrets are supplied at runtime
- **WHEN** the service needs a key or credential
- **THEN** it reads it from runtime configuration, not from the built image

#### Scenario: Secrets never appear in output
- **WHEN** the service logs or returns an error
- **THEN** no private key or credential value appears

#### Scenario: Player keys are absent from the table service
- **WHEN** the table service is running
- **THEN** it holds no player private key

### Requirement: Wallet database durability

The system SHALL back up the wallet database automatically and SHALL verify that a restore
produces a working wallet, because coins cannot be rediscovered from keys alone.

#### Scenario: Backups run automatically
- **WHEN** the service is deployed
- **THEN** database backups are taken on a schedule without manual action

#### Scenario: A restore is verified before real value is at risk
- **WHEN** the deployment is prepared for real-value play
- **THEN** a restore from backup has been performed and verified to yield a working wallet
- **AND** real-value play is not enabled before that verification

#### Scenario: Backup failure is surfaced
- **WHEN** a scheduled backup fails
- **THEN** the failure is reported rather than passing silently

#### Scenario: Schema changes do not require restoring an old snapshot
- **WHEN** the service's schema changes
- **THEN** the change is additive and forward-only

### Requirement: Chain service dependency health

The system SHALL report the health of the external chain services it depends on, and SHALL make a
dependency outage visible rather than presenting it as a game fault.

#### Scenario: An unreachable chain service is reported
- **WHEN** the transaction oracle or header service is unreachable
- **THEN** the service reports itself degraded and identifies the dependency

#### Scenario: Play is not started when settlement is impossible
- **WHEN** the transaction oracle is unreachable
- **THEN** the service does not start a new real-value hand

#### Scenario: An in-progress hand is not silently abandoned
- **WHEN** a dependency becomes unreachable mid-hand
- **THEN** players are informed
- **AND** each player's recovery path remains available

### Requirement: Health and readiness reporting

The system SHALL expose its health, distinguishing a process that is running from one that is
able to serve play.

#### Scenario: Liveness reflects the process
- **WHEN** the process is running
- **THEN** the liveness check succeeds

#### Scenario: Readiness reflects the ability to serve play
- **WHEN** the database or status tracking is unavailable
- **THEN** the readiness check fails

#### Scenario: Status tracking is part of readiness
- **WHEN** transaction status tracking is not running
- **THEN** the service is not reported ready for real-value play

### Requirement: Observability of money movement

The system SHALL record every money-moving action and its outcome in a form sufficient to
reconstruct what happened to a hand's funds.

#### Scenario: Every broadcast is recorded with its outcome
- **WHEN** the service broadcasts a transaction
- **THEN** the transaction, its purpose, and its classified outcome are recorded

#### Scenario: A rejection is recorded with its reason
- **WHEN** a broadcast is rejected
- **THEN** the reason is recorded

#### Scenario: A hand's money history is reconstructable
- **WHEN** a hand's funds are investigated after the fact
- **THEN** the records identify the buy-ins, the pot, the settlement or refunds, and their statuses

### Requirement: Restricted network exposure

The system SHALL expose only the interfaces required by its clients, and SHALL keep interfaces
lacking cryptographic authentication off public networks.

#### Scenario: Unauthenticated interfaces are not publicly reachable
- **WHEN** the deployment is configured
- **THEN** any interface without cryptographic authentication is not reachable from the public
  network

#### Scenario: Only intended ports are exposed
- **WHEN** the service is deployed
- **THEN** only the ports its clients require are reachable

### Requirement: Reproducible deployment and rollback

The system SHALL deploy from a versioned artifact and SHALL be able to return to the previous
version without restoring the wallet database.

#### Scenario: Deployment uses a versioned artifact
- **WHEN** the service is deployed
- **THEN** it runs a tagged, reproducible build artifact

#### Scenario: Rollback does not touch the wallet database
- **WHEN** a deployment is rolled back
- **THEN** the previous artifact is restored
- **AND** the wallet database is not reverted

#### Scenario: Dependency versions are pinned
- **WHEN** the artifact is built
- **THEN** the wallet library and toolchain versions are pinned
