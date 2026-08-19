## Purpose

Deals cards among mutually distrusting players with no dealer and no trusted server, so that
each player learns exactly their own hole cards and nothing about anyone else's, and so that
any deal can be proven honest after the hand ends.

## ADDED Requirements

### Requirement: Public deck encoding

The system SHALL encode each card as a fixed, public, deterministic point on the secp256k1
curve, such that every participant independently derives an identical starting deck without
communication.

#### Scenario: Independent participants derive the same deck
- **WHEN** two participants independently construct the starting deck for a 52-card game
- **THEN** both produce the same ordered list of 52 encodings
- **AND** the encodings are derivable by any observer without secret material

#### Scenario: Distinct cards have distinct encodings
- **WHEN** the starting deck is constructed
- **THEN** all 52 encodings are distinct
- **AND** each encoding maps back to exactly one card identity

### Requirement: Commutative masking

The system SHALL mask cards using an operation where masks applied by different players
commute, so that masks may be removed in any order and the recovered card does not depend on
the order in which players acted.

#### Scenario: Masks strip in any order
- **WHEN** two players each apply a secret mask to the same card encoding
- **AND** the masks are then removed in the reverse order of application
- **THEN** the original card encoding is recovered

#### Scenario: Order of removal is irrelevant
- **WHEN** three players have each masked a card encoding
- **AND** the masks are removed in any permutation of the three
- **THEN** every permutation recovers the identical original encoding

#### Scenario: A partially unmasked card reveals nothing
- **WHEN** a participant removes every mask they know from a card that also carries a mask
  held by another player
- **THEN** the result matches no card in the public deck

### Requirement: Cooperative shuffle

The system SHALL shuffle the deck through sequential per-player contributions, each applying
a secret permutation and secret masking, such that the final order is unknown to every
participant unless all participants collude.

#### Scenario: No single player knows the order
- **WHEN** every seated player has contributed one shuffle step
- **THEN** no individual player can determine which position holds which card
- **AND** the deck retains exactly 52 distinct entries

#### Scenario: One honest shuffler suffices
- **WHEN** all players but one collude to fix the deck order
- **AND** the remaining player shuffles honestly
- **THEN** the colluding players cannot determine the final order

#### Scenario: A player who declines to shuffle cannot be skipped silently
- **WHEN** a seated player does not contribute a shuffle step
- **THEN** the deal SHALL NOT proceed
- **AND** the failure is attributable to that seat

### Requirement: Per-position independent keys

After shuffling, the system SHALL re-mask the deck so that each deck position is protected by
an independent per-position secret from every player, rather than by one global per-player
secret.

#### Scenario: Revealing one position does not expose another
- **WHEN** all players reveal their secrets for one deck position
- **AND** that position's card becomes known to everyone
- **THEN** no information about any other position's card is revealed

### Requirement: Private hole-card deal

The system SHALL deal a position privately to one recipient by having every other player
disclose only that position's secret to that recipient alone, so the recipient learns the
card and no one else does.

#### Scenario: Recipient identifies their card
- **WHEN** every other player sends the recipient their secret for the recipient's position
- **THEN** the recipient recovers a valid card identity from the public deck

#### Scenario: A non-recipient cannot identify the card
- **WHEN** a player who is not the recipient attempts to identify that position's card using
  every secret available to them
- **THEN** they cannot determine the card identity

#### Scenario: Colluding opponents cannot read a hole card
- **WHEN** every player except the recipient pools all of their secrets for the recipient's
  position
- **THEN** they still cannot determine the card, because the recipient's own secret is
  required

### Requirement: Public board deal

The system SHALL deal a community card by having all players disclose that position's secret
to everyone, so that every participant recovers the same card.

#### Scenario: All players agree on a board card
- **WHEN** all players disclose their secrets for a board position
- **THEN** every participant recovers the same card identity

#### Scenario: A withheld board secret blocks the reveal
- **WHEN** at least one player withholds their secret for a board position
- **THEN** no participant can determine the card
- **AND** the withholding seat is identifiable

### Requirement: Verifiable showdown

The system SHALL allow a player to prove, at showdown, which cards they held, such that all
other participants can verify the claim against the deck they collectively produced.

#### Scenario: An honest claim verifies
- **WHEN** a player discloses the secrets for their hole-card positions at showdown
- **THEN** every other participant recomputes the same card identities
- **AND** the claim is accepted

#### Scenario: A false claim is rejected
- **WHEN** a player claims to have held cards other than those the disclosed secrets produce
- **THEN** verification fails
- **AND** the claim is attributable to that seat

#### Scenario: A player cannot substitute cards mid-hand
- **WHEN** a player attempts to claim a card from a deck position that was not dealt to them
- **THEN** verification fails

### Requirement: Secret quality and hygiene

The system SHALL generate every masking secret from a cryptographically secure random source,
reject unusable values, and never transmit a secret for a position that has not yet been
authorised for reveal.

#### Scenario: Unusable secrets are rejected
- **WHEN** secret generation produces a value that is not valid for masking
- **THEN** the value is discarded and generation retried
- **AND** no invalid secret is ever used in a deal

#### Scenario: Premature reveal is refused
- **WHEN** a participant requests a position's secret before that position is authorised for
  reveal
- **THEN** the request is refused
- **AND** the refusal is recorded
