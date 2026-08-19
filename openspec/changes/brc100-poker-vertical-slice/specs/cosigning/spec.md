## Purpose

Collects a signature from every seat's own wallet on a transaction spending the shared pot, so
the pot can pay the winner without any party ever holding custody of it, and so no seat can be
tricked into signing something other than what the hand actually decided.

## ADDED Requirements

### Requirement: Partially-signed transaction distribution

The system SHALL distribute a proposed pot transaction to every seat required to sign it, in a
form that lets each seat reconstruct exactly what it is being asked to authorise.

#### Scenario: All required seats receive the proposal
- **WHEN** a pot transaction requires signatures from every seat
- **THEN** each required seat receives the proposal
- **AND** each can reconstruct the full transaction being signed

#### Scenario: Seats verify they received the same proposal
- **WHEN** the proposal is distributed to all seats
- **THEN** each seat can confirm the others were asked to sign the identical transaction

### Requirement: Independent pre-signature verification

Each seat SHALL verify a proposed transaction against its own record of the hand before
signing, and SHALL refuse to sign when the transaction does not match.

#### Scenario: A settlement matching the hand result is signed
- **WHEN** the proposal pays the pot to the seat that the verified showdown determined won
- **AND** the amounts and inputs match the seat's record of the hand
- **THEN** the seat signs

#### Scenario: A settlement paying the wrong winner is refused
- **WHEN** the proposal pays a seat other than the one the showdown determined won
- **THEN** the seat refuses to sign
- **AND** the refusal and its reason are reported to the other seats

#### Scenario: An altered amount is refused
- **WHEN** the proposal's output amounts do not match the pot and the hand result
- **THEN** the seat refuses to sign

#### Scenario: An unexpected output is refused
- **WHEN** the proposal contains an output the hand result does not account for
- **THEN** the seat refuses to sign

#### Scenario: An unexpected input is refused
- **WHEN** the proposal spends an input that is not the pot the seat funded
- **THEN** the seat refuses to sign

#### Scenario: Script validity is checked locally before signing
- **WHEN** a seat has assembled the signatures it holds for a proposal
- **THEN** it evaluates the resulting scripts locally
- **AND** it does not broadcast a transaction that fails local evaluation

### Requirement: Signature collection and assembly

The system SHALL collect signatures from the required seats and assemble them into a valid
unlocking script in the order the locking script requires.

#### Scenario: A complete signature set produces a valid transaction
- **WHEN** every required seat has signed
- **THEN** the signatures are assembled into an unlocking script that satisfies the pot output
- **AND** the assembled transaction passes local script evaluation

#### Scenario: Signature order is enforced
- **WHEN** signatures are collected in an arbitrary arrival order
- **THEN** they are assembled in the order the locking script requires

#### Scenario: An incomplete set is not broadcast
- **WHEN** at least one required signature is missing
- **THEN** the transaction is not broadcast

#### Scenario: An invalid signature is rejected and attributed
- **WHEN** a seat submits a signature that does not verify
- **THEN** it is rejected
- **AND** the submitting seat is identified

### Requirement: Commitment stability under signing

The system SHALL ensure that what a seat signs cannot change after it signs, including the set
and order of transaction outputs.

#### Scenario: Output order does not change after signing
- **WHEN** a pot transaction is constructed for signing
- **THEN** its output order is fixed and is not randomised
- **AND** signatures collected earlier remain valid

#### Scenario: Change output presence does not change after signing
- **WHEN** a pot transaction includes a change output
- **THEN** that output is present in the transaction that is signed and broadcast
- **AND** it is not silently dropped for being small

#### Scenario: A changed transaction invalidates collected signatures
- **WHEN** any signed field of a proposal is modified after signatures were collected
- **THEN** verification fails and the transaction is not broadcast

### Requirement: Refund pre-signing before funding

The system SHALL ensure every seat holds a fully-signed refund of its own stake before that
seat's funds enter the pot.

#### Scenario: Funding is blocked without a refund in hand
- **WHEN** a seat has not obtained its signed refund
- **THEN** that seat does not fund the pot

#### Scenario: A refund returns the stake
- **WHEN** a seat's refund becomes spendable and is broadcast
- **THEN** it returns that seat's stake, less fees, to that seat

#### Scenario: Refund finality is checked before broadcast
- **WHEN** a refund is about to be broadcast
- **THEN** it is checked to be final and spendable at the current height
- **AND** a non-final refund is not broadcast

### Requirement: Non-cooperation handling

The system SHALL define a bounded outcome when a seat will not sign, so that no seat's funds
are permanently trapped.

#### Scenario: A refusing seat cannot capture the pot
- **WHEN** a seat refuses to sign a correct settlement
- **THEN** the pot is not paid to that seat
- **AND** the remaining seats can recover their stakes through the refund path

#### Scenario: A disappearing seat does not trap funds
- **WHEN** a seat stops responding before settlement completes
- **THEN** every other seat retains a path to recover its stake

#### Scenario: Non-cooperation is attributable
- **WHEN** settlement fails because a seat did not sign
- **THEN** the responsible seat is identified to the others

### Requirement: Pot output bookkeeping

The system SHALL maintain its own record of pot outputs and of the state of each
sign-and-broadcast attempt, because the wallet does not track outputs it cannot sign for.

#### Scenario: Pot outputs are tracked by the application
- **WHEN** a pot output is created
- **THEN** the application records it and can later locate it for spending

#### Scenario: Pot funds are kept separate from fee funds
- **WHEN** pot outputs and fee-paying outputs both exist
- **THEN** they are held separately so a pot output is never selected to pay a fee

#### Scenario: A crash mid-settlement is recoverable
- **WHEN** the service restarts after a failure between signing and broadcast
- **THEN** it can determine from its own records whether the transaction was broadcast
- **AND** it does not double-spend or abandon the pot
