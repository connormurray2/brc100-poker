## Purpose

Defines how the system talks to a BRC-100 wallet on Teratestnet — how funds enter a wallet, how
broadcast outcomes are interpreted, and how transaction status is tracked — so that money
movement is never misreported as success when it was refused.

## ADDED Requirements

### Requirement: Network targeting

The system SHALL operate against Teratestnet, and SHALL fail to start rather than run against a
different network than the one it was configured for.

#### Scenario: Configured network is used consistently
- **WHEN** the system starts configured for Teratestnet
- **THEN** the wallet, storage, and chain services all target Teratestnet

#### Scenario: A network mismatch prevents startup
- **WHEN** any component's configured network disagrees with the others
- **THEN** the system refuses to start and reports the mismatch

#### Scenario: Network is never left to a default
- **WHEN** the system starts
- **THEN** the network is set explicitly for every component that accepts it
- **AND** no component relies on its library default

### Requirement: Funds entry

The system SHALL treat a wallet as having only the coins it created or explicitly took in, and
SHALL record an incoming external payment so that the receiving wallet can later spend it.

#### Scenario: An external payment is credited
- **WHEN** a player sends a payment to the wallet's receive key and the payment is presented
- **THEN** the coin is verified and recorded
- **AND** the balance increases by the payment amount

#### Scenario: A credited coin is spendable
- **WHEN** an external payment has been credited
- **THEN** the wallet can subsequently spend that coin

#### Scenario: An unverifiable payment is refused
- **WHEN** a presented payment fails proof verification
- **THEN** it is not recorded
- **AND** the balance is unchanged

#### Scenario: No recovery is assumed from keys alone
- **WHEN** a wallet is constructed from a key with no existing stored state
- **THEN** its balance is zero
- **AND** the system does not report or imply that prior coins can be rediscovered

### Requirement: Broadcast outcome classification

The system SHALL classify every broadcast outcome as accepted, rejected, backpressured, or
indeterminate, and SHALL NOT treat a rejection as success.

#### Scenario: A rejection is recognised as failure
- **WHEN** a broadcast is refused by the network's validator
- **THEN** the system records it as rejected
- **AND** it does so even if no transport-level error is reported

#### Scenario: A rejection is never retried
- **WHEN** a broadcast has been rejected
- **THEN** the system does not resubmit that transaction unchanged

#### Scenario: Backpressure is retried
- **WHEN** a broadcast is refused for capacity reasons and was never queued
- **THEN** the system retries after the indicated delay

#### Scenario: An indeterminate outcome is reconciled, not blindly retried
- **WHEN** a broadcast fails in a way that leaves the transaction's fate unknown
- **THEN** the system queries the transaction's status to determine what happened
- **AND** does not resubmit before establishing that

#### Scenario: Acceptance is not treated as settlement
- **WHEN** a broadcast is accepted
- **THEN** the system does not report the transaction as settled until a status confirms it

### Requirement: Status tracking

The system SHALL run the status-tracking process required for transactions to receive status
updates, and SHALL apply those updates without stalling or crashing the pipeline.

#### Scenario: Transactions receive status without manual intervention
- **WHEN** a transaction has been broadcast
- **THEN** its status is updated as the network's view of it changes

#### Scenario: The system refuses to run without status tracking
- **WHEN** status tracking is not enabled or fails to start
- **THEN** the system does not accept real-value play
- **AND** reports the condition

#### Scenario: Duplicate status updates are harmless
- **WHEN** the same status update is delivered more than once
- **THEN** the resulting state is the same as if it were delivered once

#### Scenario: Status handling does not block or crash
- **WHEN** status updates arrive
- **THEN** handling them does not block the delivery pipeline
- **AND** a failure while handling one update does not terminate the process

#### Scenario: Exactly one status stream is opened
- **WHEN** the system is running
- **THEN** it maintains a single status subscription

### Requirement: Spendability and settlement distinction

The system SHALL distinguish a coin that is spendable from a transaction that is settled, and
SHALL report each accurately to players.

#### Scenario: A seen transaction makes its coin spendable
- **WHEN** a transaction reaches a status indicating the network has seen it
- **THEN** its outputs may be treated as spendable

#### Scenario: Settlement requires proof against block headers
- **WHEN** a transaction is reported as settled
- **THEN** its inclusion has been verified against block headers

#### Scenario: A double-spend attempt is surfaced
- **WHEN** a transaction's status indicates a double-spend attempt
- **THEN** the affected hand is halted and the condition reported

### Requirement: Fee policy

The system SHALL apply a fee rate sufficient for the network's validator to accept its
transactions, and SHALL fail locally rather than broadcast an underpaying transaction.

#### Scenario: Underpayment fails locally
- **WHEN** a transaction would be broadcast with a fee below the accepted minimum
- **THEN** the attempt fails locally before broadcast

#### Scenario: Caller-supplied unlocking scripts are sized for fees
- **WHEN** a transaction includes an input whose unlocking script the wallet does not produce
- **THEN** that input's script length is declared for fee calculation
- **AND** the declaration does not under-state the real size

### Requirement: Boundary value translation

The system SHALL present network and wallet values at any external boundary in the form the
BRC-100 specification defines, regardless of internal representations.

#### Scenario: Internal network names are not exposed
- **WHEN** a client queries the network through any exposed interface
- **THEN** the value returned is valid per BRC-100
- **AND** internal library naming is not leaked

### Requirement: Identity source

The system SHALL establish player identity from keys exchanged in the game protocol, and SHALL
NOT rely on network identity-discovery services for player identity.

#### Scenario: Seat identity comes from the game protocol
- **WHEN** a player joins a table
- **THEN** their identity is the key they present and prove in the game protocol

#### Scenario: Identity discovery is not used for seating
- **WHEN** seating and authorising a player
- **THEN** the system does not depend on an external identity-discovery lookup
