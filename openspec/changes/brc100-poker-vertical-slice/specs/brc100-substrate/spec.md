## Purpose

Carries BRC-100 wallet calls over the network so that a wallet holding a player's keys can
serve requests from a remote caller it does not trust, letting players sign for themselves
instead of surrendering their keys to the table.

## ADDED Requirements

### Requirement: Remote BRC-100 call transport

The system SHALL expose BRC-100 wallet methods over a network transport, such that a remote
caller can invoke a method and receive its result or a structured error, without the wallet's
private key material leaving the process that holds it.

#### Scenario: A remote call is served
- **WHEN** an authorised caller invokes an exposed BRC-100 method with valid arguments
- **THEN** the wallet executes it and returns the result
- **AND** no private key material appears in the request or the response

#### Scenario: Results match the in-process wallet
- **WHEN** the same method with the same arguments is invoked in-process and over the transport
- **THEN** both produce equivalent results

#### Scenario: An unknown method is rejected
- **WHEN** a caller invokes a method the wallet does not expose
- **THEN** the call is refused with a structured error identifying the cause
- **AND** no partial side effect occurs

#### Scenario: Malformed arguments are rejected
- **WHEN** a call arrives with arguments that fail validation
- **THEN** it is refused with a structured error before any wallet state changes

### Requirement: Mutual cryptographic authentication

The system SHALL require both parties to prove control of their identity key by cryptographic
means, and SHALL NOT accept an identity that is merely asserted in a request field or header.

#### Scenario: An asserted identity is rejected
- **WHEN** a caller claims an identity without producing a valid proof of key control
- **THEN** the call is refused
- **AND** no wallet operation is performed

#### Scenario: A caller authenticates by proving key control
- **WHEN** a caller proves control of its identity key
- **THEN** the call proceeds and is attributed to that identity

#### Scenario: The wallet authenticates to the caller
- **WHEN** a caller connects to a wallet endpoint
- **THEN** the wallet proves control of its own identity key
- **AND** the caller can detect a substituted or impersonated endpoint

#### Scenario: A replayed request is rejected
- **WHEN** a previously valid authenticated request is captured and re-sent
- **THEN** it is refused

#### Scenario: A tampered request is rejected
- **WHEN** any authenticated request is modified in transit
- **THEN** authentication fails and the call is refused

### Requirement: Confidential transport

The system SHALL protect requests and responses in transit against disclosure and
modification, and SHALL refuse to operate over an unprotected channel outside of local
development.

#### Scenario: Unprotected transport is refused
- **WHEN** the service is configured for a non-local deployment without transport protection
- **THEN** it refuses to start and reports the misconfiguration

#### Scenario: Private material stays confidential in transit
- **WHEN** a response carries material private to one seat
- **THEN** that material is not readable by a network observer

### Requirement: Least-privilege method exposure

The system SHALL expose only the BRC-100 methods a caller requires, and SHALL refuse calls to
methods outside the caller's granted set.

#### Scenario: A table cannot enumerate a player's wallet
- **WHEN** a table service attempts a method outside its granted set, such as listing all of a
  player's outputs or actions
- **THEN** the call is refused

#### Scenario: A player's own client retains full access
- **WHEN** a player's own client calls a method within its granted set
- **THEN** the call is permitted

#### Scenario: Grants are per-identity
- **WHEN** two callers with different identities invoke the same method
- **THEN** each is evaluated against its own granted set

### Requirement: Explicit signing consent

The system SHALL NOT sign on behalf of a player without that player's approval for the
specific transaction, and SHALL present what is being signed in terms the player can check.

#### Scenario: Signing requires approval
- **WHEN** a remote caller requests a signature
- **THEN** the wallet does not sign until the player approves that specific request

#### Scenario: The player sees the material terms
- **WHEN** a signing request is presented for approval
- **THEN** the amounts, destinations, and the outputs being committed to are shown

#### Scenario: A declined request signs nothing
- **WHEN** the player declines a signing request
- **THEN** no signature is produced and the caller receives a refusal

#### Scenario: An approval does not authorise a second signature
- **WHEN** a caller re-sends a signing request that was already approved and served
- **THEN** the wallet does not sign again without fresh approval

### Requirement: Network identifier translation

The system SHALL report BRC-100 network identifiers as values valid under BRC-100 at any
boundary it exposes, regardless of the internal names the underlying wallet library returns.

#### Scenario: Teratestnet is reported as a valid identifier
- **WHEN** a caller queries the network over the substrate while running on Teratestnet
- **THEN** the response is a valid BRC-100 network identifier
- **AND** it is not the underlying library's internal name

### Requirement: Availability and abuse resistance

The system SHALL bound the resources any single caller can consume, so that one caller cannot
deny service to others.

#### Scenario: A flooding caller is throttled
- **WHEN** one caller issues requests far faster than its allowance
- **THEN** its excess requests are refused or delayed
- **AND** other callers continue to be served

#### Scenario: Oversized requests are refused
- **WHEN** a request body exceeds the configured maximum
- **THEN** it is refused without being fully buffered
