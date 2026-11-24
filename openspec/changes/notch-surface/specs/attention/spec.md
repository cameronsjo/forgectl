## ADDED Requirements

### Requirement: Row key
Every row of `attention list --json` SHALL carry `key`, 16 lowercase hex characters derived only from the row's kind and the fields fixed when its wait was created. A herdr or herdr-core read, a worker merge or a session merge MUST NOT change it.

#### Scenario: herdr flaps
- **WHEN** the same desk item is listed once with herdr unavailable and once with herdr naming its pane
- **THEN** both listings give the row the same `key`

#### Scenario: A reused desk name
- **WHEN** a new desk item takes the name of an earlier one after `done/` is emptied
- **THEN** the new item's row has a different `key`, because a desk key also covers the item's hash and `added_at`

### Requirement: One row per key
Two rows from one source with the same `key` SHALL be listed once, as the lower-cost row.

#### Scenario: A sweep names a worker twice
- **WHEN** `sweep.json` lists the same worker blocked on a prompt twice
- **THEN** `attention list` shows one row for it

### Requirement: Short key in the text view
The text view of `attention list` SHALL start each row with the first 6 characters of its `key`.

#### Scenario: Reading the text view
- **WHEN** the queue has one row
- **THEN** its line starts with that row's key's first 6 characters and a space

### Requirement: Surface tier
Every row SHALL carry `tier`: `raise` for a waiting desk item, a blocked worker and a stuck drain; `count` for a marker-clean PR and an `Action (Cameron)`; `hidden` for questions and other actions. A row that comes from a session record (kind `q` or `a`) MUST NOT be `raise`, whatever its `origin`.

#### Scenario: A worker's question
- **WHEN** a worker session publishes a question
- **THEN** its row's `tier` is `hidden`

### Requirement: Source detail
`sources` SHALL report `drain` and `core` (herdr-core) beside the existing sources. `core` SHALL be `absent` when no desk item names a terminal, because hc is asked only then. When herdr or herdr-core is unavailable, `herdrDetail` or `coreDetail` SHALL say why, cleaned and cut to 200 characters. When the sweep is `ok`, `sweepGeneratedAt` SHALL be when that sweep ran.

#### Scenario: hc is not running
- **WHEN** `hc ls --json` fails
- **THEN** `sources.core` is `unavailable`, `coreDetail` names the failure, and no row carries a `terminal`

### Requirement: Drain row
`attention list` SHALL add one `drain` row (cost 1, tier `raise`, fixed text) only when `drain.json` reads `running` with a fresh tick, a claude-slots check that cannot answer has held claude launches for 15 minutes, and the pid it names is still the drain that wrote it. It MUST NOT write, lock or create anything to decide this, and no text the drain wrote SHALL reach the row.

#### Scenario: A crashed drain
- **WHEN** `drain.json` shows a stuck hold but its pid is gone
- **THEN** no `drain` row is listed

### Requirement: Desk terminal
A desk row SHALL carry `terminal`, the herdr-app terminal that queued the item, only when the item recorded one, `hc ls --json` lists that terminal live, and the item recorded it under the same hc core `attention list` asks. The terminal MUST NOT enter the row's `key`.

#### Scenario: Another core
- **WHEN** an item recorded `t3` under one hc core and `attention list` asks a different core that also has a live `t3`
- **THEN** the row carries no `terminal`
