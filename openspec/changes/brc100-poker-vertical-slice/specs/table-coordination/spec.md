## Purpose

Carries game messages between the players at a table and keeps their views of the hand in
agreement — table lifecycle, seat join and leave, message ordering and de-duplication, and the
handling of players who go slow or disappear mid-hand.

## ADDED Requirements

### Requirement: Transport abstraction

The system SHALL route all game messages through a transport interface that supports
subscribing to a table's messages and publishing a message to a table, so that the game
protocol runs unchanged over different underlying transports.

#### Scenario: The same hand plays over a different transport
- **WHEN** the game protocol is run over an in-memory transport and then over a network
  transport with the same action sequence
- **THEN** the resulting game state is identical in both cases
- **AND** no game logic differs between the two runs

#### Scenario: Subscription can be released
- **WHEN** a subscriber releases its subscription to a table
- **THEN** it receives no further messages for that table

### Requirement: Message de-duplication

The system SHALL carry a de-duplication key on every published message and SHALL deliver a
message with a repeated key to the application exactly once, while permitting re-publication
for the benefit of participants catching up.

#### Scenario: A redelivered message is applied once
- **WHEN** the same message is delivered twice with the same de-duplication key
- **THEN** the application applies it once
- **AND** game state reflects a single application

#### Scenario: Re-publication serves a catching-up peer
- **WHEN** a message is re-published so a reconnecting peer can receive it
- **THEN** peers that already applied it do not apply it again
- **AND** the reconnecting peer receives it

### Requirement: Table lifecycle

The system SHALL allow a table to be created with declared stakes and seat count, advertise it
as joinable, and close it when the hand and settlement have completed.

#### Scenario: A table advertises its terms
- **WHEN** a table is created
- **THEN** its stakes, buy-in amount, and seat count are visible to prospective players
  before they commit funds

#### Scenario: A full table refuses joins
- **WHEN** every seat is occupied and another player attempts to join
- **THEN** the join is refused

#### Scenario: Terms cannot change after funding
- **WHEN** any player has committed a buy-in
- **THEN** the table's stakes and buy-in amount SHALL NOT change

### Requirement: Seat join and identity

The system SHALL require each joining player to present a stable public identity, and SHALL
bind that identity to the seat for the duration of the hand.

#### Scenario: A seat is bound to one identity
- **WHEN** a player joins a seat with their identity
- **THEN** messages for that seat are only accepted from that identity

#### Scenario: A forged seat message is rejected
- **WHEN** a message claims to act for a seat but is not attributable to that seat's identity
- **THEN** the message is rejected and game state is unchanged

#### Scenario: The same identity cannot occupy two seats
- **WHEN** an identity already seated attempts to take a second seat at the same table
- **THEN** the request is refused

### Requirement: Ordered protocol progression

The system SHALL require the deal and betting protocol steps to occur in their defined order,
and SHALL reject a message that arrives for a step that is not current.

#### Scenario: A step out of sequence is rejected
- **WHEN** a message for a later protocol step arrives before the current step completes
- **THEN** the message is rejected or deferred
- **AND** it does not advance game state out of order

#### Scenario: All seats agree on the current step
- **WHEN** a protocol step completes
- **THEN** every seat advances to the same next step

### Requirement: Timeouts and unresponsive seats

The system SHALL bound the time a table waits for any required message from a seat, and SHALL
take a defined action when that bound is exceeded.

#### Scenario: A slow player's turn times out
- **WHEN** a player does not act within the action time limit
- **THEN** a default action is applied for them
- **AND** the default is check when facing no bet, and fold when facing a bet

#### Scenario: A stalled deal is attributable
- **WHEN** a seat fails to send a required deal or reveal message within the limit
- **THEN** the hand cannot proceed
- **AND** the responsible seat is identified to all other seats

#### Scenario: A stall does not strand funds
- **WHEN** a hand cannot complete because a seat stopped responding
- **THEN** the funds recovery path remains available to every player

### Requirement: Disconnect and reconnect

The system SHALL allow a player who loses connectivity to rejoin their seat and resynchronise
to the current hand state without restarting the hand.

#### Scenario: A reconnecting player resynchronises
- **WHEN** a player reconnects mid-hand and requests catch-up
- **THEN** they receive the messages needed to reach current state
- **AND** their reconstructed state matches the other seats

#### Scenario: Private material is not re-sent to the wrong seat
- **WHEN** a player reconnects and catches up
- **THEN** they receive no private material belonging to another seat
