## Purpose

Moves the money for a hand — buy-in into a shared pot no one controls alone, payout to the
winner the hand actually produced, and a guaranteed way for every player to get their stake back
if the table stops cooperating.

## ADDED Requirements

### Requirement: Declared terms before commitment

The system SHALL present the buy-in amount, the pot arrangement, and the refund terms to a player
before that player commits any funds.

#### Scenario: Terms are shown before funding
- **WHEN** a player is asked to fund a buy-in
- **THEN** the amount, the pot's locking arrangement, and the refund availability time are shown

#### Scenario: Terms cannot change after commitment
- **WHEN** any player has funded a buy-in
- **THEN** the declared terms for that hand SHALL NOT change

### Requirement: Refund precedes funding

The system SHALL ensure a player holds a fully-signed refund of their own stake before that
stake is committed to the pot.

#### Scenario: No refund means no funding
- **WHEN** a player does not hold a signed refund of their stake
- **THEN** the system does not commit that player's funds to the pot

#### Scenario: The refund returns the stake to its owner
- **WHEN** a player's refund is broadcast after it becomes spendable
- **THEN** the player receives their stake back, less transaction fees

#### Scenario: The refund needs no counterparty
- **WHEN** a player broadcasts their refund
- **THEN** no other player's cooperation or signature is required

#### Scenario: A refund is not broadcast before it is valid
- **WHEN** a refund's spendable time has not yet been reached
- **THEN** the system does not broadcast it

### Requirement: Pot funding

The system SHALL collect each seat's buy-in into a pot output that requires every seat's
authorisation to spend, such that no single party — including the table service — can move it.

#### Scenario: The pot requires all seats to spend
- **WHEN** the pot is funded
- **THEN** spending it requires an authorisation from every seat

#### Scenario: The table service cannot move the pot
- **WHEN** the table service alone attempts to spend the pot
- **THEN** the attempt fails

#### Scenario: The pot total matches the buy-ins
- **WHEN** all seats have funded
- **THEN** the pot value equals the sum of the declared buy-ins

#### Scenario: Play does not begin on a partial pot
- **WHEN** at least one seat has not funded its buy-in
- **THEN** the hand does not begin
- **AND** seats that did fund retain their refund path

### Requirement: Settlement to the hand's winner

The system SHALL pay the pot to the player the hand determined won, in an amount consistent with
the game engine's pot resolution.

#### Scenario: The verified winner is paid
- **WHEN** a hand completes and the winner is established
- **THEN** the settlement pays that player

#### Scenario: Split results pay each entitled player
- **WHEN** the hand result divides the pot among multiple players
- **THEN** each entitled player is paid their share

#### Scenario: Value is conserved
- **WHEN** a settlement is constructed
- **THEN** the sum of its outputs plus fees equals the pot value

#### Scenario: A settlement inconsistent with the hand is not signed
- **WHEN** a proposed settlement disagrees with the hand's outcome
- **THEN** it is refused and not broadcast

### Requirement: Spendable payout

The system SHALL pay a winner in a form the winner's own wallet can subsequently spend.

#### Scenario: The winner can spend the payout
- **WHEN** a settlement is confirmed
- **THEN** the winner's wallet recognises the received coin
- **AND** can spend it in a later transaction

#### Scenario: A payout form that cannot be spent is not used
- **WHEN** constructing the payout
- **THEN** the system does not use a form that records no derivation material for the receiver

### Requirement: Recovery when cooperation fails

The system SHALL guarantee that every player can recover their stake when a hand cannot settle
cooperatively.

#### Scenario: An abandoned hand resolves to refunds
- **WHEN** a hand cannot complete because a seat stopped cooperating
- **THEN** each player recovers their stake through their refund once it is spendable

#### Scenario: No party can capture funds by refusing
- **WHEN** a seat refuses to authorise a correct settlement
- **THEN** that seat gains no more than its own refunded stake

#### Scenario: Refund and settlement cannot both succeed
- **WHEN** a settlement has been confirmed for a hand
- **THEN** the refunds for that hand can no longer be spent

### Requirement: Settlement durability

The system SHALL record its intent before signing or broadcasting a money-moving transaction, so
that an interruption cannot leave the pot in an unknown state.

#### Scenario: Intent is recorded before broadcast
- **WHEN** a money-moving transaction is about to be signed or broadcast
- **THEN** the intent is durably recorded first

#### Scenario: A crash is recoverable
- **WHEN** the service restarts after failing between signing and broadcast
- **THEN** it determines from its records what was attempted
- **AND** completes or abandons it without double-spending the pot

#### Scenario: Pot outputs remain locatable
- **WHEN** the service restarts at any point after the pot is funded
- **THEN** it can locate the pot output and its current state

### Requirement: Player-visible money state

The system SHALL report to each player the state of their own funds at each stage.

#### Scenario: A player sees their commitment state
- **WHEN** a player has funded a buy-in
- **THEN** they can see that their stake is committed and that a refund is held

#### Scenario: A player sees the settlement outcome
- **WHEN** a hand settles
- **THEN** each player is shown the outcome and the resulting change to their funds

#### Scenario: A stalled hand is reported honestly
- **WHEN** a hand cannot settle
- **THEN** players are told the hand is stalled, why, and when their refund becomes available
