## Purpose

Runs a hand of Texas Hold'em as a deterministic state machine — seats, blinds, betting
streets, legal actions, and pot award — so that every participant independently computes the
same game state and the same winner from the same sequence of actions.

## ADDED Requirements

### Requirement: Hand ranking

The system SHALL evaluate any five-card poker hand to a comparable rank, and SHALL select the
best five-card hand available from a player's hole cards combined with the community cards.

#### Scenario: Standard categories rank correctly
- **WHEN** two hands of different categories are compared
- **THEN** the higher category wins, ordered: high card, pair, two pair, trips, straight,
  flush, full house, quads, straight flush

#### Scenario: Best five of seven is selected
- **WHEN** a player holds two hole cards and five community cards are available
- **THEN** the evaluation returns the strongest five-card combination available

#### Scenario: Ties break on kickers
- **WHEN** two hands share the same category
- **THEN** the winner is determined by the ranked comparison of the relevant card values
- **AND** hands identical in all ranked values compare exactly equal

#### Scenario: Ace plays low in a five-high straight
- **WHEN** a hand contains an ace with a five, four, three, and two
- **THEN** it ranks as a five-high straight

#### Scenario: Ace plays high in a broadway straight
- **WHEN** a hand contains an ace with a king, queen, jack, and ten
- **THEN** it ranks as an ace-high straight
- **AND** it ranks above a king-high straight

### Requirement: Seat and button management

The system SHALL seat between two and six players, assign a dealer button, and derive blind
positions and action order from the button.

#### Scenario: A hand requires at least two seats
- **WHEN** fewer than two players are seated
- **THEN** no hand starts

#### Scenario: Table capacity is enforced
- **WHEN** a seventh player attempts to take a seat
- **THEN** the request is refused

#### Scenario: The button advances between hands
- **WHEN** a hand completes and another begins with the same seats
- **THEN** the button moves to the next occupied seat in order

#### Scenario: Heads-up blinds are positioned correctly
- **WHEN** exactly two players are seated
- **THEN** the button posts the small blind and acts first pre-flop
- **AND** the other seat posts the big blind and acts first on every later street

### Requirement: Betting rounds

The system SHALL run four betting streets — pre-flop, flop, turn, and river — revealing three,
one, and one community card respectively, and SHALL close a street only when every player who
is still in the hand has matched the current bet or folded.

#### Scenario: Streets progress in order
- **WHEN** a betting street closes and more than one player remains
- **THEN** the next street begins and its community cards are revealed

#### Scenario: A street stays open until bets are matched
- **WHEN** a player raises after others have already acted
- **THEN** action returns to those players
- **AND** the street does not close until all have matched the raise or folded

#### Scenario: The hand ends when only one player remains
- **WHEN** all players but one have folded
- **THEN** the hand ends immediately without revealing further community cards
- **AND** the remaining player is awarded the pot

### Requirement: Legal action enforcement

The system SHALL compute the set of legal actions for the player to act, and SHALL reject any
action outside that set or taken out of turn.

#### Scenario: Out-of-turn actions are rejected
- **WHEN** a player who is not to act submits an action
- **THEN** the action is rejected and game state is unchanged

#### Scenario: Checking a live bet is rejected
- **WHEN** a player attempts to check while facing an unmatched bet
- **THEN** the action is rejected
- **AND** the legal actions offered are fold, call, or raise

#### Scenario: An undersized raise is rejected
- **WHEN** a player attempts to raise by less than the minimum raise increment
- **THEN** the action is rejected, unless the raise commits their entire remaining stack

#### Scenario: A player cannot wager more than their stack
- **WHEN** a player attempts to commit more chips than they hold
- **THEN** the action is rejected

#### Scenario: An all-in below the call amount is permitted
- **WHEN** a player's entire remaining stack is less than the amount required to call
- **THEN** the player may commit their whole stack
- **AND** the shortfall does not require them to fold

### Requirement: Pot and side-pot resolution

The system SHALL track committed chips per player, form side pots when players are all-in for
differing amounts, and award each pot only among players eligible for it.

#### Scenario: A single winner takes the pot
- **WHEN** one player holds the strongest hand at showdown
- **THEN** that player receives the entire pot

#### Scenario: Tied hands split the pot
- **WHEN** two players hold exactly equal hands at showdown
- **THEN** the pot is divided between them
- **AND** any indivisible remainder is assigned by a deterministic rule all participants compute
  identically

#### Scenario: A short all-in creates a side pot
- **WHEN** a player is all-in for less than another player's bet
- **THEN** a main pot is formed up to the all-in amount and a side pot holds the excess
- **AND** the all-in player is eligible only for the main pot

#### Scenario: Chip conservation holds
- **WHEN** a hand completes and all pots are awarded
- **THEN** the total chips awarded equals the total chips committed by all players

### Requirement: Deterministic replay

The system SHALL derive game state solely from the ordered sequence of hand actions and dealt
cards, so that any participant replaying that sequence reaches an identical state.

#### Scenario: Independent participants agree on state
- **WHEN** two participants apply the same ordered action sequence to the same starting state
- **THEN** both compute identical pots, stacks, street, and player-to-act

#### Scenario: A disputed outcome is resolvable from the record
- **WHEN** participants disagree about a hand's winner
- **AND** the full action and reveal sequence is available
- **THEN** replaying it yields the authoritative outcome
